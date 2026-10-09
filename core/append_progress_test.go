package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAppendProgressOrderNoReplacementDedupAndThrottle(t *testing.T) {
	p := &lifecyclePlatform{}
	cfg := DefaultStreamPreviewCfg()
	cfg.IntervalMs = 50
	sp := newStreamPreview(cfg, p, nil, context.Background(), nil)
	sp.startPlaceholder("思考中…")
	events := []Event{
		{Type: EventProgress, ProgressID: "a", Content: "先检查代码。"},
		{Type: EventToolUse, ProgressID: "b", ToolName: "Read", ToolInput: "SECRET"},
		{Type: EventProgress, ProgressID: "c", Content: "再验证行为。"},
		{Type: EventToolUse, ProgressID: "d", ToolName: "Read", ToolInput: "OTHER_SECRET"},
	}
	for _, e := range events {
		text := e.Content
		if e.Type == EventToolUse {
			text = publicToolProgress(e)
		}
		sp.appendProgressEvent(e, text)
	}
	// A replay of the first event must not append, including after other events.
	sp.appendProgressEvent(events[0], events[0].Content)
	// Same source ID with revised text must not rewrite the already accepted row.
	sp.appendProgressEvent(events[0], "修改后的进度")
	sp.appendProgressEvent(Event{Type: EventThinking, Content: "PRIVATE_THINKING"}, "PRIVATE_THINKING")
	p.previewMu.Lock()
	count := len(p.edits)
	p.previewMu.Unlock()
	if count != 0 {
		t.Fatal("updated before throttle interval")
	}
	deadline := time.Now().Add(time.Second)
	for {
		p.previewMu.Lock()
		edits := append([]string(nil), p.edits...)
		p.previewMu.Unlock()
		if len(edits) > 0 {
			if len(edits) != 1 || edits[0] != "card-1:先检查代码。\n读取文件\n再验证行为。\n读取文件" {
				t.Fatal(edits)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("progress never flushed")
		}
		time.Sleep(time.Millisecond)
	}
	// Later updates retain all prior visible progress as an exact prefix.
	sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: "e"}, "最后检查。")
	time.Sleep(80 * time.Millisecond)
	p.previewMu.Lock()
	latest := p.edits[len(p.edits)-1]
	p.previewMu.Unlock()
	if latest != "card-1:先检查代码。\n读取文件\n再验证行为。\n读取文件\n最后检查。" {
		t.Fatal(latest)
	}
	sp.discard()
}

func TestAppendProgressFoldingBudgetsAndOldReplay(t *testing.T) {
	p := &lifecyclePlatform{}
	sp := newStreamPreview(DefaultStreamPreviewCfg(), p, nil, context.Background(), nil)
	sp.startPlaceholder("思考中…")
	defer sp.discard()
	for i := 0; i < 50; i++ {
		sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: fmt.Sprint(i)}, fmt.Sprintf("步骤%d", i))
	}
	if sp.progressFolded != 0 {
		t.Fatal("normal fifty-entry turn folded")
	}
	for i := 50; i < 120; i++ {
		sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: fmt.Sprint(i)}, fmt.Sprintf("步骤%d", i)+strings.Repeat("测", 200))
	}
	before := sp.progressTextLocked()
	if sp.progressFolded == 0 || !strings.HasPrefix(before, fmt.Sprintf("…（前 %d 条已折叠）", sp.progressFolded)) || !progressFits(before) || !strings.Contains(before, "步骤119") || strings.Contains(before, "步骤0\n") {
		t.Fatalf("bad folding: %s", before)
	}
	sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: "0"}, "步骤0")
	if sp.progressTextLocked() != before {
		t.Fatal("folded replay reappeared")
	}
	// Astral runes and JSON-escaped controls must fit too; huge newest event keeps its tail.
	sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: "huge"}, strings.Repeat("\x01😀", 10000)+"最新尾部")
	after := sp.progressTextLocked()
	if !progressFits(after) || !strings.HasSuffix(after, "最新尾部") {
		t.Fatal("huge entry exceeds budget or hides newest")
	}
}

func TestAppendProgressNoIDReplayAndDefaults(t *testing.T) {
	if !(DisplayCfg{Mode: "quiet"}).appendProgress() || (DisplayCfg{Mode: "full"}).appendProgress() || (DisplayCfg{Mode: "quiet", ProgressHistory: "recent"}).appendProgress() {
		t.Fatal("defaults changed")
	}
	p := &lifecyclePlatform{}
	sp := newStreamPreview(DefaultStreamPreviewCfg(), p, nil, context.Background(), nil)
	sp.startPlaceholder("思考中…")
	defer sp.discard()
	e := Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "SECRET"}
	sp.appendProgressEvent(e, publicToolProgress(e))
	sp.appendProgressEvent(e, publicToolProgress(e))
	if sp.progressTextLocked() != "运行命令" {
		t.Fatal(sp.progressTextLocked())
	}
}

func TestKeepProgressCardFinalFlushAndOwnership(t *testing.T) {
	for _, keep := range []bool{false, true} {
		for _, transfer := range []bool{false, true} {
			t.Run(fmt.Sprintf("keep=%v/stop=%v", keep, transfer), func(t *testing.T) {
				p := &lifecyclePlatform{}
				sp := newStreamPreview(DefaultStreamPreviewCfg(), p, nil, context.Background(), nil)
				sp.keepProgressCard = keep
				sp.startPlaceholder("思考中…")
				sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: "a"}, "已完成检查。")
				if transfer {
					original := sp
					sp = sp.detachForFinal()
					original.discard()
				}
				if !sp.deliverFinal("最终回复", "", func(p Platform, ctx any, s string) error { return p.Send(context.Background(), ctx, s) }, "发送失败") {
					t.Fatal("send failed")
				}
				sp.discard()
				time.Sleep(20 * time.Millisecond)
				p.previewMu.Lock()
				defer p.previewMu.Unlock()
				if keep {
					if len(p.deletes) != 0 || len(p.edits) != 1 || p.edits[0] != "card-1:已完成检查。" {
						t.Fatalf("retained=%v deleted=%v", p.edits, p.deletes)
					}
				} else if len(p.deletes) != 1 {
					t.Fatal(p.deletes)
				}
				if sent := p.getSent(); len(sent) != 1 || sent[0] != "最终回复" {
					t.Fatal(sent)
				}
			})
		}
	}
}

func TestAppendProgressFinalFailurePreservesPendingHistory(t *testing.T) {
	p := &lifecyclePlatform{}
	sp := newStreamPreview(DefaultStreamPreviewCfg(), p, nil, context.Background(), nil)
	sp.startPlaceholder("思考中…")
	sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: "a"}, "先检查。")
	sp.appendProgressEvent(Event{Type: EventToolUse, ProgressID: "b", ToolName: "Bash"}, "运行命令")
	if sp.deliverFinal("最终回复", "", func(Platform, any, string) error { return fmt.Errorf("fixture send failed") }, "发送失败") {
		t.Fatal("unexpected success")
	}
	sp.discard()
	p.previewMu.Lock()
	defer p.previewMu.Unlock()
	if len(p.deletes) != 0 || len(p.edits) != 1 || p.edits[0] != "card-1:先检查。\n运行命令\n\n*发送失败*" {
		t.Fatalf("history lost: %v %v", p.edits, p.deletes)
	}
}
