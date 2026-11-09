package feishu

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// contentAdds lists the rune growth of every body-content update, in order.
func contentAdds(f *previewFixture) []int {
	var adds []int
	prev := 0
	for _, c := range f.read() {
		if strings.HasSuffix(c.path, "/content") {
			n := len([]rune(c.body["content"].(string)))
			adds = append(adds, n-prev)
			prev = n
		}
	}
	return adds
}

// A long reply is half revealed when the final text arrives. The end of the turn, with every
// card call taking as long as in production (about 0.5 s), must finish the reveal in steps and
// settle the card within about a second of the final.
func TestFinalArrivesMidReveal_StepsAndBudget(t *testing.T) {
	f, p, h := newSmoothPreview(t, func(c *core.StreamPreviewCfg) {
		c.RevealCps, c.PushIntervalMs = 80, 150
		c.TypingIntervalMs, c.TypingStep = 40, 4
	})
	ctx := context.Background()
	if err := p.AddPreviewTool(ctx, h, "读文件"); err != nil {
		t.Fatal(err)
	}
	final := replyBody(strings.Repeat("字", 760))
	_ = p.SetPreviewBody(ctx, h, final)
	now := time.Now()
	for i := 1; i <= 4; i++ { // four seconds of paced reveal
		if i == 4 {
			f.mu.Lock()
			f.delay = 500 * time.Millisecond // production card call latency
			f.mu.Unlock()
		}
		publishAt(t, p, h, now.Add(time.Duration(i)*time.Second))
	}
	h.live.mu.Lock()
	shown := h.live.shown
	h.live.mu.Unlock()
	if shown < 200 || shown > 420 {
		t.Fatalf("setup: %d runes shown", shown)
	}
	callsBefore := len(f.read())
	addsBefore := len(contentAdds(f))

	start := time.Now()
	p.CatchUpPreview(ctx, h, final)
	tCatch := time.Since(start)
	if err := p.FinishSegmentedPreview(ctx, h, final); err != nil {
		t.Fatal(err)
	}
	tTotal := time.Since(start)

	adds := contentAdds(f)[addsBefore:]
	t.Logf("shown at final %d of %d; catch-up %v, finish %v, total %v; %d card calls (%d body updates, growth per update %v)",
		shown, len([]rune(final)), tCatch.Round(time.Millisecond), (tTotal - tCatch).Round(time.Millisecond), tTotal.Round(time.Millisecond), len(f.read())-callsBefore, len(adds), adds)
	if lastContent(f) != final {
		t.Fatalf("card did not end with the whole reply (%d runes)", len([]rune(lastContent(f))))
	}
	// With 0.5 s per call the rest fits two updates in the 1 s budget; neither is the whole backlog.
	for _, n := range adds {
		if n > 250 {
			t.Errorf("one update added %d runes: %v", n, adds)
		}
	}
	if len(adds) < 2 {
		t.Errorf("rest of the reply went out in %d update(s), want it in steps", len(adds))
	}
	// 1 s of reveal, plus the card calls that settle it (close stream, final card) at 0.5 s each.
	if tTotal > 2300*time.Millisecond {
		t.Errorf("end of turn took %v", tTotal)
	}
}

// With fast card calls the rest of the reply is released in small even steps and the reveal ends
// within about a second of the final.
func TestFinalArrivesMidReveal_SmallStepsWithinOneSecond(t *testing.T) {
	f, p, h := newSmoothPreview(t, func(c *core.StreamPreviewCfg) { c.RevealCps, c.PushIntervalMs = 80, 150 })
	ctx := context.Background()
	final := replyBody(strings.Repeat("字", 760))
	_ = p.SetPreviewBody(ctx, h, final)
	now := time.Now()
	for i := 1; i <= 4; i++ {
		publishAt(t, p, h, now.Add(time.Duration(i)*time.Second))
	}
	addsBefore := len(contentAdds(f))
	start := time.Now()
	p.CatchUpPreview(ctx, h, final)
	el := time.Since(start)
	adds := contentAdds(f)[addsBefore:]
	if lastContent(f) != final {
		t.Fatalf("reveal incomplete: %d of %d runes", len([]rune(lastContent(f))), len([]rune(final)))
	}
	if el > 1300*time.Millisecond {
		t.Errorf("reveal took %v", el)
	}
	if len(adds) < 4 {
		t.Errorf("only %d updates: %v", len(adds), adds)
	}
	for _, n := range adds {
		if n > 100 {
			t.Errorf("one update added %d runes: %v", n, adds)
		}
	}
	t.Logf("reveal took %v in %d updates: %v", el.Round(time.Millisecond), len(adds), adds)
}
