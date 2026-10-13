package core

import "fmt"

// InputWriteError means the input stream is unusable and its process must restart.
// Byte counts describe transport progress, not acknowledgement by the agent.
type InputWriteError struct {
	WrittenBytes int
	TotalBytes   int
	Err          error
}

func (e *InputWriteError) Error() string {
	return fmt.Sprintf("agent stdin write failed (%d/%d bytes): %v", e.WrittenBytes, e.TotalBytes, e.Err)
}

func (e *InputWriteError) Unwrap() error { return e.Err }

// InputFailureReporter preserves a write failure even if events close before
// the foreground turn can observe it, including writes of control responses.
type InputFailureReporter interface {
	InputFailure() *InputWriteError
}
