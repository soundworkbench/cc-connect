package feishu

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func newSmoothPreview(t *testing.T, mod func(*core.StreamPreviewCfg)) (*previewFixture, *Platform, *feishuPreviewHandle) {
	t.Helper()
	f, p := newPreviewFixture(t)
	cfg := core.DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	cfg.PushIntervalMs = 60000
	cfg.TypingIntervalMs, cfg.TypingStep = 40, 4 // 100 chars/s
	cfg.PrintStrategy = "delay"
	if mod != nil {
		mod(&cfg)
	}
	handle, err := p.StartSegmentedPreview(context.Background(), replyContext{chatID: "oc"}, cfg, core.NewI18n(core.LangChinese))
	if err != nil {
		t.Fatal(err)
	}
	h := handle.(*feishuPreviewHandle)
	t.Cleanup(h.live.stop)
	return f, p, h
}

func (f *previewFixture) countSuffix(suffix string) int {
	n := 0
	for _, c := range f.read() {
		if strings.HasSuffix(c.path, suffix) {
			n++
		}
	}
	return n
}

func publishAt(t *testing.T, p *Platform, h *feishuPreviewHandle, now time.Time) {
	t.Helper()
	h.live.mu.Lock()
	defer h.live.mu.Unlock()
	if err := p.publishSegmented(context.Background(), h, now); err != nil {
		t.Fatal(err)
	}
}

