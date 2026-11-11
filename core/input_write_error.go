package core

import (
	"errors"
	"fmt"
)

// InputWriteError means the input stream is unusable and its process must restart.
// Byte counts describe transport progress, not acknowledgement by the agent.
type InputWriteError struct {
	WrittenBytes int
	TotalBytes   int
	Err          error
}

func (e *InputWriteError) Error() string {
	if errors.Is(e.Err, ErrPermissionChannelClosed) {
		return e.Err.Error()
	}
	return fmt.Sprintf("agent stdin write failed (%d/%d bytes): %v", e.WrittenBytes, e.TotalBytes, e.Err)
}

func (e *InputWriteError) Unwrap() error { return e.Err }

// ErrPermissionChannelClosed is the Err of an InputWriteError raised when the
// agent process reports that it can no longer read tool permission answers:
// every tool call it makes from then on fails, so it must restart.
var ErrPermissionChannelClosed = errors.New("agent lost its tool permission channel")

// InputFailureReporter preserves a write failure even if events close before
// the foreground turn can observe it, including writes of control responses.
type InputFailureReporter interface {
	InputFailure() *InputWriteError
}

// BackgroundWorkReporter is implemented by agent sessions that know whether a
// task they started (a subagent, a background command) is still running after
// the turn's result. Such a session must not be reclaimed: closing it would
// cut the task off mid-work.
type BackgroundWorkReporter interface {
	HasBackgroundWork() bool
}

func hasBackgroundWork(as AgentSession) bool {
	r, ok := as.(BackgroundWorkReporter)
	return ok && r.HasBackgroundWork()
}
