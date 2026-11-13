package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type sendCall struct {
	method, path, receiveIDType string
	replyInThread               bool
}

// sendRecorder is a fake Feishu API that records how each message was sent:
// a reply (POST /messages/<id>/reply) or a new chat message (POST /messages).
type sendRecorder struct {
	mu    sync.Mutex
	calls []sendCall
	n     int
}

func (s *sendRecorder) platform(t *testing.T) *Platform {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			fmt.Fprint(w, `{"code":0,"expire":7200,"tenant_access_token":"fixture"}`)
			return
		}
		var body struct {
			ReplyInThread bool `json:"reply_in_thread"`
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		s.mu.Lock()
		s.n++
		n := s.n
		s.calls = append(s.calls, sendCall{method: r.Method, path: r.URL.Path,
			receiveIDType: r.URL.Query().Get("receive_id_type"), replyInThread: body.ReplyInThread})
		s.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/cardkit/v1/cards"):
			fmt.Fprint(w, `{"code":0,"data":{"card_id":"card_fixture"}}`)
		case strings.HasSuffix(r.URL.Path, "/reply"):
			fmt.Fprintf(w, `{"code":0,"data":{"message_id":"om_reply_%d"}}`, n)
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/im/v1/messages":
			fmt.Fprintf(w, `{"code":0,"data":{"message_id":"om_new_%d"}}`, n)
		default:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Platform{platformName: "feishu", useInteractiveCard: true, dedup: &core.MessageDedup{},
		appID: t.Name(), appSecret: "fixture", domain: srv.URL, topics: &topicMemory{roots: map[string]string{}},
		client: lark.NewClient(t.Name(), "fixture", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client()))}
}

// last returns the last message send (reply or create), skipping card entity
// and patch calls.
func (s *sendRecorder) last(t *testing.T) sendCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.calls) - 1; i >= 0; i-- {
		c := s.calls[i]
		if c.method == http.MethodPost && strings.HasPrefix(c.path, "/open-apis/im/v1/messages") {
			return c
		}
	}
	t.Fatal("no message sent")
	return sendCall{}
}

func (s *sendRecorder) has(method, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c.method == method && c.path == path {
			return true
		}
	}
	return false
}

func decisionCardFixture() *core.Card {
	return core.NewCard().Title("需要你选择", "orange").Markdown("**用哪个方案？**").
		Buttons(core.CardButton{Text: "方案一", Type: "primary", Value: "act:/decision d1 1"}).Build()
}

func formCardFixture() *core.Card {
	cb := core.NewCard().Title("需要你填写", "blue").Markdown("**出差信息**")
	card := cb.Build()
	card.Elements = append(card.Elements, core.CardForm{Name: "form_d2", SubmitText: "提交", SubmitType: "primary",
		SubmitValue: "act:/form-submit d2", Fields: []core.CardFormField{{Name: "f1", Label: "城市", Kind: "input"}}})
	return card
}

