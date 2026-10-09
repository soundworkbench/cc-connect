package feishu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func forwardedDSLCard(t *testing.T, text string) string {
	t.Helper()
	card, err := json.Marshal(map[string]any{"schema": "2.0", "body": map[string]any{"elements": []any{map[string]any{"tag": "markdown", "content": text}}}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"title": nil, "elements": [][]any{{map[string]any{"tag": "text", "text": "请升级至最新版本客户端，以查看内容"}}}, "user_dsl": string(card)})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestForwardedInteractiveCard_UserDSLPreservesBody(t *testing.T) {
	want := "80 家、4 批；目前实际 5 路，尚未达到 10 路。"
	if got := extractInteractiveCardText(forwardedDSLCard(t, want)); got != want {
		t.Fatalf("got %q, want forwarded card body %q", got, want)
	}
}

func TestForwardedInteractiveCard_DispatchesToAgent(t *testing.T) {
	p := &Platform{platformName: "feishu", botOpenID: "ou_bot"}
	p.userNameCache.Store("ou_sender", "Sender")
	p.chatNameCache.Store("oc_chat", "Chat")
	var got *core.Message
	p.handler = func(_ core.Platform, msg *core.Message) { got = msg }
	p.dispatchMessage(context.Background(), "interactive", forwardedDSLCard(t, "@_user_1 forwarded body"), []*larkim.MentionEvent{{Key: strPtr("@_user_1"), Id: &larkim.UserId{OpenId: strPtr("ou_bot")}}}, "om_forward", "feishu:oc_chat:ou_sender", "ou_sender", "oc_chat", replyContext{messageID: "om_forward", chatID: "oc_chat"}, "", 123)
	if got == nil {
		t.Fatal("forwarded interactive card was silently dropped")
	}
	if got.Content != "forwarded body" || got.MessageID != "om_forward" || got.UserMessageTimeMs != 123 {
		t.Fatalf("unexpected dispatched message: %#v", got)
	}
}

func TestForwardedInteractiveCard_InvalidDSLRetainsFallback(t *testing.T) {
	raw := `{"user_dsl":"invalid-json","elements":[[{"tag":"text","text":"fallback"}]]}`
	if got := extractInteractiveCardText(raw); got != "fallback" {
		t.Fatalf("got %q", got)
	}
	if got := extractInteractiveCardText("broken-json"); !strings.Contains(got, "card") {
		t.Fatalf("missing card placeholder: %q", got)
	}
}
