package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Event JSON intentionally separates thread_id from the ordinary reply tree.
func TestP2PThreadSessionKeys(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, p2p := range []bool{false, true} {
			p := &Platform{platformName: "feishu", threadIsolation: group, p2pThreadIsolation: p2p}
			for _, tc := range []struct {
				name, event, main, thread string
				isolated                  bool
			}{
				{"top", `{"chat_type":"p2p","message_id":"om_top"}`, "feishu:oc:ou", "", false},
				{"quote", `{"chat_type":"p2p","message_id":"om_quote","parent_id":"om_parent","root_id":"om_root"}`, "feishu:oc:ou", "", false},
				{"empty thread", `{"chat_type":"p2p","message_id":"om_quote","root_id":"om_root","thread_id":""}`, "feishu:oc:ou", "", false},
				{"thread A", `{"chat_type":"p2p","message_id":"om_reply","parent_id":"om_parent","root_id":"om_a","thread_id":"omt_a"}`, "feishu:oc:ou", "feishu:oc:root:om_a", p2p},
				{"thread B", `{"chat_type":"p2p","message_id":"om_reply","root_id":"om_b","thread_id":"omt_b"}`, "feishu:oc:ou", "feishu:oc:root:om_b", p2p},
				{"thread root", `{"chat_type":"p2p","message_id":"om_root","thread_id":"omt_a"}`, "feishu:oc:ou", "feishu:oc:root:om_root", p2p},
				{"group top", `{"chat_type":"group","message_id":"om_top"}`, "feishu:oc:ou", "feishu:oc:root:om_top", group},
				{"group quote", `{"chat_type":"group","message_id":"om_reply","root_id":"om_a"}`, "feishu:oc:ou", "feishu:oc:root:om_a", group},
				{"group thread", `{"chat_type":"group","message_id":"om_reply","root_id":"om_b","thread_id":"omt_b"}`, "feishu:oc:ou", "feishu:oc:root:om_b", group},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var msg larkim.EventMessage
					if err := json.Unmarshal([]byte(tc.event), &msg); err != nil {
						t.Fatal(err)
					}
					want := tc.main
					if tc.isolated {
						want = tc.thread
					}
					if got := p.makeSessionKey(&msg, "oc", "ou"); got != want {
						t.Fatalf("group=%t p2p=%t key=%s want=%s", group, p2p, got, want)
					}
					rc := replyContext{messageID: "om_reply", chatID: "oc", sessionKey: want}
					if got := p.shouldReplyInThread(rc); got != tc.isolated {
						t.Fatalf("reply_in_thread=%t", got)
					}
					if tc.isolated {
						restored, err := p.ReconstructReplyCtx(want)
						if err != nil {
							t.Fatal(err)
						}
						if !p.shouldReplyInThread(restored.(replyContext)) {
							t.Fatal("restored thread lost")
						}
						if p.sessionKeyFromCardAction("oc", "ou", map[string]any{"session_key": want}) != want {
							t.Fatal("card command lost scope")
						}
					}
				})
			}
		}
	}
	for _, enabled := range []bool{false, true} {
		opts := map[string]any{"app_id": "p2p-option-test", "app_secret": "test", "enable_feishu_card": false}
		if enabled {
			opts["p2p_thread_isolation"] = true
		}
		platform, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		if platform.(*Platform).p2pThreadIsolation != enabled {
			t.Fatal("option default/parsing")
		}
	}
	p := &Platform{platformName: "lark", p2pThreadIsolation: true, shareSessionInChannel: true}
	msg := &larkim.EventMessage{ChatType: strPtr("p2p"), ThreadId: strPtr("omt"), RootId: strPtr("om_root")}
	if p.makeSessionKey(msg, "oc", "ou") != "lark:oc:root:om_root" || p.makeSessionKey(nil, "oc", "ou") != "lark:oc" {
		t.Fatal("tag or legacy shared session changed")
	}
}

