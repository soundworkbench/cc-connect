package feishu

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

var _ core.TaskCardPlatform = (*Platform)(nil)

// SendTaskCard posts a background task's card as a CardKit entity, so later
// updates replace it in place without streaming mode (which closes itself
// after ten minutes; tasks often run longer).
func (p *Platform) SendTaskCard(ctx context.Context, rc any, view core.TaskCardView) (any, error) {
	if !p.useInteractiveCard {
		return nil, core.ErrNotSupported
	}
	handle, err := p.SendPreviewStart(ctx, rc, p.taskCardJSON(view))
	if err != nil {
		return nil, err
	}
	h := handle.(*feishuPreviewHandle)
	if view.State.Live() {
		p.liveCards.add(h.messageID, h.cardID, view.Interrupted)
	}
	return h, nil
}

// UpdateTaskCard replaces the card with view.
func (p *Platform) UpdateTaskCard(ctx context.Context, handle any, view core.TaskCardView) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok {
		return core.ErrNotSupported
	}
	if !view.State.Live() {
		defer p.liveCards.remove(h.messageID)
	}
	card := p.taskCardJSON(view)
	if h.cardID != "" {
		return p.updateCardEntity(ctx, h, card)
	}
	return p.patchCardMessage(ctx, h.messageID, card)
}

// taskCardJSON lays out a task card. Live: the title marking it a background
// task, the current step with the spinner, the elapsed minutes, the latest
// progress, a grey line saying the chat stays free, and the steps panel.
// Ended: the outcome line, the summary and the steps panel. There is no stop
// button: the person asks the agent to stop a task.
func (p *Platform) taskCardJSON(v core.TaskCardView) string {
	var els []map[string]any
	if v.State.Live() {
		els = append(els, map[string]any{"tag": "markdown", "content": "**" + escapeCardText(v.Title) + "**"})
		step := map[string]any{"tag": "markdown", "content": escapeCardText(v.Status)}
		if key := p.cachedStepIcons().spinner; key != "" {
			step["icon"] = map[string]any{"tag": "custom_icon", "img_key": key}
		} else {
			step["content"] = spinnerGlyphs[0] + " " + escapeCardText(v.Status)
		}
		els = append(els, step)
		if v.Elapsed != "" {
			els = append(els, map[string]any{"tag": "markdown", "content": "<font color='grey'>" + escapeCardText(v.Elapsed) + "</font>", "text_size": "notation"})
		}
		if v.Recent != "" {
			els = append(els, map[string]any{"tag": "markdown", "content": escapeCardText(v.Recent)})
		}
		if v.Hint != "" {
			els = append(els, map[string]any{"tag": "markdown", "content": "<font color='grey'>" + escapeCardText(v.Hint) + "</font>", "text_size": "notation"})
		}
	} else {
		color := "grey"
		switch v.State {
		case core.TaskCardCompleted:
			color = "green"
		case core.TaskCardFailed:
			color = "red"
		}
		els = append(els, map[string]any{"tag": "markdown", "content": "<font color='" + color + "'>" + escapeCardText(v.Status) + "</font>"})
		if v.Summary != "" {
			els = append(els, map[string]any{"tag": "markdown", "content": escapeCardText(v.Summary)})
		}
	}
	if len(v.Details) > 0 {
		lines := make([]string, len(v.Details))
		for i, d := range v.Details {
			lines[i] = "· " + escapeCardText(d)
		}
		els = append(els, buildRichPanel(v.DetailsTitle, []map[string]any{{"tag": "markdown", "content": strings.Join(lines, "\n"), "text_size": "notation"}}))
	}
	summary := v.Title
	if !v.State.Live() {
		summary = v.Status
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "default", "summary": map[string]any{"content": summary}},
		"body":   map[string]any{"elements": els},
	}
	b, _ := json.Marshal(card)
	return string(b)
}
