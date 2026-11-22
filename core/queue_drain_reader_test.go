package core

import (
	"fmt"
	"testing"
	"time"
)

// A message queued just as a turn ends is drained while the idle unsolicited
// reader is already running. The queued turn must get its own result back:
// if the reader took it, the turn would wait forever with the session locked
// and every later message would stay queued.
func TestQueuedTurnAfterReaderStartGetsItsResult(t *testing.T) {
	env := newTaskFollowEnv(t, time.Hour)
	e := env.e
	for i := 0; i < 20; i++ {
		e.interactiveMu.Lock()
		state := e.interactiveStates[env.key]
		e.interactiveMu.Unlock()
		state.mu.Lock()
		readerRunning := state.unsolicitedCancel != nil
		state.mu.Unlock()
		if !readerRunning {
			t.Fatalf("round %d: no unsolicited reader while idle", i)
		}

		// Hold the session as the ending turn does, so the next message queues.
		session := e.sessions.GetOrCreateActive(env.key)
		gen, ok := session.TryLock()
		if !ok {
			t.Fatalf("round %d: session busy", i)
		}
		env.msgs++
		id := fmt.Sprint(env.msgs)
		e.ReceiveMessage(env.p, &Message{SessionKey: env.key, Platform: env.p.Name(), MessageID: id, UserID: "user", Content: "第 " + id + " 句", ReplyCtx: "ctx-" + id})

		drained := make(chan bool, 1)
		go func() { drained <- e.drainPendingMessages(state, session, e.sessions, env.key, gen) }()
		select {
		case <-env.s.sent:
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: queued prompt not sent", i)
		}
		env.s.events <- Event{Type: EventResult, Content: "好的", Done: true}
		select {
		case unlocked := <-drained:
			if !unlocked {
				t.Fatalf("round %d: drain did not release the session", i)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: queued turn never got its result", i)
		}
	}
}
