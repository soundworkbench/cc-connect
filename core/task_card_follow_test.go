package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type taskCardMove struct {
	from, to, rc any
	view         TaskCardView
}

// movingTaskCards records task cards and their moves; each move returns a
// new handle "moved-<n>".
type movingTaskCards struct {
	taskCardPlatform
	mmu   sync.Mutex
	moves []taskCardMove
}

func (p *movingTaskCards) MoveTaskCard(_ context.Context, h any, rc any, v TaskCardView) (any, error) {
	p.mmu.Lock()
	defer p.mmu.Unlock()
	to := fmt.Sprintf("moved-%d", len(p.moves))
	p.moves = append(p.moves, taskCardMove{h, to, rc, v})
	return to, nil
}

func (p *movingTaskCards) moved() []taskCardMove {
	p.mmu.Lock()
	defer p.mmu.Unlock()
	return append([]taskCardMove(nil), p.moves...)
}

type taskFollowEnv struct {
	t    *testing.T
	p    *movingTaskCards
	s    *stoppableSession
	e    *Engine
	key  string
	msgs int
}

// newTaskFollowEnv runs a first turn that leaves one background task running
// with its card open; the card is below that turn's reply.
func newTaskFollowEnv(t *testing.T, gap time.Duration) *taskFollowEnv {
	t.Helper()
	p := &movingTaskCards{taskCardPlatform: taskCardPlatform{failurePlatform: failurePlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}}}
	s := &stoppableSession{queuedLifecycleSession: &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 4)}}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	e.SetStreamPreviewCfg(cfg)
	e.taskCards.openDelay = 60 * time.Millisecond
	e.taskCards.minGap = 20 * time.Millisecond
	e.taskCards.tick = 10 * time.Millisecond
	e.taskCards.followGap = gap
	t.Cleanup(func() { _ = e.Stop() })
	env := &taskFollowEnv{t: t, p: p, s: s, e: e, key: "segmented:user"}
	env.send("整理文件")
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "任务甲", Background: yes()})
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "b", Description: "任务乙", Background: yes()})
	env.s.events <- Event{Type: EventResult, Content: "已在后台运行", Done: true}
	waitUntil(t, "task card", func() bool { sent, _ := p.cards(); return len(sent) == 1 })
	env.waitIdle()
	return env
}

func (env *taskFollowEnv) task(ev TaskEvent) { env.s.events <- Event{Type: EventTask, Task: &ev} }

func (env *taskFollowEnv) send(text string) {
	env.t.Helper()
	env.msgs++
	id := fmt.Sprint(env.msgs)
	env.e.ReceiveMessage(env.p, &Message{SessionKey: env.key, Platform: env.p.Name(), MessageID: id, UserID: "user", Content: text, ReplyCtx: "ctx-" + id})
	select {
	case <-env.s.sent:
	case <-time.After(3 * time.Second):
		env.t.Fatalf("prompt %q missing", text)
	}
}

// state is the follow card's count and the number of moves so far.
func (env *taskFollowEnv) state() (below int, moving bool, busy int) {
	m := env.e.taskCards
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.follow[env.key]; c != nil {
		below, moving = c.below, c.moving
	}
	return below, moving, m.busy[env.key]
}

func (env *taskFollowEnv) waitIdle() {
	env.t.Helper()
	waitUntil(env.t, "turn end", func() bool { _, moving, busy := env.state(); return busy == 0 && !moving })
}

// round is one exchange: the person's message, then the agent's reply.
func (env *taskFollowEnv) round() {
	env.t.Helper()
	before := env.replies()
	env.send(fmt.Sprintf("第 %d 句", env.msgs))
	env.s.events <- Event{Type: EventResult, Content: "好的", Done: true}
	waitUntil(env.t, "round done", func() bool {
		_, moving, busy := env.state()
		return busy == 0 && !moving && env.replies() > before
	})
}

// replies counts the agent's replies the platform shows.
func (env *taskFollowEnv) replies() int {
	stub := &env.p.stubPlatformEngine
	stub.mu.Lock()
	defer stub.mu.Unlock()
	n := 0
	for _, s := range stub.sent {
		if strings.Contains(s, "好的") {
			n++
		}
	}
	return n
}

func lastMove(t *testing.T, p *movingTaskCards) taskCardMove {
	t.Helper()
	m := p.moved()
	if len(m) == 0 {
		t.Fatal("no move")
	}
	return m[len(m)-1]
}

