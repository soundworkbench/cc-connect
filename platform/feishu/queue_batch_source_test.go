package feishu

import (
	"context"
	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"testing"
	"time"
)

func TestDispatchCoreMessage_QueueBatchSource(t *testing.T) {
	for _, tc := range []struct{ name, contextSource, explicit, want string }{
		{"real-user", "", "", ""}, {"role-app", "relay", "", "relay"}, {"explicit", "relay", "timer", "timer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *core.Message
			p := &Platform{handler: func(_ core.Platform, msg *core.Message) { got = msg }}
			p.dispatchCoreMessage(&core.Message{Content: "hello", Source: tc.explicit, ReplyCtx: replyContext{source: tc.contextSource}})
			if got == nil || got.Source != tc.want {
				t.Fatalf("message=%+v want source=%q", got, tc.want)
			}
		})
	}
}

func TestOnMessage_QueueBatchRealUserAndAppClassification(t *testing.T) {
	for _, tc := range []struct{ sender, want string }{{"user", ""}, {"app", "relay"}} {
		t.Run(tc.sender, func(t *testing.T) {
			received := make(chan *core.Message, 1)
			p := &Platform{platformName: "feishu", dedup: &core.MessageDedup{}, handler: func(_ core.Platform, msg *core.Message) { received <- msg }}
			p.userNameCache.Store("ou_sender", "sender")
			p.chatNameCache.Store("oc_chat", "chat")
			event := &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
				Sender:  &larkim.EventSender{SenderType: strPtr(tc.sender), SenderId: &larkim.UserId{OpenId: strPtr("ou_sender")}},
				Message: &larkim.EventMessage{MessageId: strPtr("message"), ChatId: strPtr("oc_chat"), ChatType: strPtr("p2p"), MessageType: strPtr("text"), Content: strPtr(`{"text":"hello"}`)},
			}}
			if err := p.onMessage(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-received:
				if got.Source != tc.want || got.Content != "hello" {
					t.Fatalf("message=%+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("SDK event was not dispatched")
			}
		})
	}
}
