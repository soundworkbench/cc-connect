package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Guards stream_partial_text rendering: the block being written appears below the
// progress rows, shares their throttle, and the next progress row supersedes it.
func TestStreamBlockBelowProgressRowsThenSuperseded(t *testing.T) {
	p := &lifecyclePlatform{}
	cfg := DefaultStreamPreviewCfg()
	cfg.IntervalMs = 30
	sp := newStreamPreview(cfg, p, nil, context.Background(), nil)
	sp.startPlaceholder("思考中…")
	sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: "a"}, "先检查代码。")
	sp.streamBlock("正在写")
	sp.streamBlock("正在写结论")
	latest := func() string {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			p.previewMu.Lock()
			n := len(p.edits)
			var last string
			if n > 0 {
				last = p.edits[n-1]
			}
			p.previewMu.Unlock()
			if last != "" {
				return last
			}
			time.Sleep(time.Millisecond)
		}
		return ""
	}
	time.Sleep(60 * time.Millisecond)
	if got := latest(); got != "card-1:先检查代码。\n\n正在写结论" {
		t.Fatal(got)
	}
	sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: "b"}, "正在写结论")
	time.Sleep(60 * time.Millisecond)
	if got := latest(); got != "card-1:先检查代码。\n正在写结论" {
		t.Fatal(got)
	}
	sp.discard()
}

// A CJK reply over 4000 bytes but under 4000 characters must finish the streamed
// card in place; only replies over the character limit spill into follow-up messages.
func TestDeliverFinalKeepsPreviewForLongCJK(t *testing.T) {
	for _, tc := range []struct {
		runes, sends int
	}{{1500, 0}, {4500, 1}} {
		mp := &mockKeepPreviewPlatform{}
		sp := newStreamPreview(DefaultStreamPreviewCfg(), mp, "ctx", context.Background(), nil)
		sp.startPlaceholder("思考中…")
		body := strings.Repeat("洞察中心每天采集。\n", tc.runes/10)
		var sent []string
		if !sp.deliverFinal(body, "", func(_ Platform, _ any, s string) error { sent = append(sent, s); return nil }, "失败") {
			t.Fatalf("runes=%d: deliverFinal failed", tc.runes)
		}
		sp.discard()
		if len(mp.deleted) != 0 || len(sent) != tc.sends {
			t.Fatalf("runes=%d: deleted=%d sends=%d", tc.runes, len(mp.deleted), len(sent))
		}
		msgs := mp.getMessages()
		last := strings.TrimPrefix(msgs[len(msgs)-1], "update:")
		if got := last + strings.Join(sent, ""); strings.ReplaceAll(got, "\n", "") != strings.ReplaceAll(body, "\n", "") {
			t.Fatalf("runes=%d: delivered text differs from body", tc.runes)
		}
	}
}

// While the turn runs the card ends with the typing marker, its dots move on each
// repaint, and the final reply finished in place carries no marker.
func TestTypingMarkerDuringTurnGoneOnFinish(t *testing.T) {
	mp := &mockKeepPreviewPlatform{}
	cfg := DefaultStreamPreviewCfg()
	cfg.TypingMarker = "输出中"
	sp := newStreamPreview(cfg, mp, "ctx", context.Background(), nil)
	sp.startPlaceholder("先查一下。")
	sp.replacePreview("先查一下。\n\n第一段")
	msgs := mp.getMessages()
	if len(msgs) != 2 || !strings.HasSuffix(msgs[0], "先查一下。\n\n输出中.") || !strings.HasSuffix(msgs[1], "第一段\n\n输出中..") {
		t.Fatalf("preview frames: %q", msgs)
	}
	if !sp.deliverFinal("第一段\n\n第二段", "", func(Platform, any, string) error { return nil }, "失败") {
		t.Fatal("deliverFinal failed")
	}
	msgs = mp.getMessages()
	if last := msgs[len(msgs)-1]; strings.Contains(last, "输出中") || !strings.HasSuffix(last, "第二段") {
		t.Fatalf("final card: %q", last)
	}
	sp.discard()
}
