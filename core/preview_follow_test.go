package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type movePlatform struct {
	failurePlatform
	mmu   sync.Mutex
	moves []any // reply contexts the card moved under
	times []time.Time
}

func (p *movePlatform) MovePreview(_ context.Context, _ any, rc any) error {
	p.mmu.Lock()
	defer p.mmu.Unlock()
	p.moves = append(p.moves, rc)
	p.times = append(p.times, time.Now())
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

func newFollowEnv(t *testing.T, settle, gap time.Duration) (*movePlatform, *steeringSession, *Engine, func(id string)) {
	t.Helper()
	oldSettle, oldGap := previewMoveSettle, previewMoveGap
	previewMoveSettle, previewMoveGap = settle, gap
	t.Cleanup(func() { previewMoveSettle, previewMoveGap = oldSettle, oldGap })
	p := &movePlatform{failurePlatform: failurePlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}}
	s := &steeringSession{queuedLifecycleSession: &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 4)}, steered: make(chan string, 8)}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	e.SetStreamPreviewCfg(cfg)
	t.Cleanup(func() { _ = e.Stop() })
	send := func(id string) {
		e.ReceiveMessage(p, &Message{SessionKey: "segmented:user", Platform: p.Name(), MessageID: id, UserID: "user", Content: "消息 " + id, ReplyCtx: "ctx-" + id})
	}
	send("1")
	select {
	case <-s.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt missing")
	}
	return p, s, e, send
}

func steerOne(t *testing.T, s *steeringSession) time.Time {
	t.Helper()
	select {
	case <-s.steered:
		return time.Now()
	case <-time.After(3 * time.Second):
		t.Fatal("message not steered into the turn")
	}
	return time.Time{}
}

// The burst from the 11-11 test: three messages 1.3 s and 1.6 s apart, with
// the production wait. The card moves once, after the burst, under the last.
func TestBurstOfMessagesMovesTheCardOnceUnderTheLast(t *testing.T) {
	p, s, _, send := newFollowEnv(t, 2500*time.Millisecond, 0)
	send("2")
	steerOne(t, s)
	time.Sleep(1300 * time.Millisecond)
	send("3")
	steerOne(t, s)
	time.Sleep(1600 * time.Millisecond)
	if got := p.moved(); len(got) != 0 {
		t.Fatalf("moved during the burst: %v", got)
	}
	send("4")
	last := steerOne(t, s)
	for deadline := time.Now().Add(8 * time.Second); len(p.moved()) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("card never moved")
		}
	}
	p.mmu.Lock()
	at := p.times[0]
	p.mmu.Unlock()
	if at.Sub(last) < 2*time.Second {
		t.Fatalf("moved %v after the last message, before the burst settled", at.Sub(last))
	}
	time.Sleep(time.Second)
	if got := p.moved(); len(got) != 1 || got[0] != "ctx-4" {
		t.Fatalf("moves=%v, want one move under the last message", got)
	}
	s.events <- Event{Type: EventResult, Content: "好了", Done: true}
}

// The messages from the second 11-11 test at +13.7 s, +18.7 s and +25.4 s,
// with every wait scaled down 10 times: the gaps outlast the settle wait, yet
// the card moves only once.
func TestCardMovesOncePerTurn(t *testing.T) {
	p, s, _, send := newFollowEnv(t, 250*time.Millisecond, time.Second)
	oldAgain := previewMoveAgain
	previewMoveAgain = 6 * time.Second
	t.Cleanup(func() { previewMoveAgain = oldAgain })
	start := time.Now()
	for _, m := range []struct {
		id string
		at time.Duration
	}{{"2", 1370 * time.Millisecond}, {"3", 1870 * time.Millisecond}, {"4", 2540 * time.Millisecond}} {
		time.Sleep(time.Until(start.Add(m.at)))
		send(m.id)
		steerOne(t, s)
	}
	time.Sleep(time.Until(start.Add(4 * time.Second)))
	if got := p.moved(); len(got) != 1 || got[0] != "ctx-2" {
		t.Fatalf("moves=%v, want one move under the first message", got)
	}
	s.events <- Event{Type: EventResult, Content: "好了", Done: true}
}

// Once previewMoveAgain has passed since the move, a new message moves the
// card again.
func TestCardMovesAgainAfterTheRepeatGap(t *testing.T) {
	p, s, _, send := newFollowEnv(t, 20*time.Millisecond, 0)
	oldAgain := previewMoveAgain
	previewMoveAgain = 400 * time.Millisecond
	t.Cleanup(func() { previewMoveAgain = oldAgain })
	send("2")
	steerOne(t, s)
	waitUntil(t, "first move", func() bool { return len(p.moved()) == 1 })
	send("3")
	steerOne(t, s)
	time.Sleep(200 * time.Millisecond)
	if got := p.moved(); len(got) != 1 {
		t.Fatalf("moved again within the repeat gap: %v", got)
	}
	time.Sleep(300 * time.Millisecond)
	send("4")
	steerOne(t, s)
	waitUntil(t, "second move", func() bool { return len(p.moved()) == 2 })
	if got := p.moved(); got[1] != "ctx-4" {
		t.Fatalf("moves=%v", got)
	}
	s.events <- Event{Type: EventResult, Content: "好了", Done: true}
}

func TestCardStaysPutRightAfterItAppears(t *testing.T) {
	p, s, _, send := newFollowEnv(t, 20*time.Millisecond, 400*time.Millisecond)
	start := time.Now()
	send("2")
	steerOne(t, s)
	waitUntil(t, "card move", func() bool { return len(p.moved()) == 1 })
	p.mmu.Lock()
	at := p.times[0]
	p.mmu.Unlock()
	if at.Sub(start) < 300*time.Millisecond {
		t.Fatalf("moved %v after the card appeared", at.Sub(start))
	}
	s.events <- Event{Type: EventResult, Content: "好了", Done: true}
}

func TestFinishedTurnDoesNotMoveItsCard(t *testing.T) {
	p, s, e, send := newFollowEnv(t, 100*time.Millisecond, 0)
	send("2")
	steerOne(t, s)
	s.events <- Event{Type: EventResult, Content: "好了", Done: true}
	deadline := time.Now().Add(3 * time.Second)
	for e.sessions.GetOrCreateActive("segmented:user").Busy() {
		if time.Now().After(deadline) {
			t.Fatal("turn hung")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if got := p.moved(); len(got) != 0 {
		t.Fatalf("a finished card moved: %v", got)
	}
}

func TestCardEventsReachTheTrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := SetStreamTraceFile(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetStreamTraceFile("") })
	cardEvent("card_move", "feishu:oc:u", "om_new", "from", "om_old", "reason", "user_message_below")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := string(data)
	for _, want := range []string{`"card_move"`, `"session":"feishu:oc:u"`, `"card":"om_new"`, `"from":"om_old"`, `"reason":"user_message_below"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("trace lacks %s: %s", want, line)
		}
	}
}
