package core

import (
	"fmt"
	"log/slog"
	"time"
)

// Returns false with the session lock owned by the caller (lockGen is that
// lock's generation), or true once the prompt belongs to the shared FIFO. Scheduler success means accepted for
// processing, not a separately collected model result.
func (e *Engine) admitScheduledPrompt(p Platform, msg *Message, session *Session, sessions *SessionManager, agent Agent, key, workspace string) (queued bool, lockGen uint64, err error) {
	e.ensureInteractiveStateForQueueing(key, p, msg.ReplyCtx, session)
	e.setQueuedWorkspace(key, workspace)
	for {
		if !msg.admissionDeadline.IsZero() && time.Now().After(msg.admissionDeadline) {
			return false, 0, fmt.Errorf("local assistant admission timed out")
		}
		if e.isShuttingDown() {
			return false, 0, fmt.Errorf("engine is shutting down")
		}
		if e.ctx.Err() != nil {
			return false, 0, e.ctx.Err()
		}
		if gen, locked := session.TryLock(); locked {
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
					session.Unlock(gen)
					if err != nil {
						return true, 0, err
					}
					return true, 0, fmt.Errorf("queue admission failed")
				}
				if !e.launchAcceptedWork(func() { e.drainOrphanedQueue(session, sessions, key, agent, workspace, gen) }) {
					session.Unlock(gen)
				}
				return true, 0, nil
			}
			if state.steerMessageIDs[msg.scheduledInputID] {
				state.mu.Unlock()
				e.interactiveMu.Unlock()
				session.Unlock(gen)
				return true, 0, nil
			}
			if e.shouldHold(msg) {
				state.mu.Unlock()
				e.interactiveMu.Unlock()
				held, err := e.holdIdleMessage(p, msg, session, sessions, agent, key, workspace, gen)
				if err != nil {
					session.Unlock(gen)
				}
				if held {
					return true, 0, err
				}
				return false, gen, err
			}
			msg.executionState = state
			if state.steerMessageIDs == nil {
				state.steerMessageIDs = make(map[string]bool)
			}
			state.steerMessageIDs[msg.scheduledInputID] = true
			state.mu.Unlock()
			e.interactiveMu.Unlock()
			return false, gen, nil
		}
		handled, err := e.admitBusyMessage(p, msg, key, session, sessions)
		if err != nil {
			return false, 0, err
		}
		if handled {
			slog.Info("scheduled prompt accepted into session queue", "session", msg.SessionKey, "input_id", msg.scheduledInputID)
			// The preceding owner can release the session between TryLock and enqueue.
			if gen, ok := session.TryLock(); ok {
				if !e.launchAcceptedWork(func() { e.drainOrphanedQueue(session, sessions, key, agent, workspace, gen) }) {
					session.Unlock(gen)
					return true, 0, fmt.Errorf("engine is shutting down")
				}
			}
			return true, 0, nil
		}
		// A dead state can coexist briefly with its still-locked processor. Wait for
		// that owner to unlock, then start/resume through the normal foreground path.
		select {
		case <-e.ctx.Done():
			return false, 0, e.ctx.Err()
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