// While text flows, the running dots stand still, so every card call carries text.
// Step changes still go out at once, and the dots move again when text stops.
func TestDotsStandStillWhileTextFlows(t *testing.T) {
	f, p, h := newSmoothPreview(t, nil)
	ctx := context.Background()
	if err := p.AddPreviewTool(ctx, h, "跑检查"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	publishAt(t, p, h, t0) // first frame: the row appears
	progressPath := "/elements/" + h.live.progressID
	base := f.countSuffix(progressPath)

	_ = p.SetPreviewBody(ctx, h, "第一段文字")
	for i := 1; i <= 3; i++ { // dots would change on each of these seconds
		_ = p.SetPreviewBody(ctx, h, strings.Repeat("字", 5+i*5))
		publishAt(t, p, h, time.Now().Add(time.Duration(i)*500*time.Millisecond))
	}
	if got := f.countSuffix(progressPath) - base; got != 0 {
		t.Fatalf("%d step list updates while text flowed, want 0", got)
	}
	if f.countSuffix("/content") < 3 {
		t.Fatalf("text updates missing: %v", f.read())
	}

	// A new step is shown at once even while text flows.
	_ = p.AddPreviewTool(ctx, h, "再跑一步")
	_ = p.SetPreviewBody(ctx, h, strings.Repeat("字", 40))
	publishAt(t, p, h, time.Now().Add(4*time.Second))
	if got := f.countSuffix(progressPath) - base; got != 1 {
		t.Fatalf("step change updates: %d, want 1", got)
	}

	// No text for longer than the pause: the dots move again.
	publishAt(t, p, h, time.Now().Add(11*time.Second))
	if got := f.countSuffix(progressPath) - base; got != 2 {
		t.Fatalf("dots did not resume when text stopped: %d updates", got)
	}
}

// Text that arrives in a burst goes out with as few card calls as the push loop can
// make: one update per frame, and each carries everything released since the last.
func TestBodyUpdatesCoalesce(t *testing.T) {
	f, p, h := newSmoothPreview(t, func(c *core.StreamPreviewCfg) { c.RevealCps, c.PushIntervalMs = 90, 150 })
	ctx := context.Background()
	_ = p.AddPreviewTool(ctx, h, "读文件")
	for i := 1; i <= 50; i++ { // many small upstream deltas
		_ = p.SetPreviewBody(ctx, h, strings.Repeat("字", i*4))
	}
	now := time.Now()
	for i := 0; i < 3; i++ { // three frames, each 700 ms apart like slow card calls
		publishAt(t, p, h, now.Add(time.Duration(i)*500*time.Millisecond))
	}
	if got := f.countSuffix("/content"); got != 3 {
		t.Fatalf("%d body calls for 3 frames, want 3", got)
	}
	h.live.mu.Lock()
	shown := h.live.shown
	h.live.mu.Unlock()
	if shown < 90 || shown > 190 { // 700 ms at 90 chars/s per frame, first frame one tick
		t.Fatalf("frames released %d chars", shown)
	}
}

func lastContent(f *previewFixture) string {
	calls := f.read()
	for i := len(calls) - 1; i >= 0; i-- {
		if strings.HasSuffix(calls[i].path, "/content") {
			return calls[i].body["content"].(string)
		}
	}
	return ""
}

// Before the card is recalled it gets the whole reply in one update and the client
// time to type it; the wait is the typing time, capped.
func TestCatchUpSendsRestAndWaitsForTyping(t *testing.T) {
	f, p, h := newSmoothPreview(t, nil)
	ctx := context.Background()
	_ = p.SetPreviewBody(ctx, h, strings.Repeat("字", 10))
	publishAt(t, p, h, time.Now())
	final := strings.Repeat("字", 40) // 30 more characters: 300 ms of typing at 100 chars/s
	before := f.countSuffix("/content")
	start := time.Now()
	p.CatchUpPreview(ctx, h, final)
	el := time.Since(start)
	if f.countSuffix("/content")-before != 1 || lastContent(f) != final {
		t.Fatalf("want the rest in one update, got %d (last %d chars)", f.countSuffix("/content")-before, len([]rune(lastContent(f))))
	}
	if el < 200*time.Millisecond || el > time.Second {
		t.Fatalf("waited %v, want about the typing time of 300 ms", el)
	}
	h.live.mu.Lock()
	stopped := h.live.stopped
	h.live.mu.Unlock()
	if !stopped {
		t.Fatal("push loop still running after catch-up")
	}
}

func TestCatchUpWaitIsCapped(t *testing.T) {
	_, p, h := newSmoothPreview(t, func(c *core.StreamPreviewCfg) { c.FinishCatchupMs = 120 })
	ctx := context.Background()
	_ = p.SetPreviewBody(ctx, h, "短")
	publishAt(t, p, h, time.Now())
	start := time.Now()
	p.CatchUpPreview(ctx, h, strings.Repeat("字", 2000)) // 20 s of typing
	if el := time.Since(start); el > time.Second {
		t.Fatalf("waited %v despite a 120 ms cap", el)
	}
}

// A card that already shows the whole reply, typed out, is left alone.
func TestCatchUpNoChangeWhenComplete(t *testing.T) {
	f, p, h := newSmoothPreview(t, nil)
	ctx := context.Background()
	_ = p.SetPreviewBody(ctx, h, "完整答复")
	publishAt(t, p, h, time.Now())
	time.Sleep(100 * time.Millisecond) // four characters take 40 ms to type
	before := len(f.read())
	start := time.Now()
	p.CatchUpPreview(ctx, h, "完整答复")
	if el := time.Since(start); el > 50*time.Millisecond || len(f.read()) != before {
		t.Fatalf("complete card changed: waited %v, %d new calls", el, len(f.read())-before)
	}
}

// A reply that does not continue the streamed text is not forced into the card.
func TestCatchUpSkipsDivergentReply(t *testing.T) {
	f, p, h := newSmoothPreview(t, nil)
	ctx := context.Background()
	_ = p.SetPreviewBody(ctx, h, "草稿")
	publishAt(t, p, h, time.Now())
	before := len(f.read())
	p.CatchUpPreview(ctx, h, "另一份答复")
	if len(f.read()) != before {
		t.Fatal("divergent reply sent to the card")
	}
}

func (f *previewFixture) deletes() int {
	n := 0
	for _, c := range f.read() {
		if c.method == "DELETE" {
			n++
		}
	}
	return n
}

func lastCardData(t *testing.T, f *previewFixture) string {
	t.Helper()
	calls := f.read()
	for i := len(calls) - 1; i >= 0; i-- {
		if card, ok := calls[i].body["card"].(map[string]any); ok {
			return card["data"].(string)
		}
	}
	t.Fatal("no card update")
	return ""
}

// A long turn: the card folds to "done · N steps" with no reply text, and nothing is recalled.
func TestCollapsePreviewLeavesStepSummaryOnly(t *testing.T) {
	f, p, h := newSmoothPreview(t, nil)
	ctx := context.Background()
	for _, s := range []string{"第一步", "第二步"} {
		_ = p.AddPreviewTool(ctx, h, s)
	}
	_ = p.SetPreviewBody(ctx, h, "答复写了一半")
	publishAt(t, p, h, time.Now())
	if err := p.CollapsePreview(ctx, h); err != nil {
		t.Fatal(err)
	}
	data := lastCardData(t, f)
	if !strings.Contains(data, "已完成 · 共 2 步") || strings.Contains(data, "答复写了一半") || strings.Contains(data, `"streaming_mode":true`) {
		t.Fatalf("collapsed card: %s", data)
	}
	if f.deletes() != 0 {
		t.Fatal("card was recalled")
	}
}

// A short turn: the reply is finished inside the card under the folded steps; no recall.
func TestSettleShortTurnReplyInsideCard(t *testing.T) {
	f, p, h := newSmoothPreview(t, nil)
	ctx := context.Background()
	_ = p.AddPreviewTool(ctx, h, "第一步")
	_ = p.SetPreviewBody(ctx, h, strings.Repeat("字", 10))
	publishAt(t, p, h, time.Now())
	final := strings.Repeat("字", 30)
	p.CatchUpPreview(ctx, h, final)
	if err := p.FinishSegmentedPreview(ctx, h, final); err != nil {
		t.Fatal(err)
	}
	data := lastCardData(t, f)
	if !strings.Contains(data, "已完成 · 共 1 步") || !strings.Contains(data, final) || strings.Contains(data, "⏳") {
		t.Fatalf("final card: %s", data)
	}
	if f.deletes() != 0 {
		t.Fatal("card was recalled")
	}
}
