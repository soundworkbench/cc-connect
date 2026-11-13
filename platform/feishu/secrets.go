package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/chenhg5/cc-connect/core"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Secret cards on Feishu: the card goes only to a private chat, only its
// owner's submit counts, and every request to Feishu has saved secret values
// masked before it is sent.

// cardOwnerKey carries a card's owner in its callback value (core.CardForm.Owner).
const cardOwnerKey = "owner"

// chatKind is what a received message tells about its chat.
type chatKind struct {
	chatType string // p2p, group, topic_group (message event) or p2p, group, topic (chat info)
	user     string // the person, for a private chat
}

// noteChat remembers a chat's type, and the person of a private chat, from a
// received message.
func (p *Platform) noteChat(chatID, chatType, userID string) {
	if chatID == "" || chatType == "" {
		return
	}
	k := chatKind{chatType: chatType}
	if chatType == "p2p" {
		k.user = userID
	}
	p.chatKinds.Store(chatID, k)
}

// PrivateChatUser implements core.PrivateChatResolver: it returns the person a
// private-chat session belongs to, or core.ErrNotPrivateChat for a group or
// topic group. A chat seen in no message since start is looked up.
func (p *Platform) PrivateChatUser(ctx context.Context, sessionKey string) (string, error) {
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) < 2 || parts[0] != p.tag() || parts[1] == "" {
		return "", fmt.Errorf("%s: not a session of this platform: %q", p.tag(), sessionKey)
	}
	chatID := parts[1]
	user := ""
	if len(parts) == 3 && !isThreadSessionKey(sessionKey) {
		user = parts[2]
	}
	var k chatKind
	if v, ok := p.chatKinds.Load(chatID); ok {
		k = v.(chatKind)
	} else {
		mode, err := p.lookupChatMode(ctx, chatID)
		if err != nil {
			return "", err
		}
		k = chatKind{chatType: mode}
		p.chatKinds.Store(chatID, k)
	}
	if k.chatType != "p2p" {
		return "", core.ErrNotPrivateChat
	}
	if user == "" {
		user = k.user
	}
	if user == "" {
		return "", errors.New("the person of this private chat is not known yet; send the card after their next message")
	}
	return user, nil
}

// lookupChatMode asks Feishu whether a chat is a private chat, a group or a
// topic group.
func (p *Platform) lookupChatMode(ctx context.Context, chatID string) (string, error) {
	if p.client == nil {
		return "", fmt.Errorf("%s: client not initialized", p.tag())
	}
	ctx, cancel := context.WithTimeout(ctx, nameLookupTimeout)
	defer cancel()
	resp, err := p.client.Im.Chat.Get(ctx, larkim.NewGetChatReqBuilder().ChatId(chatID).Build())
	if err != nil {
		return "", fmt.Errorf("%s: chat info: %w", p.tag(), err)
	}
	if !resp.Success() || resp.Data == nil || resp.Data.ChatMode == nil {
		return "", fmt.Errorf("%s: chat info: code %d", p.tag(), resp.Code)
	}
	return *resp.Data.ChatMode, nil
}

// SetSecretStore implements core.SecretMasker.
func (p *Platform) SetSecretStore(s *core.SecretStore) { p.secrets.Store(s) }

// maskingHTTPClient sends Feishu API requests with saved secret values
// masked in their JSON bodies.
type maskingHTTPClient struct {
	secrets *atomic.Pointer[core.SecretStore]
	base    larkcore.HttpClient // nil: http.DefaultClient, as the SDK uses
}

func (c *maskingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	base := c.base
	if base == nil {
		base = http.DefaultClient
	}
	s := c.secrets.Load()
	if s == nil || req.Body == nil || req.Body == http.NoBody ||
		!strings.Contains(req.Header.Get("Content-Type"), "json") ||
		strings.Contains(req.URL.Path, "/open-apis/auth/") || // the app's own credentials
		!s.HasSecrets() {
		return base.Do(req)
	}
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("feishu: read request body: %w", err)
	}
	body, _ = s.RedactJSON(body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return base.Do(req)
}

// redactCallbackResponse masks saved secret values in the card or toast that
// answers a click; it does not go through the API client.
func (p *Platform) redactCallbackResponse(resp *callback.CardActionTriggerResponse) *callback.CardActionTriggerResponse {
	s := p.secrets.Load()
	if resp == nil || s == nil || !s.HasSecrets() {
		return resp
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return resp
	}
	out, changed := s.RedactJSON(raw)
	if !changed {
		return resp
	}
	var masked callback.CardActionTriggerResponse
	if json.Unmarshal(out, &masked) != nil {
		return resp
	}
	return &masked
}
