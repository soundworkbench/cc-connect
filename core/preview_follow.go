package core

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PreviewMover is implemented by segmented previews that can re-post the live
// card at the bottom of the chat (replying to replyCtx) and fold the old one.
// After a move the same handle drives the new card. It returns
// ErrPreviewIsLatest, and moves nothing, when the card is already the latest
// message in the chat.
type PreviewMover interface {
	MovePreview(ctx context.Context, previewHandle any, replyCtx any) error
}

// ErrPreviewIsLatest reports that a card needs no move: nothing sits below it.
var ErrPreviewIsLatest = errors.New("preview is already the latest message")

var (
	// previewMoveSettle is how long a burst of messages must stay quiet
	// before the card moves: each new message restarts the wait, and the
	// card then moves once, under the last of them.
	previewMoveSettle = 2500 * time.Millisecond
	// previewMoveGap is how long the turn's card stays where it appeared
	// before it may move.
	previewMoveGap = 10 * time.Second
	// previewMoveAgain is how long after a move the card may move again. A
	// turn's card normally moves once: messages sent within this time after
	// the move stay below it rather than chasing the card down the chat.
	previewMoveAgain = 60 * time.Second
)

// followUser keeps the live card below a message the person just added to the
// running turn. Message timestamps are not compared with the card's: they come
// from the platform's clock, and a skew would leave the card above the message.
func (sp *streamPreview) followUser(sessionKey string, replyCtx any) {
	mover, ok := sp.segmented.(PreviewMover)
	if !ok {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.previewMsgID == nil || sp.hidden || sp.degraded {
		return
	}
	if !sp.lastMoveAt.IsZero() && time.Since(sp.lastMoveAt) < previewMoveAgain {
		return
	}
	sp.followKey, sp.followCtx = sessionKey, replyCtx
	at := time.Now().Add(previewMoveSettle)
	if earliest := sp.previewStartedAt.Add(previewMoveGap); at.Before(earliest) {
		at = earliest
	}
	if sp.followTimer != nil {
		sp.followTimer.Stop()
	}
	sp.followTimer = time.AfterFunc(time.Until(at), func() { sp.moveToBottom(mover) })
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
	from := previewID(sp.platform, sp.previewMsgID)
	err := mover.MovePreview(sp.ctx, sp.previewMsgID, sp.followCtx)
	switch {
	case errors.Is(err, ErrPreviewIsLatest):
		cardEvent("card_move_skipped", sp.followKey, from, "reason", "already_latest")
		return
	case errors.Is(err, ErrNotSupported):
		return
	case err != nil:
		slog.Warn("card: move failed", "session", sp.followKey, "card", from, "error", err)
		return
	}
	sp.lastMoveAt = time.Now()
	cardEvent("card_move", sp.followKey, previewID(sp.platform, sp.previewMsgID), "from", from, "reason", "user_message_below")
}

// PreviewIdentifier names a card for logs (Feishu: its message ID).
type PreviewIdentifier interface {
	PreviewID(previewHandle any) string
}

func previewID(p Platform, handle any) string {
	if id, ok := p.(PreviewIdentifier); ok && handle != nil {
		return id.PreviewID(handle)
	}
	return ""
}

// cardEvent records a card lifecycle event at INFO and, when trace_file is
// set, in the trace. It names the session and card, never message text.
func cardEvent(event, sessionKey, card string, kv ...any) {
	args := append([]any{"session", sessionKey, "card", card}, kv...)
	slog.Info("card: "+event, args...)
	if StreamTraceOn() {
		fields := map[string]any{"session": sessionKey, "card": card}
		for i := 0; i+1 < len(kv); i += 2 {
			if k, ok := kv[i].(string); ok {
				fields[k] = kv[i+1]
			}
		}
		StreamTrace(event, fields)
	}
}
