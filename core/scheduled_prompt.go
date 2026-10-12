package core

import (
	"fmt"
	"log/slog"
	"time"
)

// Returns false with the session lock owned by the caller, or true once the
// prompt belongs to the shared FIFO. Scheduler success means accepted for
// processing, not a separately collected model result.
func (e *Engine) admitScheduledPrompt(p Platform, msg *Message, session *Session, sessions *SessionManager, agent Agent, key, workspace string) (bool, error) {
	e.ensureInteractiveStateForQueueing(key, p, msg.ReplyCtx, session)
	e.setQueuedWorkspace(key, workspace)
	for {
		if !msg.admissionDeadline.IsZero() && time.Now().After(msg.admissionDeadline) {
			return false, fmt.Errorf("local assistant admission timed out")
		}
		if e.isShuttingDown() {
			return false, fmt.Errorf("engine is shutting down")
		}
		if e.ctx.Err() != nil {
			return false, e.ctx.Err()
		}
		if session.TryLock() {
			e.interactiveMu.Lock()
			state := e.interactiveStates[key]
			if state == nil {
				state = &interactiveState{platform: p, replyCtx: msg.ReplyCtx, eventsNeedResync: true}
				e.interactiveStates[key] = state
			}
			state.mu.Lock()
			if len(state.pendingMessages) > 0 {
				state.mu.Unlock()
				e.interactiveMu.Unlock()
				handled, err := e.admitBusyMessage(p, msg, key, session, sessions)
				if err != nil || !handled {
					session.Unlock()
					if err != nil {
						return true, err
					}
					return true, fmt.Errorf("queue admission failed")
				}
				if !e.launchAcceptedWork(func() { e.drainOrphanedQueue(session, sessions, key, agent, workspace) }) {
					session.Unlock()
				}
				return true, nil
			}
			if state.steerMessageIDs[msg.scheduledInputID] {
				state.mu.Unlock()
				e.interactiveMu.Unlock()
				session.Unlock()
				return true, nil
			}
			if e.shouldHold(msg) {
				state.mu.Unlock()
				e.interactiveMu.Unlock()
				held, err := e.holdIdleMessage(p, msg, session, sessions, agent, key, workspace)
				if err != nil {
					session.Unlock()
				}
				return held, err
			}
			msg.executionState = state
			if state.steerMessageIDs == nil {
				state.steerMessageIDs = make(map[string]bool)
			}
			state.steerMessageIDs[msg.scheduledInputID] = true
			state.mu.Unlock()
			e.interactiveMu.Unlock()
			return false, nil
		}
		handled, err := e.admitBusyMessage(p, msg, key, session, sessions)
		if err != nil {
			return false, err
		}
		if handled {
			slog.Info("scheduled prompt accepted into session queue", "session", msg.SessionKey, "input_id", msg.scheduledInputID)
			// The preceding owner can release the session between TryLock and enqueue.
			if session.TryLock() {
				if !e.launchAcceptedWork(func() { e.drainOrphanedQueue(session, sessions, key, agent, workspace) }) {
					session.Unlock()
					return true, fmt.Errorf("engine is shutting down")
				}
			}
			return true, nil
		}
		// A dead state can coexist briefly with its still-locked processor. Wait for
		// that owner to unlock, then start/resume through the normal foreground path.
		select {
		case <-e.ctx.Done():
			return false, e.ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (e *Engine) applyQueuedMode(as AgentSession, mode string, agent Agent) func() {
	if mode == "" {
		return nil
	}
	switcher, ok := as.(LiveModeSwitcher)
	if !ok || !switcher.SetLiveMode(mode) {
		return nil
	}
	restore := "default"
	if agent == nil {
		agent = e.agent
	}
	if ma, ok := agent.(interface{ GetMode() string }); ok && ma.GetMode() != "" {
		restore = ma.GetMode()
	}
	return func() { switcher.SetLiveMode(restore) }
}
