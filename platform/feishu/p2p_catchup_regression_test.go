package feishu

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// This regression and its fixture also compile against the pre-fix source.
// No Start call is made: cached names avoid HTTP and no real app is contacted.
func newCatchupRegressionPlatform(t *testing.T, dir string, enabled bool) (*Platform, chan *core.Message) {
	t.Helper()
	platform, err := newPlatform("feishu", lark.FeishuBaseUrl, map[string]any{
		"app_id": "catchup-regression", "app_secret": "test-secret", "allow_from": "*",
		"cc_data_dir": dir, "cc_project": "test", "enable_feishu_card": false,
		"p2p_catchup": enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := platform.(*Platform)
	got := make(chan *core.Message, 32)
	p.handler = func(_ core.Platform, msg *core.Message) { got <- msg }
	p.userNameCache.Store("ou_user", "User")
	p.chatNameCache.Store("oc_private", "Private")
	return p, got
}

func catchupRegressionEvent(id, chatType string, created time.Time) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: strPtr("ou_user")}, SenderType: strPtr("user")},
		Message: &larkim.EventMessage{MessageId: &id, ChatId: strPtr("oc_private"), ChatType: &chatType,
			MessageType: strPtr("text"), Content: strPtr(`{"text":"message during restart"}`),
			CreateTime: strPtr(strconv.FormatInt(created.UnixMilli(), 10))},
	}}
}

func TestP2PCatchup_RecentOldPrivateMessage(t *testing.T) {
	p, got := newCatchupRegressionPlatform(t, t.TempDir(), true)
	created := core.StartTime.Add(-time.Minute)
	if err := p.onMessage(context.Background(), catchupRegressionEvent("om_missed", "p2p", created)); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		if msg.MessageID != "om_missed" || msg.Content != "message during restart" {
			t.Fatalf("unexpected delivered message: %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("recent old private message was lost during restart")
	}
}

func TestP2PCatchup_OldMessagePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, chatType string
		age            time.Duration
		enabled        bool
	}{
		{"over_ten_minutes", "p2p", 11 * time.Minute, true},
		{"old_group_unchanged", "group", time.Minute, true},
		{"disabled", "p2p", time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, got := newCatchupRegressionPlatform(t, t.TempDir(), tc.enabled)
			event := catchupRegressionEvent("om_old", tc.chatType, core.StartTime.Add(-tc.age))
			if err := p.onMessage(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			select {
			case msg := <-got:
				t.Fatalf("old message unexpectedly delivered: %+v", msg)
			case <-time.After(30 * time.Millisecond):
			}
		})
	}
}

func TestP2PCatchup_PersistedIDDoesNotReplyAfterRestart(t *testing.T) {
	dir := t.TempDir()
	p, got := newCatchupRegressionPlatform(t, dir, true)
	event := catchupRegressionEvent("om_processed", "p2p", core.StartTime.Add(-time.Minute))
	if err := p.onMessage(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("first message not delivered")
	}
	restarted, after := newCatchupRegressionPlatform(t, dir, true)
	if err := restarted.onMessage(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-after:
		t.Fatalf("persisted message was delivered twice: %+v", msg)
	case <-time.After(30 * time.Millisecond):
	}
}
