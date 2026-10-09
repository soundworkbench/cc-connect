package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type finalOrderSession struct {
	smokeSession
	sent chan string
}

func (s *finalOrderSession) Send(prompt, _ string, _ []core.ImageAttachment, _ []core.FileAttachment) error {
	s.sent <- prompt
	return nil
}

type finalOrderAgent struct {
	smokeAgent
	session *finalOrderSession
}

func (a *finalOrderAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	return a.session, nil
}

// The real engine and Feishu SDK talk only to the local fixture API. Failures
// are permanent API rejections, so adapter transport retries cannot mask them.
func TestPreviewLifecycleFinalHTTPOrder(t *testing.T) {
	for _, order := range []core.FinalReplyOrder{core.FinalReplySendFirst, core.FinalReplyRecallFirst} {
		for _, scenario := range []string{"normal", "multipart", "retry", "send_failed", "recall_failed", "queued", "silent", "error", "closed", "stop", "cancel"} {
			t.Run(string(order)+"/"+scenario, func(t *testing.T) {
				var mu sync.Mutex
				var calls []string
				cards := 0
				sends := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if strings.Contains(r.URL.Path, "tenant_access_token") {
						fmt.Fprint(w, `{"code":0,"expire":7200,"tenant_access_token":"fixture"}`)
						return
					}
					mu.Lock()
					defer mu.Unlock()
					switch r.Method {
					case http.MethodDelete:
						calls = append(calls, "delete")
						if scenario == "recall_failed" {
							fmt.Fprint(w, `{"code":230006,"msg":"recall rejected"}`)
							return
						}
					case http.MethodPatch:
						calls = append(calls, "update")
					case http.MethodPost:
						var body struct {
							MsgType string `json:"msg_type"`
							Content string `json:"content"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if body.MsgType == "interactive" && strings.Contains(body.Content, "思考中") {
							cards++
							calls = append(calls, "card")
						} else {
							if body.MsgType == "text" && order == core.FinalReplyRecallFirst && scenario == "send_failed" {
								var plain struct {
									Text string `json:"text"`
								}
								if err := json.Unmarshal([]byte(body.Content), &plain); err != nil || plain.Text != "answer" {
									t.Errorf("fallback lost answer: %q (%v)", body.Content, err)
								}
								calls = append(calls, "plain")
							} else {
								calls = append(calls, "send")
							}
							sends++
							if (scenario == "retry" && sends == 1) || (scenario == "send_failed" && sends <= 2) {
								fmt.Fprint(w, `{"code":230006,"msg":"send rejected"}`)
								return
							}
						}
					default:
						t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					}
					fmt.Fprintf(w, `{"code":0,"data":{"message_id":"om_card_%d"}}`, cards)
				}))
				defer srv.Close()
				p := &Platform{client: lark.NewClient("final-order-"+scenario, "fixture", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())), platformName: "feishu", useInteractiveCard: true, progressStyle: "compact", finalReplyOrder: order}
				s := &finalOrderSession{smokeSession: smokeSession{events: make(chan core.Event, 8)}, sent: make(chan string, 3)}
				e := core.NewEngine("order", &finalOrderAgent{session: s}, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
				e.SetStreamPreviewCfg(core.DefaultStreamPreviewCfg())
				e.SetDisplayConfig(core.DisplayCfg{Mode: "quiet", HideAgentFooter: true})
				defer func() { _ = e.Stop() }()
				read := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), calls...) }
				wait := func(reason string, fn func() bool) {
					t.Helper()
					deadline := time.Now().Add(4 * time.Second)
					for !fn() {
						if time.Now().After(deadline) {
							t.Fatalf("%s: %v", reason, read())
						}
						time.Sleep(time.Millisecond)
					}
				}
				key := "feishu:oc:ou"
				rc := replyContext{chatID: "oc", sessionKey: key}
				receive := func(id, content string) {
					e.ReceiveMessage(p, &core.Message{SessionKey: key, Platform: "feishu", MessageID: id, UserID: "ou", Content: content, ReplyCtx: rc})
				}
				receive("m1", "work")
				select {
				case <-s.sent:
				case <-time.After(4 * time.Second):
					t.Fatal("no prompt")
				}
				wait("placeholder", func() bool { v := read(); return len(v) > 0 && v[0] == "card" })
				if scenario == "queued" {
					receive("m2", "queued work")
					wait("queue notice", func() bool { return len(read()) >= 2 })
				}
				body := "answer"
				if scenario == "multipart" {
					body = strings.Repeat("a", 4100)
				}
				switch scenario {
				case "silent":
					s.events <- core.Event{Type: core.EventResult, Content: "NO_REPLY", Done: true}
				case "error":
					s.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("fixture failure")}
				case "closed":
					s.events <- core.Event{Type: core.EventText, Content: body}
					close(s.events)
				case "stop", "cancel":
					receive(scenario, "/"+scenario)
				default:
					s.events <- core.Event{Type: core.EventResult, Content: body, Done: true}
				}
				if scenario == "queued" {
					select {
					case <-s.sent:
					case <-time.After(4 * time.Second):
						t.Fatal("queue not drained")
					}
					wait("second placeholder", func() bool { mu.Lock(); defer mu.Unlock(); return cards == 2 })
					s.events <- core.Event{Type: core.EventResult, Content: "answer two", Done: true}
				}
				wait("turn ends", func() bool { return !e.GetSessions().GetOrCreateActive(key).Busy() })
				got := read()
				want := map[string][]string{
					"normal": {"card", "send", "delete"}, "multipart": {"card", "send", "send", "delete"},
					"retry": {"card", "send", "send", "delete"}, "send_failed": {"card", "send", "send", "update"},
					"recall_failed": {"card", "send", "delete"},
					"queued":        {"card", "send", "send", "delete", "card", "send", "delete"},
					"silent":        {"card", "delete"}, "error": {"card", "send", "delete"}, "closed": {"card", "send", "delete"}, "stop": {"card", "send", "delete"}, "cancel": {"card", "send", "delete"},
				}[scenario]
				if order == core.FinalReplyRecallFirst {
					if replacement, ok := map[string][]string{
						"normal": {"card", "delete", "send"}, "multipart": {"card", "delete", "send", "send"},
						"retry": {"card", "delete", "send", "send"}, "send_failed": {"card", "delete", "send", "send", "plain"},
						"recall_failed": {"card", "delete", "send"}, "queued": {"card", "send", "delete", "send", "card", "delete", "send"},
						"error": {"card", "delete", "send"}, "closed": {"card", "delete", "send"},
					}[scenario]; ok {
						want = replacement
					}
				}
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("%v want %v", got, want)
				}
			})
		}
	}
}
