package core

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// timerTaskPlatform shows task cards and can start proactive messages.
type timerTaskPlatform struct{ *taskCardPlatform }

func (p *timerTaskPlatform) ReconstructReplyCtx(key string) (any, error) { return key, nil }

// bgTurnSession ends its turn at once while a background task it started is
// still running, as Claude Code does for a run_in_background subagent.
type bgTurnSession struct {
	*cujAgentSession
	bg atomic.Bool
}

func (s *bgTurnSession) HasBackgroundWork() bool { return s.Alive() && s.bg.Load() }

func (s *bgTurnSession) closes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCount
}

// Regression (butler ux8, 06:39 JST): a timer with session_mode new-per-run
// whose turn started a background subagent closed the agent right after the
// turn's result. The busy process was stopped, the task died, and no task
// card opened because the task never lived past the card's open delay.
func TestExecuteTimerJob_NewPerRunWaitsForBackgroundTask(t *testing.T) {
	oldPoll := backgroundWorkPoll
	backgroundWorkPoll = 10 * time.Millisecond
	defer func() { backgroundWorkPoll = oldPoll }()

	p := &timerTaskPlatform{&taskCardPlatform{failurePlatform: failurePlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}}}
	s := &bgTurnSession{cujAgentSession: newCUJAgentSession()}
	s.bg.Store(true)
	s.pendingEvents = []Event{
		{Type: EventToolUse, ToolName: "Agent"},
		{Type: EventTask, Task: &TaskEvent{Kind: TaskStarted, TaskID: "t1", Description: "后台整理周报", Background: yes()}},
		{Type: EventResult, Content: "已在后台开始整理", Done: true},
	}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.taskCards.openDelay = 60 * time.Millisecond
	e.taskCards.minGap = 20 * time.Millisecond
	e.taskCards.tick = 10 * time.Millisecond
	t.Cleanup(func() { _ = e.Stop() })

	done := make(chan error, 1)
	go func() {
		done <- e.ExecuteTimerJob(&TimerJob{ID: "bg", SessionKey: "segmented:chat:user", Prompt: "后台整理周报", SessionMode: "new-per-run"})
	}()

	waitUntil(t, "task card while the timer run waits", func() bool { sent, _ := p.cards(); return len(sent) == 1 })
	select {
	case err := <-done:
		t.Fatalf("timer job returned while its background task was running (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	if n := s.closes(); n != 0 {
		t.Fatalf("agent closed %d times while its background task was running", n)
	}

	s.events <- Event{Type: EventTask, Task: &TaskEvent{Kind: TaskEnded, TaskID: "t1", Status: "completed", Summary: "周报整理完成"}}
	s.bg.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("timer job: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timer job did not finish after its background task ended")
	}
	if n := s.closes(); n != 1 {
		t.Fatalf("agent closes = %d, want 1 after the task ended", n)
	}
	waitUntil(t, "settled task card", func() bool {
		_, up := p.cards()
		return len(up) > 0 && up[len(up)-1].view.State == TaskCardCompleted
	})
}
