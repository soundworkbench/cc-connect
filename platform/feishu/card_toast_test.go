package feishu

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestCardActionToastKeepsTheCard(t *testing.T) {
	ip, _ := newAllowFromTestPlatform(t, "ou_allowed")
	ip.cardNavHandler = func(string, string) *core.Card {
		return &core.Card{Toast: "这个按钮已失效，请重新发送消息"}
	}
	resp, err := ip.onCardAction(cardActionEvent("ou_allowed", "act:/retry stale"))
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Card != nil || resp.Toast == nil || resp.Toast.Content != "这个按钮已失效，请重新发送消息" {
		t.Fatalf("resp=%+v", resp)
	}
}

// KeepPreviewInPlace runs outside the preview lock while a move may switch
// the handle to the re-posted card; run with -race.
func TestKeepPreviewInPlaceReadsHandleUnderLock(t *testing.T) {
	_, p := newPreviewFixture(t)
	p.keepPreviewMaxAge = time.Nanosecond
	h := &feishuPreviewHandle{messageID: "om_a", chatID: "oc"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			h.mu.Lock()
			h.messageID = "om_b"
			h.mu.Unlock()
		}
	}()
	for i := 0; i < 200; i++ {
		if p.KeepPreviewInPlace(context.Background(), h, time.Second) {
			t.Fatal("long turn kept in place")
		}
	}
	wg.Wait()
}
