package core

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type movePlatform struct {
	failurePlatform
	mmu   sync.Mutex
	moves []any // reply contexts the card moved under
}

func (p *movePlatform) MovePreview(_ context.Context, _ any, rc any) error {
	p.mmu.Lock()
	defer p.mmu.Unlock()
	p.moves = append(p.moves, rc)
	return nil
}

func (p *movePlatform) moved() []any {
	p.mmu.Lock()
	defer p.mmu.Unlock()
	return append([]any(nil), p.moves...)
}

type steeringSession struct {
	*queuedLifecycleSession
	steered chan string
}

func (s *steeringSession) Steer(prompt, _ string, _ []ImageAttachment, _ []FileAttachment) (bool, error) {
	s.steered <- prompt
	return true, nil
}

func newFollowEnv(t *testing.T) (*movePlatform, *steeringSession, *Engine, func(id, content string)) {
	t.Helper()
	old := previewMoveGap
	previewMoveGap = 150 * time.Millisecond
	t.Cleanup(func() { previewMoveGap = old })
	p := &movePlatform{failurePlatform: failurePlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}}
	s := &steeringSession{queuedLifecycleSession: &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 4)}, steered: make(chan string, 8)}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	e.SetStreamPreviewCfg(cfg)
	t.Cleanup(func() { _ = e.Stop() })
	send := func(id, content string) {
		e.ReceiveMessage(p, &Message{SessionKey: "segmented:user", Platform: p.Name(), MessageID: id, UserID: "user", Content: content, ReplyCtx: "ctx-" + id, UserMessageTimeMs: time.Now().UnixMilli()})
	}
	send("1", "写一份周报")
	select {
	case <-s.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt missing")
	}
	return p, s, e, send
}

func TestBurstOfMessagesMovesTheCardOnceBelowTheLast(t *testing.T) {
	p, s, _, send := newFollowEnv(t)
	send("2", "加上下周计划")
	send("3", "用表格")
	for i := 0; i < 2; i++ {
		select {
		case <-s.steered:
		case <-time.After(3 * time.Second):
			t.Fatal("message not steered into the turn")
		}
	}
	waitUntil(t, "card move", func() bool { return len(p.moved()) == 1 })
	time.Sleep(3 * previewMoveGap)
	if got := p.moved(); len(got) != 1 || got[0] != "ctx-3" {
		t.Fatalf("moves=%v, want one move under the last message", got)
	}

	// Later messages move it again, never sooner than the gap.
	start := time.Now()
	send("4", "再加一节风险")
	<-s.steered
	waitUntil(t, "second move", func() bool { return len(p.moved()) == 2 })
	if got := p.moved(); got[1] != "ctx-4" || time.Since(start) > 2*time.Second {
		t.Fatalf("moves=%v", got)
	}
	s.events <- Event{Type: EventResult, Content: "好了", Done: true}
}

func TestFinishedTurnDoesNotMoveItsCard(t *testing.T) {
	p, s, e, send := newFollowEnv(t)
	send("2", "补一句")
	<-s.steered
	s.events <- Event{Type: EventResult, Content: "好了", Done: true}
	deadline := time.Now().Add(3 * time.Second)
	for e.sessions.GetOrCreateActive("segmented:user").Busy() {
		if time.Now().After(deadline) {
			t.Fatal("turn hung")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(2 * previewMoveGap)
	if got := p.moved(); len(got) != 0 {
		t.Fatalf("a finished card moved: %v", got)
	}
}

func TestFollowWaitsForTheGapAfterTheCardStarts(t *testing.T) {
	old := previewMoveGap
	previewMoveGap = 80 * time.Millisecond
	t.Cleanup(func() { previewMoveGap = old })
	p := &movePlatform{failurePlatform: failurePlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}}
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented = true
	sp := newStreamPreview(cfg, p, "ctx", context.Background(), nil)
	start := time.Now()
	sp.previewMsgID, sp.previewStartedAt = 0, start
	sp.followUser("ctx-a")
	sp.followUser("ctx-b")
	waitUntil(t, "move", func() bool { return len(p.moved()) == 1 })
	if elapsed := time.Since(start); elapsed < previewMoveGap {
		t.Fatalf("moved %v after the card started, before the gap", elapsed)
	}
	if got := p.moved(); got[0] != "ctx-b" {
		t.Fatalf("moves=%v", got)
	}
}
