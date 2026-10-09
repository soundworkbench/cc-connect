package core

import (
	"errors"
	"log/slog"
)

// state.mu must be held, keeping admission/history ordered with the turn drain.
func (e *Engine) trySteerMessageLocked(state *interactiveState, p Platform, msg *Message, session *Session, sessions *SessionManager) bool {
	if msg.scheduledPrompt || msg.ModeOverride != "" || session == nil || sessions == nil || state.stopped || state.pending != nil {
		return false
	}
	if msg.MessageID != "" && state.steerMessageIDs[msg.MessageID] {
		return true
	}
	prompt := e.buildSenderPrompt(msg.Content, msg.UserID, msg.UserName, msg.Platform, msg.SessionKey, msg.ChannelKey)
	var accepted bool
	var err error
	var receipt *SteerReceipt
	if async, ok := state.agentSession.(AgentSessionAsyncSteerer); ok {
		receipt, err = async.SteerAsync(prompt, msg.MessageID, msg.Images, msg.Files)
		accepted = receipt != nil
	} else if steerer, ok := state.agentSession.(AgentSessionSteerer); ok {
		accepted, err = steerer.Steer(prompt, msg.MessageID, msg.Images, msg.Files)
	} else {
		return false
	}
	if errors.Is(err, ErrSteerNotWritten) {
		state.steerWriteFailed = true
		return false
	}

	if accepted || err != nil {
		runMessageAccepted(msg)
		session.TouchUserActivity()
		session.AddHistory("user", msg.Content)
		sessions.Save()
		if msg.UserMessageTimeMs > state.currentTurnUserMessageTimeMs {
			state.currentTurnUserMessageTimeMs = msg.UserMessageTimeMs
		}
		if state.steerMessageIDs == nil {
			state.steerMessageIDs = make(map[string]bool)
		}
		if msg.MessageID != "" {
			state.steerMessageIDs[msg.MessageID] = true
		}
		if err != nil {
			// Uncertain transport errors must never trigger an automatic duplicate.
			slog.Warn("active-turn message delivery unconfirmed", "session", msg.SessionKey, "msg_id", msg.MessageID)
			if errors.Is(err, ErrSteerWriteUncertain) {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgSteerWriteInterrupted))
			}
		} else {
			e.steerReaction(p, msg.ReplyCtx, receipt)
		}

		return true
	}
	return false
}

// Input received while a session starts must not wait for the whole first turn.
func (e *Engine) steerQueuedMessages(state *interactiveState, session *Session, sessions *SessionManager) {
	state.mu.Lock()
	defer state.mu.Unlock()
	e.steerQueuedMessagesLocked(state, session, sessions)
}

// state.mu must be held. Never retry a failed/uncertain write or overtake a
// scheduled prompt, file, or permission-mode boundary.
func (e *Engine) steerQueuedMessagesLocked(state *interactiveState, session *Session, sessions *SessionManager) {
	for len(state.pendingMessages) > 0 {
		q := state.pendingMessages[0]
		if !q.steerRetry {
			return
		}
		msg := &Message{Content: q.content, MessageID: q.messageID, Images: q.images, Files: q.files,
			UserID: q.userID, UserName: q.userName, Platform: q.msgPlatform, SessionKey: q.msgSessionKey,
			ChannelKey: q.channelKey, ReplyCtx: q.replyCtx, UserMessageTimeMs: q.userMessageTimeMs}
		if !e.trySteerMessageLocked(state, q.platform, msg, session, sessions) {
			if state.steerWriteFailed {
				state.pendingMessages[0].steerRetry = false
				state.steerWriteFailed = false
				e.reply(q.platform, q.replyCtx, e.i18n.T(MsgMessageSteerWriteFailed))
			}
			return
		}
		state.pendingMessages = state.pendingMessages[1:]
	}
}

// Network reaction calls and acknowledgement waits never hold the session lock.
func (e *Engine) steerReaction(p Platform, replyCtx any, receipt *SteerReceipt) {
	reaction, ok := p.(SteerReaction)
	if !ok {
		return
	}
	go func() {
		finish := reaction.StartSteerReaction(e.ctx, replyCtx)
		if receipt == nil {
			finish(true)
			return
		}
		select {
		case <-receipt.Ack:
			finish(true)
		case <-receipt.End:
			// Prefer an acknowledgement that raced process exit.
			select {
			case <-receipt.Ack:
				finish(true)
			default:
				finish(false)
			}
		case <-e.ctx.Done():
			finish(false)
		}
	}()
}
