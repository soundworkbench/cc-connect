package feishu

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	callback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// formSubmitCallback is a card.action.trigger body in the shape Feishu
// documents for a submit button inside a form container: the button's value
// and name, plus every field in form_value keyed by its name. A multi-select
// is an array, a single select a string, a date "YYYY-MM-DD +0800".
func formSubmitCallback(t *testing.T, value, name, formValue string) *callback.CardActionTriggerEvent {
	t.Helper()
	body := `{
  "schema": "2.0",
  "header": {"event_id": "ev_1", "event_type": "card.action.trigger", "create_time": "1762905600000", "app_id": "cli_xxx", "tenant_key": "tk"},
  "event": {
    "operator": {"tenant_key": "tk", "open_id": "ou_allowed"},
    "token": "c-token",
    "action": {"value": ` + value + `, "tag": "button", "name": "` + name + `", "form_value": ` + formValue + `},
    "host": "im_message",
    "context": {"open_message_id": "om_card", "open_chat_id": "oc_test_chat"}
  }
}`
	var ev callback.CardActionTriggerEvent
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return &ev
}

type navCall struct{ action, sessionKey string }

func submitForm(t *testing.T, ev *callback.CardActionTriggerEvent, reply *core.Card) (navCall, *callback.CardActionTriggerResponse) {
	t.Helper()
	ip, _ := newAllowFromTestPlatform(t, "ou_allowed")
	got := make(chan navCall, 1)
	ip.cardNavHandler = func(action, sessionKey string) *core.Card {
		got <- navCall{action, sessionKey}
		return reply
	}
	resp, err := ip.onCardAction(ev)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-got:
		return c, resp
	default:
		t.Fatal("the submit never reached the card handler")
		return navCall{}, nil
	}
}

func TestDecisionFormSubmitsFromFeishuCallbacks(t *testing.T) {
	decided := core.NewCard().Title("已决定：周报包含哪些部分？", "green").Build()
	for name, tc := range map[string]struct{ id, formValue, want string }{
		"multi-select": {"d7", `{"choice": ["1", "3"]}`, `{"choice":["1","3"]}`},
		"dropdown":     {"d8", `{"choice": "7"}`, `{"choice":"7"}`},
	} {
		t.Run(name, func(t *testing.T) {
			value := `{"action": "act:/decision-form ` + tc.id + `", "session_key": "feishu:oc_test_chat:ou_allowed"}`
			call, resp := submitForm(t, formSubmitCallback(t, value, "decision_"+tc.id+"_submit", tc.formValue), decided)
			if call.action != "act:/decision-form "+tc.id+" "+tc.want || call.sessionKey != "feishu:oc_test_chat:ou_allowed" {
				t.Fatalf("call=%+v", call)
			}
			if resp == nil || resp.Card == nil || resp.Card.Type != "raw" || !strings.Contains(plainJSON(resp.Card.Data), "已决定") {
				t.Fatalf("the card was not replaced: %+v", resp)
			}
		})
	}
}

func TestSendFormSubmitsFromAFeishuCallback(t *testing.T) {
	value := `{"action": "act:/form-submit f1", "session_key": "feishu:oc_test_chat:ou_allowed"}`
	formValue := `{"f1": "华东区", "f2": "2026-11-20 +0800", "f3": "2", "f4": ["1", "3"]}`
	submitted := core.NewCard().Title("✅ 已提交：出差申请", "green").Build()
	call, resp := submitForm(t, formSubmitCallback(t, value, "form_f1_submit", formValue), submitted)
	action, raw, _ := strings.Cut(call.action, " {")
	if action != "act:/form-submit f1" {
		t.Fatalf("action=%q", call.action)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte("{"+raw), &got); err != nil {
		t.Fatalf("values %q: %v", raw, err)
	}
	if got["f1"] != "华东区" || got["f2"] != "2026-11-20 +0800" || got["f3"] != "2" || len(got["f4"].([]any)) != 2 {
		t.Fatalf("values=%v", got)
	}
	if resp == nil || resp.Card == nil || !strings.Contains(plainJSON(resp.Card.Data), "已提交") {
		t.Fatalf("the card was not replaced: %+v", resp)
	}
}

func captureInfo(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

func TestDroppedCardCallbacksAreLoggedWithAReason(t *testing.T) {
	for name, tc := range map[string]struct {
		user, value, reason string
		nav                 bool
	}{
		"no action value":     {"ou_allowed", `{}`, "reason=no_action", false},
		"handler had no card": {"ou_allowed", `{"action": "act:/decision-form gone"}`, "reason=no_card_update", true},
		"unknown prefix":      {"ou_allowed", `{"action": "zzz:/what"}`, "reason=unknown_action", false},
		"user not allowed":    {"ou_stranger", `{"action": "act:/decision-form d1"}`, "reason=user_not_allowed", false},
	} {
		t.Run(name, func(t *testing.T) {
			ip, _ := newAllowFromTestPlatform(t, "ou_allowed")
			if tc.nav {
				ip.cardNavHandler = func(string, string) *core.Card { return nil }
			}
			ev := formSubmitCallback(t, tc.value, "decision_d1_submit", `{"choice": ["1"], "note": "私人内容"}`)
			ev.Event.Operator.OpenID = tc.user
			logs := captureInfo(t)
			if _, err := ip.onCardAction(ev); err != nil {
				t.Fatal(err)
			}
			out := logs.String()
			if !strings.Contains(out, "level=INFO") || !strings.Contains(out, `msg="card: callback_dropped"`) || !strings.Contains(out, tc.reason) {
				t.Fatalf("log=%s", out)
			}
			if strings.Contains(out, "私人内容") {
				t.Fatalf("form values were logged: %s", out)
			}
		})
	}
}

// A form submit that is answered late is refreshed in the background; when
// that fails, the log names the action and the fields but not what was typed.
func TestLateFormRefreshFailureLogsNoFormValues(t *testing.T) {
	logs := captureInfo(t)
	ip, _ := newAllowFromTestPlatform(t, "ou_allowed")
	done := make(chan struct{})
	ip.cardNavHandler = func(_, sessionKey string) *core.Card {
		time.Sleep(cardNavTimeout + 100*time.Millisecond)
		ip.cardActionMsgMu.Lock()
		delete(ip.cardActionMsgIDs, sessionKey) // the refresh then has no card to update
		ip.cardActionMsgMu.Unlock()
		defer close(done)
		return core.NewCard().Title("✅ 已提交：出差申请", "green").Build()
	}
	value := `{"action": "act:/form-submit f1", "session_key": "feishu:oc_test_chat:ou_allowed"}`
	resp, err := ip.onCardAction(formSubmitCallback(t, value, "form_f1_submit", `{"f1": "私密内容-华东区", "f2": "口令 hunter2"}`))
	if err != nil || resp == nil || resp.Toast == nil {
		t.Fatalf("late answer: resp=%+v err=%v", resp, err)
	}
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "async card refresh failed") {
		if time.Now().After(deadline) {
			t.Fatalf("no refresh failure logged: %s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	out := logs.String()
	if strings.Contains(out, "私密内容") || strings.Contains(out, "hunter2") {
		t.Fatalf("form values reached the log: %s", out)
	}
	if !strings.Contains(out, "act:/form-submit f1 fields=f1,f2") {
		t.Fatalf("log lacks the action and field names: %s", out)
	}
}
