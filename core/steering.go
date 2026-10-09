package core

import "log/slog"

// state.mu must be held, keeping admission/history ordered with the turn drain.
func (e *Engine) trySteerMessageLocked(state *interactiveState, p Platform, msg *Message, session *Session, sessions *SessionManager) bool {
	if session == nil || sessions == nil || state.stopped || state.pending != nil {
		return false
	}
	steerer, ok := state.agentSession.(AgentSessionSteerer)
	if !ok {
		return false
	}
	prompt := e.buildSenderPrompt(msg.Content, msg.UserID, msg.UserName, msg.Platform, msg.SessionKey, msg.ChannelKey)
	accepted, err := steerer.Steer(prompt, msg.MessageID, msg.Images, msg.Files)
	if accepted || err != nil {
		runMessageAccepted(msg)
		session.TouchUserActivity()
		session.AddHistory("user", msg.Content)
		sessions.Save()
		if msg.UserMessageTimeMs > state.currentTurnUserMessageTimeMs {
			state.currentTurnUserMessageTimeMs = msg.UserMessageTimeMs
		}
		if err != nil {
			// Do not log upstream text: it can include user input or credentials.
			slog.Warn("active-turn message delivery unconfirmed", "session", msg.SessionKey, "msg_id", msg.MessageID)
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMessageSteerUnconfirmed))
		} else {
			slog.Info("message accepted into active turn", "session", msg.SessionKey, "msg_id", msg.MessageID)
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMessageSteered))
		}
		return true
	}
	return false
}

// Input received while a session starts must not wait for the whole first turn.
func (e *Engine) steerQueuedMessages(state *interactiveState, session *Session, sessions *SessionManager) {
	state.mu.Lock()
	defer state.mu.Unlock()
	for len(state.pendingMessages) > 0 {
		q := state.pendingMessages[0]
		msg := &Message{Content: q.content, MessageID: q.messageID, Images: q.images, Files: q.files,
			UserID: q.userID, UserName: q.userName, Platform: q.msgPlatform, SessionKey: q.msgSessionKey,
			ChannelKey: q.channelKey, ReplyCtx: q.replyCtx, UserMessageTimeMs: q.userMessageTimeMs}
		if !e.trySteerMessageLocked(state, q.platform, msg, session, sessions) {
			return
		}
		state.pendingMessages = state.pendingMessages[1:]
	}
}
