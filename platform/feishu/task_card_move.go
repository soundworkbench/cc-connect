package feishu

import (
	"context"
	"log/slog"

	"github.com/chenhg5/cc-connect/core"
)

var _ core.TaskCardMover = (*Platform)(nil)

// MoveTaskCard posts the task card again as a new message, replying to rc (in
// a topic: inside the topic), and folds the old card into one grey line,
// view.Moved. Nothing is recalled, so the chat shows no "recalled a message"
// line. A failed fold leaves the old card as it was; the new card counts.
func (p *Platform) MoveTaskCard(ctx context.Context, handle any, rc any, view core.TaskCardView) (any, error) {
	old, ok := handle.(*feishuPreviewHandle)
	if !ok {
		return nil, core.ErrNotSupported
	}
	moved, err := p.SendTaskCard(ctx, rc, view)
	if err != nil {
		return nil, err
	}
	messageID, _ := old.ids()
	p.liveCards.remove(messageID)
	fold := greyLineCardJSON(view.Moved)
	old.mu.Lock()
	cardID := old.cardID
	old.mu.Unlock()
	if cardID != "" {
		err = p.updateCardEntity(ctx, old, fold)
	} else {
		err = p.patchCardMessage(ctx, messageID, fold)
	}
	if err != nil {
		slog.Warn(p.tag()+": fold moved task card failed", "message_id", messageID, "error", err)
	}
	return moved, nil
}