// Six rounds after the card opened: it moves after the second round (four
// messages below it) and again after the fourth and sixth, each time replying
// to the latest message; later updates reach the new card.
func TestTaskCardFollowsTheChat(t *testing.T) {
	env := newTaskFollowEnv(t, 0)
	env.round()
	if len(env.p.moved()) != 0 {
		t.Fatal("moved after one round")
	}
	env.round()
	if got := env.p.moved(); len(got) != 1 || got[0].from != "task-card-0" || got[0].rc != "ctx-3" || got[0].view.Moved != "↓ 已移到下方" ||
		got[0].view.State != TaskCardRunning || len(got[0].view.Rows) != 2 {
		t.Fatalf("moves after two rounds: %+v", got)
	}
	for i := 0; i < 4; i++ {
		env.round()
	}
	got := env.p.moved()
	if len(got) != 3 || got[1].from != "moved-0" || got[2].from != "moved-1" || got[2].rc != "ctx-7" {
		t.Fatalf("moves after six rounds: %+v", got)
	}
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "读取文件"})
	waitUntil(t, "update on the moved card", func() bool {
		_, up := env.p.cards()
		return len(up) > 0 && up[len(up)-1].handle == "moved-2"
	})
}

// Moves are at least followGap apart, counted from where the card was posted.
func TestTaskCardFollowWaitsForTheGap(t *testing.T) {
	const gap = 600 * time.Millisecond
	env := newTaskFollowEnv(t, gap)
	env.round()
	env.round()
	env.round()
	if len(env.p.moved()) != 0 {
		t.Fatal("moved within the gap")
	}
	time.Sleep(gap)
	env.round()
	if len(env.p.moved()) != 1 {
		t.Fatalf("moves=%d after the gap, want 1", len(env.p.moved()))
	}
	env.round()
	env.round()
	env.round()
	if len(env.p.moved()) != 1 {
		t.Fatalf("moved again within the gap: %+v", env.p.moved())
	}
}

// Once every task has ended, a card pushed up moves once more to show the
// result, and never again.
func TestSettledTaskCardMovesOnceWhenPushedUp(t *testing.T) {
	env := newTaskFollowEnv(t, time.Hour)
	env.round()
	env.round()
	if len(env.p.moved()) != 0 {
		t.Fatal("live card moved within the gap")
	}
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a", Status: "completed", Summary: "做完了"})
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "b", Status: "failed", Summary: "出错了"})
	waitUntil(t, "final move", func() bool { return len(env.p.moved()) == 1 })
	if m := lastMove(t, env.p); m.view.State != TaskCardFailed || m.from != "task-card-0" {
		t.Fatalf("final move=%+v", m)
	}
	for i := 0; i < 4; i++ {
		env.round()
	}
	time.Sleep(50 * time.Millisecond)
	if got := env.p.moved(); len(got) != 1 {
		t.Fatalf("settled card moved again: %+v", got)
	}
	env.e.taskCards.mu.Lock()
	defer env.e.taskCards.mu.Unlock()
	if env.e.taskCards.follow[env.key] != nil {
		t.Fatal("settled card still followed")
	}
}

// A card still in view when its tasks end stays where it is.
func TestSettledTaskCardInViewStays(t *testing.T) {
	env := newTaskFollowEnv(t, time.Hour)
	env.round()
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a", Status: "completed"})
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "b", Status: "completed"})
	waitUntil(t, "settled", func() bool { return lastView2(env.p).State == TaskCardCompleted })
	env.round()
	env.round()
	time.Sleep(50 * time.Millisecond)
	if got := env.p.moved(); len(got) != 0 {
		t.Fatalf("card in view moved: %+v", got)
	}
}

// A card due its last move while a turn runs moves only after that turn's
// reply, so it never cuts into the streaming turn card.
func TestSettledTaskCardWaitsForTheTurn(t *testing.T) {
	env := newTaskFollowEnv(t, time.Hour)
	env.round()
	env.round()
	env.send("再问一句")
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a", Status: "completed"})
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "b", Status: "completed"})
	waitUntil(t, "settled", func() bool { return lastView2(env.p).State == TaskCardCompleted })
	time.Sleep(50 * time.Millisecond)
	if got := env.p.moved(); len(got) != 0 {
		t.Fatalf("moved during a turn: %+v", got)
	}
	env.s.events <- Event{Type: EventResult, Content: "好的", Done: true}
	waitUntil(t, "move after the turn", func() bool { return len(env.p.moved()) == 1 })
	if m := lastMove(t, env.p); m.rc != "ctx-4" || m.view.State != TaskCardCompleted {
		t.Fatalf("move=%+v", m)
	}
}

// A platform that cannot move cards leaves them where they are.
func TestTaskCardWithoutMoverStays(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "任务甲", Background: yes()})
	env.s.events <- Event{Type: EventResult, Content: "已在后台运行", Done: true}
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	m := env.e.taskCards
	m.mu.Lock()
	c := m.follow[env.key]
	c.below, c.placedAt = 10, time.Now().Add(-time.Hour)
	m.followLocked(env.e, c)
	moving := c.moving
	m.mu.Unlock()
	if moving {
		t.Fatal("move started on a platform without a mover")
	}
}

func lastView2(p *movingTaskCards) TaskCardView {
	sent, up := p.cards()
	if len(up) > 0 {
		return up[len(up)-1].view
	}
	if len(sent) > 0 {
		return sent[len(sent)-1].view
	}
	return TaskCardView{}
}
