package core

import (
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
