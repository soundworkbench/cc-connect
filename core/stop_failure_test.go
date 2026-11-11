package core

import (
	"testing"
	"time"
)

// /stop marks the turn stopped and kills the process. When the closed event
// stream wins the race against the stop signal, the turn must not settle a
// failure card for an exit the person asked for.
func TestStoppedTurnWhoseProcessExitsShowsNoFailure(t *testing.T) {
	f := newFailureEnv(t)
	f.send("1", "整理客户名单")
	f.e.interactiveMu.Lock()
	state := f.e.interactiveStates[f.key]
	f.e.interactiveMu.Unlock()
	if state == nil {
		t.Fatal("no interactive state")
	}
	// The stop flag is set but its signal is not closed, so the turn loop
	// sees only the closed event stream, as when select picks it first.
	state.mu.Lock()
	state.stopped = true
	state.mu.Unlock()
	close(f.s.events)
	f.waitIdle()
	time.Sleep(50 * time.Millisecond)
	if _, failures := f.p.snapshot(); len(failures) != 0 {
		t.Fatalf("stopped turn settled a failure card: %+v", failures)
	}
}
