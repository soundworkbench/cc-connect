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
	e.ensureInteractiveStateForQueueing(key, p, msg.ReplyCtx)
	for {
		if !msg.admissionDeadline.IsZero() && time.Now().After(msg.admissionDeadline) {
			return false, fmt.Errorf("local assistant admission timed out")
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
			duplicate := state.steerMessageIDs[msg.scheduledInputID]
			if !duplicate {
				if state.steerMessageIDs == nil {
					state.steerMessageIDs = make(map[string]bool)
				}
				state.steerMessageIDs[msg.scheduledInputID] = true
			}
			state.mu.Unlock()
			e.interactiveMu.Unlock()
			if duplicate {
				session.Unlock()
				return true, nil
			}
			return false, nil
		}
		if e.deliverMessageForBusySession(p, msg, key, session, sessions) {
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
