package feishu

import (
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestPacedReveal_ReleasesBurstAtSteadyRate(t *testing.T) {
	v := &segmentedPreview{cfg: core.StreamPreviewCfg{RevealCps: 90, RevealCatchupMs: 4000, PushIntervalMs: 150, TypingIntervalMs: 40, TypingStep: 4}}
	v.pend, v.pendDirty = strings.Repeat("字", 400), true
	now := time.Unix(1000, 0)
	var steps []int
	prev := 0
	for i := 0; i < 60 && v.shown < 400; i++ {
		if err := v.adoptPending(); err != nil {
			t.Fatal(err)
		}
		v.reveal(now)
		steps = append(steps, v.shown-prev)
		prev = v.shown
		now = now.Add(150 * time.Millisecond)
	}
	if v.shown != 400 {
		t.Fatalf("shown %d, want 400", v.shown)
	}
	// 400 chars: backlog rate 100/s is capped at the 100 chars/s typing speed, so about 4 s, not instant.
	if len(steps) < 24 || len(steps) > 30 {
		t.Fatalf("took %d pushes: %v", len(steps), steps)
	}
	for _, s := range steps[1:] {
		if s > 20 {
			t.Fatalf("push released %d chars, above the typing speed budget: %v", s, steps)
		}
	}
	// New text that continues the old one keeps the same element; text that does not restarts it.
	id := v.bodyID
	v.pend, v.pendDirty = v.target+"后续", true
	_ = v.adoptPending()
	if v.bodyID != id || v.shown != 400 {
		t.Fatal("continuation must not restart the reveal")
	}
	v.pend, v.pendDirty = "另起一段", true
	_ = v.adoptPending()
	if v.bodyID == id || v.shown != 0 {
		t.Fatal("rewritten text must restart the reveal")
	}
}
