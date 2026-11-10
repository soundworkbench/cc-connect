package core

import (
	"context"
	"path/filepath"
	"sync"
	"strings"
	"testing"
	"time"
)

// A person's burst while the assistant is idle becomes one turn, without
// "queued" notices for the follow-ups.
func TestDebounce_IdleHumanBurstBecomesOneTurn(t *testing.T) {
	e, p, s := batchEngine(t)
	cfg := DefaultQueueBatchConfig()
	cfg.Debounce = 250 * time.Millisecond
	e.SetQueueBatchConfig(cfg)
	for i := 1; i <= 3; i++ {
		e.ReceiveMessage(p, batchMessage(i))
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case got := <-s.deliveries:
		t.Fatalf("turn started before the window closed: %q", got.prompt)
	default:
	}
	got := cc27Take(t, s.deliveries)
	for _, want := range []string{"内容1", "内容2", "内容3"} {
		if !strings.Contains(got.prompt, want) {
			t.Fatalf("merged turn lacks %s: %q", want, got.prompt)
		}
	}
	s.events <- Event{Type: EventResult, Content: "done", Done: true}
	cc27WaitIdle(t, e, "test:user")
	if len(s.deliveries) != 0 {
		t.Fatal("follow-up started its own turn")
	}
	for _, sent := range p.getSent() {
		if sent == e.i18n.T(MsgMessageQueued) {
			t.Fatal("queued notice shown during debounce")
		}
	}
}

// A lone message waits only for the window.
func TestDebounce_SingleMessageIsUnchanged(t *testing.T) {
	e, p, s := batchEngine(t)
	cfg := DefaultQueueBatchConfig()
	cfg.Debounce = 100 * time.Millisecond
	e.SetQueueBatchConfig(cfg)
	e.ReceiveMessage(p, batchMessage(1))
	got := cc27Take(t, s.deliveries)
	if got.prompt != "内容1" {
		t.Fatalf("prompt=%q", got.prompt)
	}
}

// With the window at zero, behaviour is as before: each message acts at once.
func TestDebounce_ZeroDisables(t *testing.T) {
	e, p, s := batchEngine(t)
	cfg := DefaultQueueBatchConfig()
	cfg.Debounce = 0
	e.SetQueueBatchConfig(cfg)
	e.ReceiveMessage(p, batchMessage(1))
	if got := cc27Take(t, s.deliveries); got.prompt != "内容1" {
		t.Fatalf("prompt=%q", got.prompt)
	}
}

// Slash commands and urgent input neither wait nor join the burst.
func TestDebounce_ControlInputSkipsWindow(t *testing.T) {
	e, _, _ := batchEngine(t)
	cfg := DefaultQueueBatchConfig()
	cfg.Debounce = time.Hour
	e.SetQueueBatchConfig(cfg)
	for _, m := range []*Message{
		{UserID: "person", Content: "/stop"},
		{UserID: "person", Content: "!ls"},
		{UserID: "person", Content: "hi", Urgent: true},
		{UserID: "person", Content: "allow", IsPermissionResponse: true},
		{UserID: "person", Content: "x", individualReply: true},
	} {
		if e.shouldDebounce(m) {
			t.Fatalf("%q must not be debounced", m.Content)
		}
	}
	if !e.shouldDebounce(&Message{UserID: "person", Content: "hello"}) {
		t.Fatal("plain human text must be debounced")
	}
	if e.shouldDebounce(&Message{UserID: "timer", Content: "tick"}) {
		t.Fatal("automation must not use the human window")
	}
}

type typingBatchPlatform struct {
	*batchPlatform
	mu     sync.Mutex
	starts []any
	stops  []any
}

func (p *typingBatchPlatform) StartTyping(_ context.Context, rc any) func() {
	p.mu.Lock()
	p.starts = append(p.starts, rc)
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		p.stops = append(p.stops, rc)
		p.mu.Unlock()
	}
}

func (p *typingBatchPlatform) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.starts), len(p.stops)
}

func typingDebounceEngine(t *testing.T, window time.Duration) (*Engine, *typingBatchPlatform, *batchSession) {
	t.Helper()
	bp := &batchPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, routes: make(chan any, 100), merged: make(chan any, 100)}
	p := &typingBatchPlatform{batchPlatform: bp}
	s := &batchSession{cujAgentSession: newCUJAgentSession(), deliveries: make(chan batchDelivery, 100)}
	e := NewEngine("batch", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.SetDataDir(t.TempDir())
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	t.Cleanup(func() { e.cancel(); _ = e.Stop() })
	cfg := DefaultQueueBatchConfig()
	cfg.Debounce = window
	e.SetQueueBatchConfig(cfg)
	return e, p, s
}

// The reaction appears when the window opens, and the turn that follows reuses
// it instead of adding a second one.
func TestDebounce_TypingStartsAtOnceAndIsReused(t *testing.T) {
	e, p, s := typingDebounceEngine(t, 300*time.Millisecond)
	e.ReceiveMessage(p, batchMessage(1))
	if starts, _ := p.counts(); starts != 1 {
		t.Fatalf("starts right after arrival = %d, want 1", starts)
	}
	select {
	case got := <-s.deliveries:
		t.Fatalf("turn started before the window closed: %q", got.prompt)
	default:
	}
	cc27Take(t, s.deliveries)
	if starts, stops := p.counts(); starts != 1 || stops != 0 {
		t.Fatalf("turn start: starts=%d stops=%d, want 1/0", starts, stops)
	}
	s.events <- Event{Type: EventResult, Content: "done", Done: true}
	cc27WaitIdle(t, e, "test:user")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, stops := p.counts(); stops == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if starts, stops := p.counts(); starts != 1 || stops != 1 {
		t.Fatalf("turn end: starts=%d stops=%d, want 1/1", starts, stops)
	}
}

// A merged turn carries its reaction on the last message; the first message's
// window reaction is stopped, not left spinning.
func TestDebounce_TypingMergedBurstLeavesNothingRunning(t *testing.T) {
	e, p, s := typingDebounceEngine(t, 200*time.Millisecond)
	e.ReceiveMessage(p, batchMessage(1))
	e.ReceiveMessage(p, batchMessage(2))
	cc27Take(t, s.deliveries)
	s.events <- Event{Type: EventResult, Content: "done", Done: true}
	cc27WaitIdle(t, e, "test:user")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		starts, stops := p.counts()
		if starts == stops {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	starts, stops := p.counts()
	t.Fatalf("starts=%d stops=%d", starts, stops)
}

// Cancelling during the window stops the reaction.
func TestDebounce_TypingStoppedOnCancel(t *testing.T) {
	e, p, s := typingDebounceEngine(t, time.Hour)
	e.ReceiveMessage(p, batchMessage(1))
	if starts, _ := p.counts(); starts != 1 {
		t.Fatalf("starts=%d", starts)
	}
	e.cleanupInteractiveState("test:user")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, stops := p.counts(); stops == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, stops := p.counts(); stops != 1 {
		t.Fatalf("stops=%d, want 1", stops)
	}
	if len(s.deliveries) != 0 {
		t.Fatal("cancelled window started a turn")
	}
}