// In a topic, decision, form and task cards are replies in the topic, and the
// returned message ID is the one later patches (deadline, cancel, tap) use.
// Outside topics they stay new chat messages.
func TestCardsStayInTheirTopic(t *testing.T) {
	type setup struct {
		threadIsolation, p2pThreadIsolation bool
		remember                            map[string]string
	}
	for _, tc := range []struct {
		name      string
		setup     setup
		rc        func(p *Platform) replyContext
		replyTo   string // "" = new chat message
		inThread  bool
		noTrigger bool
	}{
		{name: "group topic session", setup: setup{threadIsolation: true},
			rc: func(p *Platform) replyContext { return reconstruct(t, p, "feishu:oc:root:om_root") }, replyTo: "om_root", inThread: true},
		{name: "p2p topic session", setup: setup{p2pThreadIsolation: true},
			rc: func(p *Platform) replyContext { return reconstruct(t, p, "feishu:oc:root:om_root") }, replyTo: "om_root", inThread: true},
		{name: "topic session without quotes", setup: setup{threadIsolation: true}, noTrigger: true,
			rc: func(p *Platform) replyContext { return reconstruct(t, p, "feishu:oc:root:om_root") }, replyTo: "om_root", inThread: true},
		{name: "per-person session, message in a topic",
			rc: func(p *Platform) replyContext {
				return replyContext{messageID: "om_trigger", chatID: "oc", sessionKey: "feishu:oc:ou", inThread: true}
			}, replyTo: "om_trigger", inThread: true},
		{name: "per-person session rebuilt from its topic", setup: setup{remember: map[string]string{"feishu:oc:ou": "om_root"}},
			rc: func(p *Platform) replyContext { return reconstruct(t, p, "feishu:oc:ou") }, replyTo: "om_root", inThread: true},
		{name: "plain group, live",
			rc: func(p *Platform) replyContext {
				return replyContext{messageID: "om_trigger", chatID: "oc", sessionKey: "feishu:oc:ou"}
			}},
		{name: "plain group, rebuilt", setup: setup{threadIsolation: true},
			rc: func(p *Platform) replyContext { return reconstruct(t, p, "feishu:oc:ou") }},
		{name: "p2p", setup: setup{p2pThreadIsolation: true},
			rc: func(p *Platform) replyContext {
				return replyContext{messageID: "om_trigger", chatID: "oc_p2p", sessionKey: "feishu:oc_p2p:ou"}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &sendRecorder{}
			p := rec.platform(t)
			p.threadIsolation, p.p2pThreadIsolation, p.noReplyToTrigger = tc.setup.threadIsolation, tc.setup.p2pThreadIsolation, tc.noTrigger
			for k, v := range tc.setup.remember {
				p.topics.note(k, v)
			}
			rc := tc.rc(p)
			ip := &interactivePlatform{p}
			ctx := context.Background()
			check := func(what, msgID string) {
				t.Helper()
				c := rec.last(t)
				if tc.replyTo != "" {
					if c.path != "/open-apis/im/v1/messages/"+tc.replyTo+"/reply" || c.replyInThread != tc.inThread {
						t.Fatalf("%s: want reply in topic to %s, got %+v", what, tc.replyTo, c)
					}
					if msgID != "" && !strings.HasPrefix(msgID, "om_reply_") {
						t.Fatalf("%s: message ID %q is not the reply's", what, msgID)
					}
					return
				}
				if c.path != "/open-apis/im/v1/messages" || c.receiveIDType != "chat_id" {
					t.Fatalf("%s: want a new chat message, got %+v", what, c)
				}
				if msgID != "" && !strings.HasPrefix(msgID, "om_new_") {
					t.Fatalf("%s: message ID %q is not the new message's", what, msgID)
				}
			}

			id, err := ip.SendCardID(ctx, rc, decisionCardFixture())
			if err != nil {
				t.Fatal(err)
			}
			check("decision card", id)
			if err := ip.PatchCard(ctx, id, rc.sessionKey, decisionCardFixture()); err != nil {
				t.Fatal(err)
			}
			if !rec.has(http.MethodPatch, "/open-apis/im/v1/messages/"+id) {
				t.Fatalf("decision card patch did not target %s", id)
			}

			formID, err := ip.SendCardID(ctx, rc, formCardFixture())
			if err != nil {
				t.Fatal(err)
			}
			check("form card", formID)

			if err := ip.SendCard(ctx, rc, decisionCardFixture()); err != nil {
				t.Fatal(err)
			}
			check("card", "")

			h, err := p.SendTaskCard(ctx, rc, core.TaskCardView{State: core.TaskCardRunning, Title: "整理文件", Label: "进行中", Line: "读取文件"})
			if err != nil {
				t.Fatal(err)
			}
			// Task cards and text quote the trigger outside topics, as before.
			follows := func(what string) {
				t.Helper()
				if tc.replyTo != "" || rc.messageID == "" {
					check(what, "")
					return
				}
				if c := rec.last(t); c.path != "/open-apis/im/v1/messages/"+rc.messageID+"/reply" || c.replyInThread {
					t.Fatalf("%s: want a plain reply to the trigger, got %+v", what, c)
				}
			}
			follows("task card")
			if tc.replyTo != "" && !strings.HasPrefix(h.(*feishuPreviewHandle).messageID, "om_reply_") {
				t.Fatalf("task card message ID %q", h.(*feishuPreviewHandle).messageID)
			}

			if err := p.Send(ctx, rc, "好的"); err != nil {
				t.Fatal(err)
			}
			follows("text")
		})
	}
}

func reconstruct(t *testing.T, p *Platform, key string) replyContext {
	t.Helper()
	rc, err := p.ReconstructReplyCtx(key)
	if err != nil {
		t.Fatal(err)
	}
	return rc.(replyContext)
}

// A per-person session follows its latest message: into a topic, back to the
// main stream, and across a restart.
func TestTopicMemoryFollowsTheSession(t *testing.T) {
	dir := t.TempDir()
	rec := &sendRecorder{}
	p := rec.platform(t)
	p.configureTopicMemory(map[string]any{"cc_data_dir": dir, "cc_project": "proj"})
	received := make(chan *core.Message, 4)
	p.handler = func(_ core.Platform, m *core.Message) { received <- m }
	deliver := func(id, thread, root string) *core.Message {
		t.Helper()
		msg := &larkim.EventMessage{MessageId: strPtr(id), ChatId: strPtr("oc"), ChatType: strPtr("p2p"),
			MessageType: strPtr("text"), Content: strPtr(`{"text":"hi"}`), ThreadId: strPtr(thread), RootId: strPtr(root)}
		if err := p.onMessage(context.Background(), &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
			Sender:  &larkim.EventSender{SenderType: strPtr("user"), SenderId: &larkim.UserId{OpenId: strPtr("ou")}},
			Message: msg}}); err != nil {
			t.Fatal(err)
		}
		select {
		case m := <-received:
			return m
		case <-time.After(3 * time.Second):
			t.Fatal("no dispatch")
			return nil
		}
	}

	m := deliver("om_in_topic", "omt", "om_root")
	if m.SessionKey != "feishu:oc:ou" {
		t.Fatalf("session key %s", m.SessionKey)
	}
	if rc := m.ReplyCtx.(replyContext); !rc.inThread || !p.shouldReplyInThread(rc) {
		t.Fatalf("live reply context lost the topic: %+v", rc)
	}
	if rc := reconstruct(t, p, "feishu:oc:ou"); rc.messageID != "om_root" || !rc.inThread {
		t.Fatalf("rebuilt reply context: %+v", rc)
	}

	restarted := &Platform{platformName: "feishu", appID: p.appID, domain: p.domain}
	restarted.configureTopicMemory(map[string]any{"cc_data_dir": dir, "cc_project": "proj"})
	if rc := reconstruct(t, restarted, "feishu:oc:ou"); rc.messageID != "om_root" || !rc.inThread {
		t.Fatalf("topic lost across restart: %+v", rc)
	}

	m = deliver("om_main", "", "")
	if rc := m.ReplyCtx.(replyContext); rc.inThread {
		t.Fatalf("main-stream message marked as topic: %+v", rc)
	}
	if rc := reconstruct(t, p, "feishu:oc:ou"); rc.messageID != "" || rc.inThread {
		t.Fatalf("session back in the main stream still rebuilt into a topic: %+v", rc)
	}
}
