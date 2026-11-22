package core

import (
	"log/slog"
	"strings"
	"time"
)

// autoCardDelay is how long an automated or background turn runs before it
// opens a turn card. The card opens only once the turn has called a tool, so
// a short turn or one that only writes text never shows a card.
var autoCardDelay = 10 * time.Second

// autoCardCause names why a foreground turn that no person started is running;
// the empty key means the turn never opens a card of its own.
//   - webhook, assistant and relay input: another assistant passed something on
//     (wake-assistant arrives as a webhook).
//   - timer and cron jobs that are not muted: they already announce their start.
//   - muted jobs and heartbeats stay silent unless they have something to say.
func autoCardCause(input *Message, muted bool) MsgKey {
	if input == nil || muted {
		return ""
	}
	id := strings.ToLower(strings.TrimSpace(input.UserID))
	switch {
	case id == "webhook" || id == "assistant" || id == "relay" || strings.HasPrefix(id, "relay:"):
		return MsgAutoCardForwarded
	case id == "timer" || id == "cron":
		return MsgAutoCardScheduled
	}
	return ""
}

// personCard reports a card that shows a person's own turn.
func (sp *streamPreview) personCard() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return !sp.hidden && !sp.autoCard
}

func (sp *streamPreview) isAutoCard() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.autoCard
}

// openAutoCard shows a hidden preview as a turn card whose first row says why
// the turn runs. It reports whether the card is up.
func (e *Engine) openAutoCard(sp *streamPreview, p Platform, replyCtx any, cause MsgKey) bool {
	sp.mu.Lock()
	sp.hidden, sp.autoCard = false, true
	sp.mu.Unlock()
	e.acknowledgeTurnStart(p, replyCtx, sp, false)
	if !sp.replacePreview(e.i18n.T(cause)) {
		sp.mu.Lock()
		sp.hidden, sp.autoCard = true, false
		sp.mu.Unlock()
		return false
	}
	return true
}

// collapseQuiet ends a card that has no reply to show. It collapses to its
// steps rather than being recalled, which would leave a "recalled" line.
func (sp *streamPreview) collapseQuiet() {
	sp.mu.Lock()
	handle := sp.previewMsgID
	sp.cancelTimerLocked()
	select {
	case <-sp.timerStop:
	default:
		close(sp.timerStop)
	}
	sp.previewMsgID = nil
	sp.degraded = true
	sp.mu.Unlock()
	if handle == nil {
		return
	}
	if c, ok := sp.platform.(PreviewCollapser); ok {
		err := c.CollapsePreview(sp.ctx, handle)
		if err == nil {
			return
		}
		slog.Warn("auto card: collapse failed; recalling it", "error", err)
	}
	if cleaner, ok := sp.platform.(PreviewCleaner); ok {
		if err := cleaner.DeletePreviewMessage(sp.ctx, handle); err != nil {
			slog.Warn("auto card: recall failed", "error", err)
		}
	}
}

// backgroundCard is the card of a background turn, read by the unsolicited
// reader. Ownership lives in state.backgroundCard: whoever clears it there
// (the reader at the turn's end, or a person's turn taking the card over)
// owns the card from then on.
type backgroundCard struct {
	e            *Engine
	state        *interactiveState
	sessionKey   string
	workspaceDir string
	sp           *streamPreview
	watch        *turnWatch
	timer        *time.Timer
	due          bool // the delay passed before the turn called a tool
	tools        int
}

// begin arms the delay at the first event of a background turn.
func (c *backgroundCard) begin() {
	c.reset()
	c.timer = time.NewTimer(autoCardDelay)
}

func (c *backgroundCard) delayC() <-chan time.Time {
	if c.timer == nil || c.sp != nil {
		return nil
	}
	return c.timer.C
}

func (c *backgroundCard) watchC() <-chan time.Time {
	if c.watch == nil {
		return nil
	}
	return c.watch.C()
}

