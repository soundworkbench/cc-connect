package feishu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	callback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// Test values only: none of these is a real credential.
const (
	fakeSecretToken = "pat-na1-FAKE0000-1111-2222-3333-444455556666"
	fakeSecretPass  = `Fa<k>e"pa&ss\word`
)

func TestSecretFormRendersAPasswordBoxForItsOwner(t *testing.T) {
	form := core.CardForm{
		Name:        "secret_form",
		Fields:      []core.CardFormField{{Name: "secret", Kind: "input", Placeholder: "粘贴到这里", Required: true, Password: true, MaxLength: 1000}},
		SubmitText:  "保存",
		SubmitType:  "primary",
		SubmitValue: "act:/secret-submit hubspot-token",
		Owner:       "ou_owner",
	}
	js := plainJSON(renderFormV2(form, "feishu:oc_dm:ou_owner"))
	for _, want := range []string{`"input_type":"password"`, `"max_length":1000`, `"owner":"ou_owner"`,
		`"action":"act:/secret-submit hubspot-token"`, `"form_action_type":"submit"`} {
		if !strings.Contains(js, want) {
			t.Fatalf("rendered form lacks %s:\n%s", want, js)
		}
	}
	plain := plainJSON(renderFormV2(core.CardForm{Name: "f", Fields: []core.CardFormField{{Name: "f1", Kind: "input"}}, SubmitText: "提交"}, ""))
	if strings.Contains(plain, "input_type") || strings.Contains(plain, "max_length") || strings.Contains(plain, `"owner"`) {
		t.Fatalf("an ordinary form changed: %s", plain)
	}
}

func TestSecretSubmitCountsOnlyForTheOwner(t *testing.T) {
	ip, _ := newAllowFromTestPlatform(t, "ou_owner,ou_other")
	var calls []string
	ip.cardNavHandler = func(action, _ string) *core.Card {
		calls = append(calls, action)
		return core.NewCard().Title("✅ 已保存：hubspot-token", "green").Build()
	}
	// A topic session has no owner in its key; the card names it instead.
	value := `{"action": "act:/secret-submit hubspot-token", "session_key": "feishu:oc_dm:root:om_root", "owner": "ou_owner", "card_schema": "2"}`
	ev := formSubmitCallback(t, value, "secret_form_submit", `{"secret": "FAKE-typed-by-someone-else"}`)
	ev.Event.Operator.OpenID = "ou_other"
	resp, err := ip.onCardAction(ev)
	if err != nil || resp == nil || resp.Toast == nil || !strings.Contains(resp.Toast.Content, "不是给你") && !strings.Contains(resp.Toast.Content, "是给") {
		t.Fatalf("someone else's submit: resp=%+v err=%v", resp, err)
	}
	if len(calls) != 0 {
		t.Fatalf("someone else's submit reached the handler: %v", calls)
	}

	ev = formSubmitCallback(t, value, "secret_form_submit", `{"secret": "FAKE-typed-by-owner"}`)
	ev.Event.Operator.OpenID = "ou_owner"
	resp, err = ip.onCardAction(ev)
	if err != nil || resp == nil || resp.Card == nil || len(calls) != 1 || !strings.HasPrefix(calls[0], "act:/secret-submit hubspot-token {") {
		t.Fatalf("owner's submit: resp=%+v err=%v calls=%v", resp, err, calls)
	}
}

