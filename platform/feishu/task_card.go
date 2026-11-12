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
		p.liveCards.keep(h.messageID, h.cardID, view.Interrupted)
	}
	return h, nil
}

// UpdateTaskCard replaces the card with view.
func (p *Platform) UpdateTaskCard(ctx context.Context, handle any, view core.TaskCardView) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok {
		return core.ErrNotSupported
	}
	if view.State.Live() {
		// Tasks joining or ending change what a restart should leave on the card.
		p.liveCards.keep(h.messageID, h.cardID, view.Interrupted)
	} else {
		defer p.liveCards.remove(h.messageID)
	}
	card := p.taskCardJSON(view)
	if h.cardID != "" {
		return p.updateCardEntity(ctx, h, card)
	}
	return p.patchCardMessage(ctx, h.messageID, card)
}

// taskCardJSON lays out a task card: a header with its title and its state as
// a coloured tag, the body, and the folded details panel. A card of one task
// has one line "<step or outcome> · <time>" (with the spinner while the task
// runs); its details hold the steps and, when the card shows a shorter line,
// the agent's original text. A card of several tasks has one row per task
// and their steps in the details. There is no stop button: the person asks
// the agent to stop a task.
func (p *Platform) taskCardJSON(v core.TaskCardView) string {
	if len(v.Rows) == 0 {
		return taskCardFrame(v, p.oneTaskBody(v))
	}
	card := taskCardFrame(v, taskRowsBody(v, true))
	if len(card) > maxRichCardJSONBytes {
		// Many long-running rows: the details keep the rows, not every step.
		card = taskCardFrame(v, taskRowsBody(v, false))
	}
	return card
}

func (p *Platform) oneTaskBody(v core.TaskCardView) []map[string]any {
	line := map[string]any{"tag": "markdown", "content": escapeCardText(v.Line)}
	if v.State.Live() {
		if key := p.cachedStepIcons().spinner; key != "" {
			line["icon"] = map[string]any{"tag": "custom_icon", "img_key": key}
		} else {
			line["content"] = spinnerGlyphs[0] + " " + escapeCardText(v.Line)
		}
	}
	els := []map[string]any{line}
	var details []map[string]any
	if len(v.Details) > 0 {
		details = append(details, notation(stepLines(v.Details)))
	}
	details = appendFull(details, v.FullTitle, v.Full)
	if len(details) > 0 {
		els = append(els, buildRichPanel(v.DetailsTitle, details))
	}
	return els
}

// taskRowsBody is the body of a card of several tasks: the shown rows, a
// note naming how many more sit in the details, then the details panel with
// every task's row, its steps (when withSteps) and a failed task's raw error.
func taskRowsBody(v core.TaskCardView, withSteps bool) []map[string]any {
	var shown, details []map[string]any
	var body []string
	for _, r := range v.Rows {
		if r.Shown {
			body = append(body, escapeCardText(r.Text))
		}
		text := escapeCardText(r.Text)
		if withSteps && len(r.Details) > 0 {
			text += "\n" + stepLines(r.Details)
		}
		details = append(details, notation(text))
		if withSteps {
			details = appendFull(details, r.FullTitle, r.Full)
		}
	}
	shown = append(shown, map[string]any{"tag": "markdown", "content": strings.Join(body, "\n")})
	if v.More != "" {
		shown = append(shown, notation("<font color='grey'>"+escapeCardText(v.More)+"</font>"))
	}
	return append(shown, buildRichPanel(v.DetailsTitle, details))
}

func notation(content string) map[string]any {
	return map[string]any{"tag": "markdown", "content": content, "text_size": "notation"}
}

func stepLines(steps []string) string {
	lines := make([]string, len(steps))
	for i, d := range steps {
		lines[i] = "· " + escapeCardText(d)
	}
	return strings.Join(lines, "\n")
}

// appendFull adds the agent's original text, as plain text so raw output and
// errors keep their characters as written.
func appendFull(els []map[string]any, title, full string) []map[string]any {
	if full == "" {
		return els
	}
	return append(els, notation("**"+escapeCardText(title)+"**"),
		map[string]any{"tag": "div", "text": map[string]any{"tag": "plain_text", "content": full, "text_size": "notation"}})
}

func taskCardFrame(v core.TaskCardView, els []map[string]any) string {
	// Header templates and text tags name grey differently.
	color, tag := "grey", "neutral"
	switch v.State {
	case core.TaskCardRunning:
		color, tag = "blue", "blue"
	case core.TaskCardCompleted:
		color, tag = "green", "green"
	case core.TaskCardFailed:
		color, tag = "red", "red"
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "default", "summary": map[string]any{"content": v.Title + " · " + v.Label}},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": v.Title},
			"template": color,
			"text_tag_list": []map[string]any{{
				"tag": "text_tag", "text": map[string]any{"tag": "plain_text", "content": v.Label}, "color": tag,
			}},
		},
		"body": map[string]any{"elements": els},
	}
	b, _ := json.Marshal(card)
	return string(b)
}
