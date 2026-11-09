package core

import "testing"

// Commands, shell shortcuts and permission clicks are never typed into the
// running turn, whatever the batching setting.
func TestSteer_ControlInputIsNotInjected(t *testing.T) {
	for _, batching := range []bool{true, false} {
		e, s, p, state, session := steerQueueFixture(t)
		e.SetQueueBatchConfig(QueueBatchConfig{Enabled: batching, MaxMessages: 20, MaxChars: 20000})
		close(s.ready)
		for _, msg := range []*Message{
			{MessageID: "a", Content: "/model", individualReply: true},
			{MessageID: "b", Content: "/model"},
			{MessageID: "c", Content: "!ls"},
			{MessageID: "d", Content: "allow", IsPermissionResponse: true},
		} {
			state.mu.Lock()
			steered := e.trySteerMessageLocked(state, p, msg, session, e.sessions)
			state.mu.Unlock()
			if steered {
				t.Fatalf("batching=%v: %q was injected into the running turn", batching, msg.Content)
			}
		}
		select {
		case got := <-s.steered:
			t.Fatalf("batching=%v: agent received %q", batching, got)
		default:
		}
	}
}