// Exercise the actual event dispatcher and HTTP reply API, including attachment
// delivery, bootstrap quote once, later quote suppression, and ordinary quotes.
func TestP2PThreadDispatchAndReply(t *testing.T) {
	var quoteFetches atomic.Int32
	replies := make(chan bool, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "tenant_access_token"):
			_, _ = w.Write([]byte(`{"code":0,"expire":7200,"tenant_access_token":"test-token"}`))
		case strings.HasSuffix(r.URL.Path, "/reply"):
			var body struct {
				ReplyInThread bool `json:"reply_in_thread"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			replies <- body.ReplyInThread
			_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"om_bot"}}`))
		case strings.Contains(r.URL.Path, "/resources/"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
		case r.URL.Path == "/open-apis/im/v1/messages/om_root":
			quoteFetches.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"msg_type":"text","sender":{"id":"ou","sender_type":"user"},"body":{"content":"{\"text\":\"ROOT CONTEXT\"}"}}]}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer srv.Close()
	received := make(chan *core.Message, 8)
	p := &Platform{platformName: "feishu", p2pThreadIsolation: true, dedup: &core.MessageDedup{},
		appID: "p2p-dispatch-test", appSecret: "test", domain: srv.URL,
		client:  lark.NewClient("p2p-dispatch-test", "test", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
		handler: func(_ core.Platform, m *core.Message) { received <- m }}
	send := func(id, kind, content, thread string) *core.Message {
		t.Helper()
		msg := &larkim.EventMessage{MessageId: strPtr(id), ChatId: strPtr("oc"), ChatType: strPtr("p2p"), MessageType: strPtr(kind), Content: strPtr(content), RootId: strPtr("om_root"), ParentId: strPtr("om_root"), ThreadId: strPtr(thread)}
		err := p.onMessage(context.Background(), &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{Sender: &larkim.EventSender{SenderType: strPtr("user"), SenderId: &larkim.UserId{OpenId: strPtr("ou")}}, Message: msg}})
		if err != nil {
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
	first := send("om_first", "text", `{"text":"hello"}`, "omt")
	if !first.IsolateAgent || first.SessionKey != "feishu:oc:root:om_root" || !strings.Contains(first.ExtraContent, "ROOT CONTEXT") || first.ChannelKey != "oc:topic:om_root" || first.LegacyChannelKey != "oc" {
		t.Fatalf("bootstrap: %+v", first)
	}
	p.noReplyToTrigger = true
	for _, command := range []string{"/new", "/model haiku", "/stop"} {
		m := send("om"+command, "text", `{"text":"`+command+`"}`, "omt")
		if !m.IsolateAgent || m.SessionKey != first.SessionKey || m.Content != command || m.ExtraContent != "" {
			t.Fatalf("thread command: %+v", m)
		}
		if err := p.Reply(context.Background(), m.ReplyCtx, "ack"); err != nil {
			t.Fatal(err)
		}
		if !<-replies {
			t.Fatal("reply escaped thread")
		}
	}
	img := send("om_img", "image", `{"image_key":"img_test"}`, "omt")
	if img.SessionKey != first.SessionKey || len(img.Images) != 1 {
		t.Fatalf("attachment lost: %+v", img)
	}
	if quoteFetches.Load() != 1 {
		t.Fatalf("expected bootstrap only; fetches=%d", quoteFetches.Load())
	}
	p.noReplyToTrigger = false
	quote := send("om_quote", "text", `{"text":"ordinary quote"}`, "")
	if quote.SessionKey != "feishu:oc:ou" || !strings.Contains(quote.ExtraContent, "ROOT CONTEXT") {
		t.Fatalf("ordinary quote changed: %+v", quote)
	}
	if err := p.Reply(context.Background(), quote.ReplyCtx, "ack"); err != nil {
		t.Fatal(err)
	}
	if <-replies {
		t.Fatal("ordinary quote became a thread reply")
	}
}
