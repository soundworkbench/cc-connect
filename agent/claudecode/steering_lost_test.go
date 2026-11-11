package claudecode

import (
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func setSteerGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := steerGrace
	steerGrace = d
	t.Cleanup(func() { steerGrace = old })
}

func nextEvent(t *testing.T, cs *claudeSession, wait time.Duration) (core.Event, bool) {
	t.Helper()
	select {
	case e := <-cs.events:
		return e, true
	case <-time.After(wait):
		return core.Event{}, false
	}
}

// A steered message that the CLI never replays after the native result must
// not leave the turn open forever: it is written once more as a normal input.
func TestClaudeSteer_UnreplayedSteerAfterResultIsResent(t *testing.T) {
	setSteerGrace(t, 80*time.Millisecond)
	cs, ch := steerFixture(t)
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	first := takeInput(t, ch)
	ack(cs, first)
	receipt, err := cs.SteerAsync("second", "m2", nil, nil)
	if err != nil || receipt == nil {
		t.Fatalf("steer not admitted: %v", err)
	}
	second := takeInput(t, ch)
	finish(cs, first["uuid"])
	if e, _ := nextEvent(t, cs, time.Second); e.Done {
		t.Fatal("result closed the turn while a steer was unaccounted for")
	}
	// No replay for `second`. The adapter must write it again with a new uuid.
	third := takeInput(t, ch)
	if third["uuid"] == second["uuid"] {
		t.Fatal("resent frame reused the old uuid")
	}
	if got := third["message"].(map[string]any)["content"]; got != "second" {
		t.Fatalf("resent content = %v", got)
	}
	ack(cs, third)
	select {
	case <-receipt.Ack:
	case <-time.After(time.Second):
		t.Fatal("original receipt not acknowledged by the resent input")
	}
	finish(cs, third["uuid"])
	if e, ok := nextEvent(t, cs, time.Second); !ok || !e.Done {
		t.Fatal("turn did not finish on the resent input's result")
	}
}

// A second silent window is reported instead of waiting for the 2 hour idle kill.
func TestClaudeSteer_UnreplayedSteerGivesUpWithError(t *testing.T) {
	setSteerGrace(t, 60*time.Millisecond)
	cs, ch := steerFixture(t)
	cs.Send("first", "m1", nil, nil)
	first := takeInput(t, ch)
	ack(cs, first)
	if r, err := cs.SteerAsync("second", "m2", nil, nil); err != nil || r == nil {
		t.Fatal("steer not admitted")
	}
	takeInput(t, ch)
	finish(cs, first["uuid"])
	nextEvent(t, cs, time.Second) // non-terminal result
	takeInput(t, ch)              // the single resend
	e, ok := nextEvent(t, cs, 2*time.Second)
	if !ok || e.Type != core.EventError {
		t.Fatalf("expected an error event, got %+v ok=%v", e, ok)
	}
	if got := e.Error.Error(); got != "这条消息没有送达，请再发一次" {
		t.Fatalf("notice = %q", got)
	}
}

func lifecycle(cs *claudeSession, m map[string]any, state string) {
	cs.handleReadLoopLine(`{"type":"command_lifecycle","command_uuid":"` + m["uuid"].(string) + `","state":"` + state + `"}`)
}

// The CLI holds a queued steer until the running turn ends, however long that
// takes. Silence from a long model call must neither resend it nor report it lost.
func TestClaudeSteer_QueuedSteerIsNeverResentOrReportedLost(t *testing.T) {
	setSteerGrace(t, 60*time.Millisecond)
	cs, ch := steerFixture(t)
	cs.Send("first", "m1", nil, nil)
	first := takeInput(t, ch)
	lifecycle(cs, first, "queued")
	lifecycle(cs, first, "started")
	receipt, err := cs.SteerAsync("second", "m2", nil, nil)
	if err != nil || receipt == nil {
		t.Fatalf("steer not admitted: %v", err)
	}
	second := takeInput(t, ch)
	lifecycle(cs, second, "queued")
	finish(cs, first["uuid"])
	if e, _ := nextEvent(t, cs, time.Second); e.Done {
		t.Fatal("result closed the turn while a queued steer was pending")
	}
	// The CLI starts the queued frame as the next turn, then stays silent for
	// several grace windows (a stalled model call).
	lifecycle(cs, second, "started")
	select {
	case <-receipt.Ack:
	case <-time.After(time.Second):
		t.Fatal("steer not acknowledged when the CLI started it")
	}
	time.Sleep(250 * time.Millisecond)
	select {
	case m := <-ch:
		t.Fatalf("a frame the CLI already took was written again: %v", m)
	default:
	}
	if e, ok := nextEvent(t, cs, 50*time.Millisecond); ok {
		t.Fatalf("unexpected event while the CLI was busy: %+v", e)
	}
	finish(cs, second["uuid"])
	if e, ok := nextEvent(t, cs, time.Second); !ok || !e.Done {
		t.Fatal("turn did not finish on the steer's own result")
	}
}

// A frame that is still only queued when the CLI goes quiet stays untouched too.
func TestClaudeSteer_QueuedButNotStartedStaysQuiet(t *testing.T) {
	setSteerGrace(t, 50*time.Millisecond)
	cs, ch := steerFixture(t)
	cs.Send("first", "m1", nil, nil)
	first := takeInput(t, ch)
	lifecycle(cs, first, "started")
	r, _ := cs.SteerAsync("second", "m2", nil, nil)
	second := takeInput(t, ch)
	lifecycle(cs, second, "queued")
	finish(cs, first["uuid"])
	nextEvent(t, cs, time.Second)
	time.Sleep(200 * time.Millisecond)
	select {
	case m := <-ch:
		t.Fatalf("queued frame written again: %v", m)
	default:
	}
	if e, ok := nextEvent(t, cs, 50*time.Millisecond); ok {
		t.Fatalf("unexpected event: %+v", e)
	}
	select {
	case <-r.Ack:
		t.Fatal("acknowledged before the CLI started it")
	default:
	}
}

// Activity from the CLI (for example a notification turn) postpones the resend.
func TestClaudeSteer_ActivityPostponesResend(t *testing.T) {
	setSteerGrace(t, 150*time.Millisecond)
	cs, ch := steerFixture(t)
	cs.Send("first", "m1", nil, nil)
	first := takeInput(t, ch)
	ack(cs, first)
	r, _ := cs.SteerAsync("second", "m2", nil, nil)
	second := takeInput(t, ch)
	finish(cs, first["uuid"])
	nextEvent(t, cs, time.Second)
	for i := 0; i < 4; i++ {
		time.Sleep(60 * time.Millisecond)
		cs.handleReadLoopLine(`{"type":"system","subtype":"status"}`)
	}
	select {
	case m := <-ch:
		t.Fatalf("resent while the CLI was active: %v", m)
	default:
	}
	ack(cs, second)
	<-r.Ack
	finish(cs, second["uuid"])
	if e, _ := nextEvent(t, cs, time.Second); !e.Done {
		t.Fatal("not terminal")
	}
}
