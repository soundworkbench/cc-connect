package core

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
		if e.ctx.Err() != nil || !session.TryLock() {
			return false
		}
		e.interactiveMu.Lock()
		current := e.interactiveStates[key]
		e.interactiveMu.Unlock()
		state.mu.Lock()
		valid := current == state && !state.stopped && !state.eventsNeedResync && state.agentSession == as && as.Alive() && len(state.pendingMessages) == 0
		if valid {
			state.backgroundTurnLease = true
		}
		state.mu.Unlock()
		if !valid {
			session.UnlockWithoutUpdate()
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
	stopped := state.stopped
	pending := len(state.pendingMessages) > 0
	agent := state.agent
	state.mu.Unlock()
	session.UnlockWithoutUpdate()
	if agent == nil {
		agent = e.agent
	}
	// Never run the drain inline inside the unsolicited reader: it hands off that
	// reader's event channel, and must be able to wait for this goroutine to exit.
	if !stopped && pending && session.TryLock() {
		go e.drainOrphanedQueue(session, sessions, key, agent, workspaceDir)
	}
}

// On reader cancellation/teardown, unlock but never dispatch queued work.
func (e *Engine) abandonBackgroundTurnLease(state *interactiveState, session *Session) {
	state.mu.Lock()
	owned := state.backgroundTurnLease
	state.backgroundTurnLease = false
	state.mu.Unlock()
	if owned {
		session.UnlockWithoutUpdate()
	}
}
