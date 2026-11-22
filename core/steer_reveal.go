package core

import "log/slog"

// steerReveal is a person's message that was steered into a turn they cannot
// see: an automated turn (timer, cron, webhook, assistant) whose preview is
// hidden. The turn's event loop takes it and opens the ordinary turn card under
// that message, so the person sees progress and gets the final reply there.
type steerReveal struct {
	platform Platform
	replyCtx any
	content  string
}

// revealSignal is read by the turn's event loop. It holds at most one message:
// the card belongs to the first person who wrote into the hidden turn.
func (s *interactiveState) revealSignal() chan steerReveal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revealChLocked()
}

func (s *interactiveState) revealChLocked() chan steerReveal {
	if s.reveal == nil {
		s.reveal = make(chan steerReveal, 1)
	}
	return s.reveal
}

// dropStaleReveal clears a message offered to a turn that has ended.
func dropStaleReveal(ch chan steerReveal) {
	select {
	case <-ch:
	default:
	}
}

func (sp *streamPreview) isHidden() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.hidden
}

// unhide turns a hidden preview into a visible one under replyCtx. Progress
// recorded while hidden stays recorded; the card shows what follows.
func (sp *streamPreview) unhide(replyCtx any) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.hidden, sp.autoCard = false, false
	sp.replyCtx = replyCtx
}

func (sp *streamPreview) hasCardOrPendingAck() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.previewMsgID != nil || sp.pendingAcknowledgement != nil
}

// showSteeredMessageLocked answers a person's message that was steered into the
// running turn. state.mu must be held.
//   - The turn already shows a card: the card follows the person (unchanged).
//   - The turn is automated: its event loop opens the turn card, or hands over
//     the card the turn opened on its own.
//   - A background turn holds the session: its card, or one line, says the
//     reply will come.
func (e *Engine) showSteeredMessageLocked(state *interactiveState, p Platform, msg *Message) {
	sp := state.activePreview
	switch {
	case sp != nil && sp.personCard():
		sp.followUser(msg.SessionKey, msg.ReplyCtx)
	case !isHumanTurn(msg):
		// Automation joining a hidden turn stays hidden.
	case sp != nil:
		select {
		case state.revealChLocked() <- steerReveal{platform: p, replyCtx: msg.ReplyCtx, content: msg.Content}:
		default:
		}
		if sp.isAutoCard() {
			sp.followUser(msg.SessionKey, msg.ReplyCtx) // the turn's own card becomes theirs
		}
	case state.backgroundTurnLease:
		// The background turn's reply goes to the person's message: on its card
		// when it has one, otherwise after one line saying so.
		state.platform, state.replyCtx = p, msg.ReplyCtx
		if bg := state.backgroundCard; bg != nil {
			bg.unhide(msg.ReplyCtx)
			bg.followUser(msg.SessionKey, msg.ReplyCtx)
			return
		}
		replyCtx := msg.ReplyCtx
		go e.reply(p, replyCtx, e.i18n.T(MsgSteerJoinsRunningWork))
	}
}

// revealHiddenTurn opens the turn card of a hidden automated turn under the
// message r came from. Only the turn's event loop calls it.
func (e *Engine) revealHiddenTurn(state *interactiveState, session *Session, sessionKey string, sp *streamPreview, watch *turnWatch, r steerReveal, hasStreamingCard bool) {
	state.mu.Lock()
	state.platform, state.replyCtx = r.platform, r.replyCtx
	state.mu.Unlock()
	sp.unhide(r.replyCtx)
	watch.enable()
	e.acknowledgeTurnStart(r.platform, r.replyCtx, sp, hasStreamingCard)
	switch {
	case sp.hasCardOrPendingAck():
		e.startQuickLine(sp, r.content, historyBefore(session.GetHistory(quickLineHistoryTurns+1), r.content))
	case !hasStreamingCard:
		e.reply(r.platform, r.replyCtx, e.i18n.T(MsgSteerJoinsRunningWork))
	}
	slog.Info("steered message opened the turn card", "session", sessionKey)
}

// historyBefore drops the steered message itself, which the steer already
// added to history; the quick line gets it separately.
func historyBefore(history []HistoryEntry, content string) []HistoryEntry {
	if n := len(history); n > 0 && history[n-1].Role == "user" && history[n-1].Content == content {
		return history[:n-1]
	}
	if n := len(history); n > quickLineHistoryTurns {
		return history[n-quickLineHistoryTurns:]
	}
	return history
}