func TestPrivateChatUser(t *testing.T) {
	srv := chatInfoServer(t, map[string]string{"oc_looked_up_dm": "p2p", "oc_looked_up_group": "group"})
	pAny, err := New(map[string]any{"app_id": "cli_xxx", "app_secret": "secret", "domain": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	p := platformOf(pAny)
	p.noteChat("oc_dm", "p2p", "ou_owner")
	p.noteChat("oc_group", "group", "ou_owner")
	p.noteChat("oc_topic", "topic_group", "ou_owner")

	for key, want := range map[string]string{
		"feishu:oc_dm:ou_owner":           "ou_owner",
		"feishu:oc_dm:root:om_1":          "ou_owner", // a topic in the private chat
		"feishu:oc_dm":                    "ou_owner", // shared session
		"feishu:oc_looked_up_dm:ou_owner": "ou_owner",
	} {
		if got, err := p.PrivateChatUser(context.Background(), key); err != nil || got != want {
			t.Errorf("%s: got %q, %v", key, got, err)
		}
	}
	for _, key := range []string{"feishu:oc_group:ou_owner", "feishu:oc_topic:root:om_1", "feishu:oc_looked_up_group:ou_owner"} {
		if _, err := p.PrivateChatUser(context.Background(), key); !errors.Is(err, core.ErrNotPrivateChat) {
			t.Errorf("%s: err = %v, want ErrNotPrivateChat", key, err)
		}
	}
	for _, key := range []string{"feishu:oc_unknown:ou_owner", "slack:oc_dm:ou_owner"} {
		if _, err := p.PrivateChatUser(context.Background(), key); err == nil || errors.Is(err, core.ErrNotPrivateChat) {
			t.Errorf("%s: err = %v, want a lookup error", key, err)
		}
	}
	// A topic of a private chat seen in no message: chat known, person not.
	if _, err := p.PrivateChatUser(context.Background(), "feishu:oc_looked_up_dm:root:om_9"); err == nil {
		t.Error("a private chat without a known person was accepted")
	}
}

func TestRequestsToFeishuMaskSavedSecrets(t *testing.T) {
	srv := chatInfoServer(t, nil)
	pAny, err := New(map[string]any{"app_id": "cli_xxx", "app_secret": "secret", "domain": srv.URL, "enable_feishu_card": true})
	if err != nil {
		t.Fatal(err)
	}
	p := platformOf(pAny)
	store := core.NewSecretStore(filepath.Join(t.TempDir(), ".secrets"))
	for name, v := range map[string]string{"hubspot-token": fakeSecretToken, "site-pass": fakeSecretPass} {
		if _, err := store.Save(name, v); err != nil {
			t.Fatal(err)
		}
	}
	p.SetSecretStore(store)

	ctx := context.Background()
	rc := replyContext{chatID: "oc_dm", sessionKey: "feishu:oc_dm:ou_owner"}
	if err := p.Send(ctx, rc, "令牌 "+fakeSecretToken+"，密码 "+fakeSecretPass); err != nil {
		t.Fatal(err)
	}
	card := core.NewCard().Title("出错了", "red").Markdown("HTTP 401, Authorization: Bearer " + fakeSecretToken).Build()
	sender, ok := pAny.(core.CardSender)
	if !ok {
		t.Fatalf("%T sends no cards", pAny)
	}
	if err := sender.SendCard(ctx, rc, card); err != nil {
		t.Fatal(err)
	}
	bodies := srv.bodies()
	if len(bodies) < 2 {
		t.Fatalf("requests = %d", len(bodies))
	}
	all := strings.Join(bodies, "\n")
	for _, leak := range []string{"FAKE0000", "pa&ss", `pa\u0026ss`, `pa\\u0026ss`} {
		if strings.Contains(all, leak) {
			t.Fatalf("%q reached Feishu:\n%s", leak, all)
		}
	}
	if !strings.Contains(all, "‹密钥:hubspot-token›") || !strings.Contains(all, "‹密钥:site-pass›") {
		t.Fatalf("no placeholder sent:\n%s", all)
	}
}

func TestCallbackAnswerMasksSavedSecrets(t *testing.T) {
	ip, _ := newAllowFromTestPlatform(t, "*")
	store := core.NewSecretStore(filepath.Join(t.TempDir(), ".secrets"))
	if _, err := store.Save("hubspot-token", fakeSecretToken); err != nil {
		t.Fatal(err)
	}
	resp := &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "info", Content: "token " + fakeSecretToken}}
	if got := ip.redactCallbackResponse(resp); got != resp {
		t.Fatal("changed without a store")
	}
	ip.SetSecretStore(store)
	got := ip.redactCallbackResponse(resp)
	if got.Toast == nil || got.Toast.Content != "token ‹密钥:hubspot-token›" {
		t.Fatalf("toast = %+v", got.Toast)
	}
}

type recordingServer struct {
	*httptest.Server
	mu  sync.Mutex
	got []string
}

func (s *recordingServer) bodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

// chatInfoServer is a stub Feishu API: it answers chat info with the given
// chat modes and records every request body it receives.
func chatInfoServer(t *testing.T, modes map[string]string) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "expire": 7200, "tenant_access_token": "t-test"})
			return
		}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			rs.mu.Lock()
			rs.got = append(rs.got, string(b))
			rs.mu.Unlock()
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/open-apis/im/v1/chats/"); ok {
			if mode, ok := modes[id]; ok {
				writeJSON(t, w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"chat_mode": mode}})
				return
			}
			writeJSON(t, w, map[string]any{"code": 232011, "msg": "chat not found"})
			return
		}
		writeJSON(t, w, map[string]any{"code": 0, "msg": "success", "data": map[string]any{"message_id": "om_sent"}})
	}))
	t.Cleanup(rs.Close)
	return rs
}
