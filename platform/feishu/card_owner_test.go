package feishu

import (
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestOtherUsersClickOnACardOnlyGetsANotice(t *testing.T) {
	ip, _ := newAllowFromTestPlatform(t, "ou_owner,ou_other")
	ip.userNameCache.Store("ou_owner", "王五")
	calls := make(chan string, 4)
	ip.cardNavHandler = func(action, key string) *core.Card { calls <- action; return nil }
	click := func(user, sessionKey string) *string {
		t.Helper()
		ev := cardActionEvent(user, "act:/decision d1 1")
		ev.Event.Action.Value["session_key"] = sessionKey
		resp, err := ip.onCardAction(ev)
		if err != nil {
			t.Fatal(err)
		}
		if resp == nil || resp.Toast == nil {
			return nil
		}
		if resp.Card != nil {
			t.Fatalf("the clicked card must stay: %+v", resp)
		}
		return &resp.Toast.Content
	}
	if got := click("ou_other", "feishu:oc_test_chat:ou_owner"); got == nil || *got != "这个按钮是给 王五 的" {
		t.Fatalf("toast=%v", got)
	}
	select {
	case a := <-calls:
		t.Fatalf("another user's click ran %q", a)
	default:
	}
	// The owner's click, a shared session and a thread session go through.
	for _, key := range []string{"feishu:oc_test_chat:ou_owner", "feishu:oc_test_chat", "feishu:oc_test_chat:root:om_root"} {
		user := "ou_other"
		if key == "feishu:oc_test_chat:ou_owner" {
			user = "ou_owner"
		}
		if got := click(user, key); got != nil {
			t.Fatalf("%s: unexpected toast %q", key, *got)
		}
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatalf("%s: click not handled", key)
		}
	}
}

func TestNotYourButtonWithoutAName(t *testing.T) {
	ip, _ := newAllowFromTestPlatform(t, "ou_other")
	old := ownerNameWait
	ownerNameWait = 50 * time.Millisecond
	t.Cleanup(func() { ownerNameWait = old })
	if got := ip.notYourButton("ou_unknown").Toast.Content; got != "这个按钮不是给你的" {
		t.Fatalf("toast=%q", got)
	}
}
