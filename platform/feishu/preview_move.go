package feishu

import (
	"context"
	"log/slog"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

var _ core.PreviewMover = (*Platform)(nil)

// MovePreview re-posts the live card as a new message at the bottom of the
// chat, replying to rc, and folds the old card into one grey line. The new card
// starts with all the text received so far, shown at once rather than typed
// again; the handle then drives the new card, so no later write reaches the
// old one. Nothing is recalled.
func (p *Platform) MovePreview(ctx context.Context, handle any, rc any) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok || h.live == nil || h.cardID == "" {
		return core.ErrNotSupported
	}
	v := h.live
	v.mu.Lock()
	if v.stopped || v.fallback {
		v.mu.Unlock()
		return core.ErrNotSupported
	}
	if v.cfg.RevealCps > 0 {
		if err := v.adoptPending(); err != nil {
			v.mu.Unlock()
			return err
		}
		v.shown, v.body = len([]rune(v.target)), v.target
	}
	now := time.Now()
	moved, err := p.SendPreviewStart(ctx, rc, v.card(false))
	if err != nil {
		v.mu.Unlock()
		return err
	}
	nh := moved.(*feishuPreviewHandle)
	// Body updates still in flight hold the gate shared; taking it exclusive
	// waits for them, so they all land on the old card.
	h.gate.Lock()
	h.mu.Lock()
	old := &feishuPreviewHandle{messageID: h.messageID, chatID: h.chatID, cardID: h.cardID, sequence: h.sequence}
	h.messageID, h.chatID, h.cardID, h.sequence = nh.messageID, nh.chatID, nh.cardID, 0
	h.mu.Unlock()
	h.gate.Unlock()
	_ = v.takePipeErr() // a failure of the old card's updates says nothing about the new card
	v.fallback = h.cardID == ""
	v.sent, v.sentBodyID, v.lastBody = v.layoutFor(v.body), v.bodyID, v.body
	v.lastProgress, v.lastFooter, v.lastPublished = v.progress(now), v.footer(now), now
	movedText := v.i18n.T(core.MsgPreviewMoved)
	interrupted := v.i18n.T(core.MsgPreviewInterrupted)
	v.mu.Unlock()

	p.liveCards.add(h.messageID, h.cardID, interrupted)
	p.liveCards.remove(old.messageID)
	p.closeSegmentedStream(ctx, old)
	if err := p.updateCardEntity(ctx, old, greyLineCardJSON(movedText)); err != nil {
		slog.Warn(p.tag()+": fold moved card failed", "message_id", old.messageID, "error", err)
	}
	return nil
}