func (c *backgroundCard) delayPassed() {
	c.timer = nil
	if c.tools > 0 {
		c.open()
	} else {
		c.due = true
	}
}

func (c *backgroundCard) observe(event Event) {
	if event.Type == EventToolUse {
		c.tools++
		if c.due && c.sp == nil {
			c.due = false
			c.open()
		}
	}
	sp := c.owned()
	if sp == nil {
		return
	}
	sp.setNotice(c.watch.observe(c.e.i18n, event))
	if event.Type == EventToolUse || event.Type == EventProgress {
		text := strings.TrimSpace(event.Content)
		if event.Type == EventToolUse {
			text = publicToolProgress(event)
		}
		sp.appendProgressEvent(event, text)
	}
}

func (c *backgroundCard) tick() {
	if sp := c.owned(); sp != nil {
		sp.setNotice(c.watch.tick(c.e.i18n))
	}
}

func (c *backgroundCard) open() {
	e, state := c.e, c.state
	state.mu.Lock()
	p, replyCtx := state.platform, state.replyCtx
	state.mu.Unlock()
	if p == nil {
		return
	}
	render := func(text string) string { return e.renderOutgoingContentForWorkspace(p, text, c.workspaceDir) }
	sp := newStreamPreview(e.streamPreview, p, replyCtx, e.ctx, e.maskStreaming(render))
	sp.progressI18n = e.i18n
	if sp.segmented == nil || !sp.canPreview() {
		return
	}
	if !e.openAutoCard(sp, p, replyCtx, MsgAutoCardBackground) {
		return
	}
	c.sp = sp
	c.watch = newTurnWatch(true, e.display.ThinkingMessages && !e.display.appendProgress())
	state.mu.Lock()
	state.backgroundCard = sp
	state.mu.Unlock()
	slog.Info("background turn opened its card", "session", c.sessionKey)
}

// owned returns the card while the reader still owns it.
func (c *backgroundCard) owned() *streamPreview {
	if c.sp == nil {
		return nil
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if c.state.backgroundCard != c.sp {
		c.sp = nil
		return nil
	}
	return c.sp
}

// take ends the reader's ownership and returns the card, if it still had it.
func (c *backgroundCard) take() *streamPreview {
	sp := c.owned()
	if sp != nil {
		c.state.mu.Lock()
		c.state.backgroundCard = nil
		c.state.mu.Unlock()
	}
	c.reset()
	return sp
}

func (c *backgroundCard) reset() {
	if c.timer != nil {
		c.timer.Stop()
	}
	if c.watch != nil {
		c.watch.stop()
	}
	c.timer, c.watch, c.sp, c.due, c.tools = nil, nil, nil, false, 0
}

// collapse settles a card the reader still owns, with no reply on it.
func (c *backgroundCard) collapse() {
	if sp := c.take(); sp != nil {
		sp.collapseQuiet()
	}
}

// adoptBackgroundCardLocked hands an open background card to a person's turn:
// the card moves under their message and carries their turn from now on.
// state.mu must be held.
func adoptBackgroundCardLocked(state *interactiveState, sessionKey string, replyCtx any) *streamPreview {
	sp := state.backgroundCard
	if sp == nil {
		return nil
	}
	state.backgroundCard = nil
	sp.unhide(replyCtx)
	sp.followUser(sessionKey, replyCtx)
	return sp
}

// adoptBackgroundCardFor gives a person's new turn the card of the background
// turn it interrupts, so the chat keeps one card.
func (e *Engine) adoptBackgroundCardFor(msg *Message, interactiveKey string) *streamPreview {
	if !isHumanTurn(msg) || msg.scheduledMute {
		return nil
	}
	e.interactiveMu.Lock()
	state := e.interactiveStates[interactiveKey]
	e.interactiveMu.Unlock()
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return adoptBackgroundCardLocked(state, msg.SessionKey, msg.ReplyCtx)
}
