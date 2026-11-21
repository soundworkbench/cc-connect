package core

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// TaskCardMover is implemented by platforms that can post a task card again
// at the bottom of the chat, replying to replyCtx (in a topic: inside it),
// and fold the old card into one grey line, view.Moved. Nothing is recalled.
// It returns the new card's handle; later updates go to it.
type TaskCardMover interface {
	MoveTaskCard(ctx context.Context, handle any, replyCtx any, view TaskCardView) (any, error)
}

const (
	// taskCardFollowAfter is how many messages must sit below a task card
	// (the person's messages and the bot's replies) before it moves: about
	// two rounds of conversation, enough to push it off a phone screen.
	taskCardFollowAfter = 4
	// taskCardFollowGap is how long a task card stays where it was posted or
	// last moved before it may move again.
	taskCardFollowGap = 3 * time.Minute
)

// turnBegan and turnEnded bracket a foreground turn of the session. A task
// card moves only between turns, so it never cuts into a streaming reply.
func (m *taskCardManager) turnBegan(sessionKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy[sessionKey]++
}

func (m *taskCardManager) turnEnded(e *Engine, sessionKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy[sessionKey]--; m.busy[sessionKey] > 0 {
		return
	}
	delete(m.busy, sessionKey)
	m.followLocked(e, m.follow[sessionKey])
}

// replied counts what a finished turn left below the session's task card:
// the person's message when the card was already there as the turn began,
// and the reply when one was sent. rc is where a move posts the card. A
// turn the agent started on its own (idle: no foreground turn holds the
// session) may move the card at once.
func (m *taskCardManager) replied(e *Engine, sessionKey string, rc any, turnStart time.Time, human, sent, idle bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.follow[sessionKey]
	if c == nil {
		return
	}
	if human && c.placedAt.Before(turnStart) {
		c.below++
	}
	if sent {
		c.below++
		if rc != nil {
			c.followCtx = rc
		}
	}
	if idle && m.busy[sessionKey] == 0 {
		m.followLocked(e, c)
	}
}

// followLocked moves c to the bottom of the chat when enough messages have
// pushed it up: a card with running tasks at most every followGap; a settled
// card once, if it was pushed up when its last task ended. Caller holds m.mu.
func (m *taskCardManager) followLocked(e *Engine, c *taskCard) {
	if c == nil || c.handle == nil || c.moving || c.below < m.followAfter {
		return
	}
	mover, ok := c.platform.(TaskCardMover)
	if !ok {
		return
	}
	switch {
	case m.open[c.sessionKey] == c && !c.allEnded():
		if time.Since(c.placedAt) < m.followGap {
			return
		}
		m.moveLocked(e, c, mover, "pushed_up")
	case c.finalDue:
		delete(m.follow, c.sessionKey)
		m.moveLocked(e, c, mover, "ended_pushed_up")
	}
}

// settledLocked decides, once a card's final state is written, whether it
// moves one last time: only when messages have pushed it up since it was
// posted or last moved. It moves now when no turn runs, else after the turn.
// Caller holds m.mu.
func (m *taskCardManager) settledLocked(e *Engine, c *taskCard) {
	if m.follow[c.sessionKey] != c {
		return
	}
	if c.moving || c.below < m.followAfter {
		delete(m.follow, c.sessionKey)
		return
	}
	c.finalDue = true
	if m.busy[c.sessionKey] == 0 {
		m.followLocked(e, c)
	}
}

// moveLocked re-posts c below the latest message. The card's current view is
// read once the move holds the card's writes, so it carries every change
// made before. Caller holds m.mu.
func (m *taskCardManager) moveLocked(e *Engine, c *taskCard, mover TaskCardMover, reason string) {
	c.moving = true
	go func() {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		m.mu.Lock()
		view := m.viewLocked(e, c)
		old, rc := c.handle, c.followCtx
		m.mu.Unlock()
		from := cardHandleID(c.platform, old)
		moved, err := mover.MoveTaskCard(e.ctx, old, rc, view)
		m.mu.Lock()
		c.moving = false
		if err == nil && moved != nil {
			c.handle, c.placedAt, c.below = moved, time.Now(), 0
		}
		m.mu.Unlock()
		if err != nil || moved == nil {
			if err != nil && !errors.Is(err, ErrNotSupported) {
				slog.Warn("task card: move failed", "session", c.sessionKey, "card", from, "error", err)
			}
			return
		}
		cardEvent("task_card_move", c.sessionKey, cardHandleID(c.platform, moved), "from", from, "reason", reason)
	}()
}
