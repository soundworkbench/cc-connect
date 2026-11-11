package core

import (
	"log/slog"
	"time"
)

// BackgroundTurnGated sessions ask the engine to reserve the same per-session
// execution slot used by user turns before starting an autonomous turn.
// The reservation is released only after its final event has been consumed.
type BackgroundTurnGated interface{ SetBackgroundTurnGate(func() bool) }

func (e *Engine) installBackgroundTurnGate(state *interactiveState, session *Session, key string) {
	state.mu.Lock()
	as := state.agentSession
	state.mu.Unlock()
	gated, ok := as.(BackgroundTurnGated)
	if !ok {
		return
	}
	gated.SetBackgroundTurnGate(func() bool {
		if e.ctx.Err() != nil {
			return false
		}
		gen, locked := session.TryLock()
		if !locked {
			return false
		}
		e.interactiveMu.Lock()
		current := e.interactiveStates[key]
		e.interactiveMu.Unlock()
		state.mu.Lock()
		valid := current == state && !state.stopped && !state.eventsNeedResync && state.agentSession == as && as.Alive() && len(state.pendingMessages) == 0
		if valid {
			state.backgroundTurnLease = true
			state.backgroundTurnLeaseGen = gen
		}
		state.mu.Unlock()
		if !valid {
			session.UnlockWithoutUpdate(gen)
			return false
		}
		e.cancelAgentSessionIdleClose(state)
		return true
	})
}

func (e *Engine) releaseBackgroundTurnLease(state *interactiveState, session *Session, sessions *SessionManager, key, workspaceDir string) {
	state.mu.Lock()
	if !state.backgroundTurnLease {
		state.mu.Unlock()
		return
	}
	state.backgroundTurnLease = false
	gen := state.backgroundTurnLeaseGen
	stopped := state.stopped
	pending := len(state.pendingMessages) > 0
	agent := state.agent
	state.mu.Unlock()
	session.UnlockWithoutUpdate(gen)
	if agent == nil {
		agent = e.agent
	}
	// Never run the drain inline inside the unsolicited reader: it hands off that
	// reader's event channel, and must be able to wait for this goroutine to exit.
	if stopped || !pending {
		return
	}
	if next, ok := session.TryLock(); ok {
		go e.drainOrphanedQueue(session, sessions, key, agent, workspaceDir, next)
	}
}

// On reader cancellation/teardown, unlock but never dispatch queued work.
func (e *Engine) abandonBackgroundTurnLease(state *interactiveState, session *Session) {
	state.mu.Lock()
	owned := state.backgroundTurnLease
	gen := state.backgroundTurnLeaseGen
	state.backgroundTurnLease = false
	state.mu.Unlock()
	if owned {
		session.UnlockWithoutUpdate(gen)
	}
}

// backgroundWorkPoll is how often awaitBackgroundWork looks at the session.
var backgroundWorkPoll = time.Second

// defaultBackgroundWorkLimit bounds awaitBackgroundWork when max_turn_time is off.
const defaultBackgroundWorkLimit = 30 * time.Minute

func (e *Engine) backgroundWorkLimit() time.Duration {
	if e.maxTurnTime > 0 {
		return e.maxTurnTime
	}
	return defaultBackgroundWorkLimit
}

// awaitBackgroundWork keeps a one-shot session (a cron run) open while tasks
// the agent started during the turn are still running. Closing it at the
// turn's result cut those tasks off: their tool calls failed until the
// process was killed. The unsolicited reader keeps relaying the tasks' output
// and permission requests meanwhile.
func (e *Engine) awaitBackgroundWork(key string, limit time.Duration) {
	deadline := time.Now().Add(limit)
	waiting := false
	for {
		e.interactiveMu.Lock()
		state := e.interactiveStates[key]
		e.interactiveMu.Unlock()
		if state == nil {
			return
		}
		state.mu.Lock()
		as, stopped := state.agentSession, state.stopped
		state.mu.Unlock()
		if stopped || as == nil || !as.Alive() || !hasBackgroundWork(as) {
			if waiting {
				slog.Info("background work ended; closing session", "session_key", key)
			}
			return
		}
		if !waiting {
			waiting = true
			slog.Info("background work still running; keeping session until it ends", "session_key", key, "limit", limit)
		}
		if time.Now().After(deadline) {
			slog.Warn("background work still running at limit; closing session", "session_key", key, "limit", limit)
			return
		}
		select {
		case <-e.ctx.Done():
			return
		case <-time.After(backgroundWorkPoll):
		}
	}
}
