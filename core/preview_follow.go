package core

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PreviewMover is implemented by segmented previews that can re-post the live
// card at the bottom of the chat (replying to replyCtx) and fold the old one.
// After a move the same handle drives the new card.
type PreviewMover interface {
	MovePreview(ctx context.Context, previewHandle any, replyCtx any) error
}

// previewMoveGap is the least time between two moves of one turn's card, and
// between the card's start and its first move: messages sent in a burst
// cost one move, made after the last of them.
var previewMoveGap = 10 * time.Second

// followUser keeps the live card below a message the person just added to the
// running turn. Message timestamps are not compared with the card's: they come
// from the platform's clock, and a skew would leave the card above the message.
// A message that raced the card's creation costs at most one extra move.
func (sp *streamPreview) followUser(replyCtx any) {
	mover, ok := sp.segmented.(PreviewMover)
	if !ok {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.previewMsgID == nil || sp.hidden || sp.degraded {
		return
	}
	sp.followCtx = replyCtx
	if sp.followTimer != nil {
		return // the pending move takes the newest message
	}
	last := sp.lastMoveAt
	if last.IsZero() {
		last = sp.previewStartedAt
	}
	sp.followTimer = time.AfterFunc(max(0, time.Until(last.Add(previewMoveGap))), func() { sp.moveToBottom(mover) })
}

func (sp *streamPreview) moveToBottom(mover PreviewMover) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.followTimer = nil
	select {
	case <-sp.timerStop:
		return // the turn already finished
	default:
	}
	if sp.previewMsgID == nil || sp.degraded {
		return
	}
	if err := mover.MovePreview(sp.ctx, sp.previewMsgID, sp.followCtx); err != nil {
		if !errors.Is(err, ErrNotSupported) {
			slog.Warn("preview move failed", "error", err)
		}
		return
	}
	sp.lastMoveAt = time.Now()
	StreamTrace("card_move", nil)
}
