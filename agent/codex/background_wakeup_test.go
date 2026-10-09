package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestBackgroundWakeup_AfterParentFinalDoesNotFinishParent(t *testing.T) {
	s := &appServerSession{events: make(chan core.Event, 8), completedTurn: "old"}
	s.threadID.Store("parent")
	s.wakeup.enabled = true
	s.wakeup.userInput("continue authorized task", time.Now())
	s.handleNotification("item/completed", json.RawMessage(`{"threadId":"parent","turnId":"old","item":{"type":"subAgentActivity","kind":"completed","id":"event-1","agentThreadId":"child"}}`))
	if len(s.wakeup.pending) != 1 {
		t.Fatal("parent-scoped completion after final was lost")
	}
	if len(s.events) != 0 || s.currentTurn != "" {
		t.Fatal("completion was misreported as parent output/turn")
	}
	s.handleNotification("turn/completed", json.RawMessage(`{"threadId":"child","turn":{"id":"child-turn","status":"completed"}}`))
	if len(s.events) != 0 {
		t.Fatal("child turn escaped isolation")
	}
}

func TestBackgroundWakeup_TwoBatchesDedupBusyStopAndDeadline(t *testing.T) {
	now := time.Now()
	w := backgroundWakeup{enabled: true}
	w.userInput("continue task", now)
	w.observe("e1", "c1", now)
	w.observe("e1", "c1", now)
	w.observe("e2", "c2", now)
	if len(w.pending) != 2 {
		t.Fatal("dedup failed")
	}
	if w.take(now.Add(3*time.Second), true) {
		t.Fatal("woke busy parent")
	}
	if !w.take(now.Add(3*time.Second), false) {
		t.Fatal("first batch not woken")
	}
	w.inFlight = false
	w.observe("e3", "c3", now.Add(4*time.Second))
	if !w.take(now.Add(7*time.Second), false) || w.used != 2 {
		t.Fatal("second batch not woken")
	}
	w.userInput("停止", now.Add(8*time.Second))
	w.observe("e4", "c4", now.Add(9*time.Second))
	if w.take(now.Add(12*time.Second), false) {
		t.Fatal("late completion restarted stopped work")
	}
	w.userInput("continue task", now)
	w.observe("e5", "c5", now)
	if w.take(now.Add(2*time.Hour), false) {
		t.Fatal("expired lease restarted")
	}
	w.userInput("continue task", now)
	w.used = 10
	w.observe("e6", "c6", now)
	if w.take(now.Add(3*time.Second), false) {
		t.Fatal("wake budget exceeded")
	}
}

func TestBackgroundWakeup_UnrelatedAndDisabledEventsIgnored(t *testing.T) {
	s := &appServerSession{events: make(chan core.Event, 8)}
	s.threadID.Store("parent")
	event := json.RawMessage(`{"threadId":"other","item":{"type":"subAgentActivity","kind":"completed","id":"x","agentThreadId":"child"}}`)
	s.wakeup.enabled = true
	s.wakeup.userInput("continue", time.Now())
	s.handleNotification("item/completed", event)
	if len(s.wakeup.pending) != 0 {
		t.Fatal("unrelated completion accepted")
	}
	s.wakeup.disable()
	s.observeBackgroundCompletion("item/completed", json.RawMessage(`{"threadId":"parent","item":{"type":"subAgentActivity","kind":"completed","id":"x","agentThreadId":"child"}}`))
	if len(s.wakeup.pending) != 0 {
		t.Fatal("disabled wake accepted")
	}
}

func TestBackgroundWakeup_ConfigBounds(t *testing.T) {
	p, err := parseBackgroundWakeupPolicy(map[string]any{"background_wakeup": true, "background_wakeup_max_turns": int64(3), "background_wakeup_timeout_mins": 5})
	if err != nil || !p.enabled || p.maxTurns != 3 || p.lifetime != 5*time.Minute {
		t.Fatal("policy not applied")
	}
	for _, v := range []any{0, 101, "10"} {
		if _, err := parseBackgroundWakeupPolicy(map[string]any{"background_wakeup_max_turns": v}); err == nil {
			t.Fatal("invalid budget accepted")
		}
	}
}
