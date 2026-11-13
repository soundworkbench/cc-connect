package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func plainText(content string) map[string]any {
	return map[string]any{"tag": "plain_text", "content": content}
}

// ReplyCard sends a structured card as a reply to the original message.
func (p *interactivePlatform) ReplyCard(ctx context.Context, rctx any, card *core.Card) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("%s: invalid reply context type %T", p.tag(), rctx)
	}

	cardJSON := renderCard(card, rc.sessionKey)
	if !p.shouldUseThreadOrReplyAPI(rc) {
		if rc.chatID == "" {
			return fmt.Errorf("%s: chatID is empty, cannot send card", p.tag())
		}
		return p.createMessage(ctx, rc.chatID, larkim.MsgTypeInteractive, cardJSON, "send card")
	}
	return p.replyMessage(ctx, rc, larkim.MsgTypeInteractive, cardJSON)
}

// SendCard sends a structured card as a new message to the chat.
func (p *interactivePlatform) SendCard(ctx context.Context, rctx any, card *core.Card) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("%s: invalid reply context type %T", p.tag(), rctx)
	}
	if rc.chatID == "" {
		return fmt.Errorf("%s: chatID is empty, cannot send card", p.tag())
	}

	cardJSON := renderCard(card, rc.sessionKey)
	if p.shouldReplyInThread(rc) {
		return p.replyMessage(ctx, rc, larkim.MsgTypeInteractive, cardJSON)
	}
	return p.createMessage(ctx, rc.chatID, larkim.MsgTypeInteractive, cardJSON, "send card")
}

// SendCardID sends a card as a new message and returns the message ID so the
// card can be patched later (core.CardIDSender). In a topic the card is a
// reply in that topic; elsewhere it is a new chat message.
func (p *interactivePlatform) SendCardID(ctx context.Context, rctx any, card *core.Card) (string, error) {
	rc, ok := rctx.(replyContext)
	if !ok {
		return "", fmt.Errorf("%s: invalid reply context type %T", p.tag(), rctx)
	}
	if rc.chatID == "" {
		return "", fmt.Errorf("%s: chatID is empty, cannot send card", p.tag())
	}
	cardJSON := renderCard(card, rc.sessionKey)
	if p.shouldReplyInThread(rc) {
		return p.replyMessageID(ctx, rc, larkim.MsgTypeInteractive, cardJSON)
	}
	return p.createMessageID(ctx, rc.chatID, larkim.MsgTypeInteractive, cardJSON, "send card")
}

// PatchCard replaces the content of an already sent card message
// (core.CardPatcher).
func (p *interactivePlatform) PatchCard(ctx context.Context, messageID, sessionKey string, card *core.Card) error {
	if messageID == "" {
		return fmt.Errorf("%s: empty card messageID", p.tag())
	}
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(renderCard(card, sessionKey)).
			Build()).
		Build()
	return p.withTransientRetry(ctx, "patch card", func() error {
		return p.withFreshTenantAccessTokenRetry(ctx, "patch card", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
			resp, err := client.Im.Message.Patch(ctx, req, options...)
			if err != nil {
				return fmt.Errorf("%s: patch card: %w", p.tag(), err)
			}
			if !resp.Success() {
				return fmt.Errorf("%s: patch card code=%d msg=%s", p.tag(), resp.Code, resp.Msg)
			}
			return nil
		})
	})
}

// RefreshCard updates a previously rendered card in-place using the Patch API.
// It looks up the messageID stored from the most recent card action callback
// for the given session key and patches that message with the new card content.
func (p *interactivePlatform) RefreshCard(ctx context.Context, sessionKey string, card *core.Card) error {
	p.cardActionMsgMu.Lock()
	msgID := p.cardActionMsgIDs[sessionKey]
	v2 := p.cardActionV2[sessionKey]
	p.cardActionMsgMu.Unlock()

	if msgID == "" {
		return fmt.Errorf("%s: no tracked card messageID for session %q", p.tag(), sessionKey)
	}

	cardJSON := marshalCard(renderCallbackCardMap(card, sessionKey, v2))
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(msgID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(cardJSON).
			Build()).
		Build()
	return p.withTransientRetry(ctx, "refresh card", func() error {
		return p.withFreshTenantAccessTokenRetry(ctx, "refresh card", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
			resp, err := client.Im.Message.Patch(ctx, req, options...)
			if err != nil {
				return fmt.Errorf("%s: refresh card: %w", p.tag(), err)
			}
			if !resp.Success() {
				return fmt.Errorf("%s: refresh card code=%d msg=%s", p.tag(), resp.Code, resp.Msg)
			}
			return nil
		})
	})
}

// renderCardMap converts a core.Card into the Feishu Interactive Card map.
// Compact and shared cards, cards with a form, and the delete-mode form use
// card JSON 2.0; the rest use the classic 1.0 layout. Used both for message
// API (via renderCard) and callback responses (CardActionTriggerResponse).
func renderCardMap(card *core.Card, sessionKey string) map[string]any {
	result := map[string]any{
		"config": map[string]any{
			"width_mode": "default",
		},
	}
	if card == nil {
		return result
	}
	if card.Compact {
		return renderCompactCardMap(card, sessionKey)
	}
	if transformed, ok := renderDeleteModeCheckerCard(card); ok {
		return transformed
	}
	if card.Shared || hasForm(card) {
		return renderSharedCardMap(card, sessionKey)
	}

	if card.Header != nil && card.Header.Title != "" {
		color := card.Header.Color
		if color == "" {
			color = "blue"
		}
		result["header"] = map[string]any{
			"title":    plainText(card.Header.Title),
			"template": color,
		}
	}

	var elements []map[string]any
	for _, elem := range card.Elements {
		switch e := elem.(type) {
		case core.CardMarkdown:
			elements = append(elements, map[string]any{
				"tag":     "markdown",
				"content": e.Content,
			})
		case core.CardDivider:
			elements = append(elements, map[string]any{
				"tag": "hr",
			})
		case core.CardActions:
			var actions []map[string]any
			for _, btn := range e.Buttons {
				btnType := btn.Type
				if btnType == "" {
					btnType = "default"
				}
				valMap := map[string]string{"action": btn.Value}
				if sessionKey != "" {
					valMap["session_key"] = sessionKey
				}
				for k, v := range btn.Extra {
					valMap[k] = v
				}
				action := map[string]any{
					"tag":   "button",
					"text":  plainText(btn.Text),
					"type":  btnType,
					"value": valMap,
				}
				if c := renderConfirm(btn.Confirm); c != nil {
					action["confirm"] = c
				}
				if e.Layout == core.CardActionLayoutEqualColumns {
					action["width"] = "fill"
				}
				actions = append(actions, action)
			}
			if len(actions) > 0 {
				if e.Layout == core.CardActionLayoutEqualColumns {
					columns := make([]map[string]any, 0, len(actions))
					for _, action := range actions {
						columns = append(columns, map[string]any{
							"tag":              "column",
							"width":            "weighted",
							"weight":           1,
							"vertical_align":   "center",
							"horizontal_align": "center",
							"elements":         []map[string]any{action},
						})
					}
					columnSet := map[string]any{
						"tag":     "column_set",
						"columns": columns,
					}
					if len(actions) == 2 {
						columnSet["flex_mode"] = "bisect"
					}
					elements = append(elements, columnSet)
				} else {
					elements = append(elements, map[string]any{
						"tag":     "action",
						"actions": actions,
					})
				}
			}
		case core.CardListItem:
			btnType := e.BtnType
			if btnType == "" {
				btnType = "default"
			}
			valMap := map[string]string{"action": e.BtnValue}
			if sessionKey != "" {
				valMap["session_key"] = sessionKey
			}
			for k, v := range e.Extra {
				valMap[k] = v
			}
			elements = append(elements, map[string]any{
				"tag":       "column_set",
				"flex_mode": "none",
				"columns": []map[string]any{
					{
						"tag":            "column",
						"width":          "weighted",
						"weight":         5,
						"vertical_align": "center",
						"elements": []map[string]any{
							{
								"tag":     "markdown",
								"content": e.Text,
							},
						},
					},
					{
						"tag":            "column",
						"width":          "auto",
						"vertical_align": "center",
						"elements": []map[string]any{
							{
								"tag":   "button",
								"text":  plainText(e.BtnText),
								"type":  btnType,
								"value": valMap,
							},
						},
					},
				},
			})
		case core.CardSelect:
			var options []map[string]any
			for _, opt := range e.Options {
				options = append(options, map[string]any{
					"text":  plainText(opt.Text),
					"value": opt.Value,
				})
			}
			selectElem := map[string]any{
				"tag":         "select_static",
				"placeholder": plainText(e.Placeholder),
				"options":     options,
			}
			if sessionKey != "" {
				selectElem["value"] = map[string]string{"session_key": sessionKey}
			}
			if e.InitValue != "" {
				selectElem["initial_option"] = e.InitValue
			}
			elements = append(elements, map[string]any{
				"tag":     "action",
				"actions": []map[string]any{selectElem},
			})
		case core.CardNote:
			elements = append(elements, map[string]any{
				"tag":      "note",
				"elements": []map[string]any{plainText(e.Text)},
			})
		}
	}

	if len(elements) == 0 {
		elements = []map[string]any{{"tag": "markdown", "content": " "}}
	}

	result["elements"] = elements
	return result
}

func hasForm(card *core.Card) bool {
	for _, elem := range card.Elements {
		if _, ok := elem.(core.CardForm); ok {
			return true
		}
	}
	return false
}

// compactCardSpacing is the gap between elements in a compact card;
// compactOptionGap is the extra space above each option after the first, so
// a description stays closer to its own button than to the next one.
const (
	compactCardSpacing = "4px"
	compactOptionGap   = "6px 0px 0px 0px"
)

// renderCompactCardMap renders a card as schema 2.0 with tight, uniform
// spacing and no coloured header unless the card asks for one. Options
// (CardListItem) become a full-width button with the description in small
// grey text underneath. Callback data is carried in the button's callback
// behavior and arrives in the action value exactly as in the classic layout.
func renderCompactCardMap(card *core.Card, sessionKey string) map[string]any {
	var elements []map[string]any
	options := 0
	callbackButton := func(text, btnType, value string, extra map[string]string) map[string]any {
		if btnType == "" {
			btnType = "default"
		}
		return map[string]any{
			"tag":       "button",
			"text":      plainText(text),
			"type":      btnType,
			"width":     "fill",
			"behaviors": []map[string]any{{"type": "callback", "value": v2CallbackValue(value, sessionKey, extra)}},
		}
	}
	grey := func(text string) map[string]any {
		return map[string]any{
			"tag":       "markdown",
			"content":   "<font color='grey'>" + text + "</font>",
			"text_size": "notation",
		}
	}
	for _, elem := range card.Elements {
		switch e := elem.(type) {
		case core.CardMarkdown:
			elements = append(elements, map[string]any{"tag": "markdown", "content": e.Content})
		case core.CardDivider:
			elements = append(elements, map[string]any{"tag": "hr"})
		case core.CardActions:
			for _, btn := range e.Buttons {
				elements = append(elements, callbackButton(btn.Text, btn.Type, btn.Value, btn.Extra))
			}
		case core.CardListItem:
			btn := callbackButton(e.BtnText, e.BtnType, e.BtnValue, e.Extra)
			if options > 0 {
				btn["margin"] = compactOptionGap
			}
			options++
			elements = append(elements, btn)
			if e.Text != "" {
				elements = append(elements, grey(e.Text))
			}
		case core.CardNote:
			elements = append(elements, grey(e.Text))
		}
	}
	if len(elements) == 0 {
		elements = []map[string]any{{"tag": "markdown", "content": " "}}
	}
	result := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "default"},
		"body": map[string]any{
			"vertical_spacing": compactCardSpacing,
			"elements":         elements,
		},
	}
	if card.Header != nil && card.Header.Title != "" {
		color := card.Header.Color
		if color == "" {
			color = "blue"
		}
		result["header"] = map[string]any{"title": plainText(card.Header.Title), "template": color}
	}
	return result
}

type deleteModeCheckerRow struct {
	id      string
	text    string
	checked bool
}

// renderDeleteModeCheckerCard turns the delete-mode session list into a 2.0
// form: one checker per session, submitted at once.
func renderDeleteModeCheckerCard(card *core.Card) (map[string]any, bool) {
	if card == nil {
		return nil, false
	}

	formRowElements := make([]map[string]any, 0)
	notes := make([]core.CardNote, 0)
	navRows := make([]core.CardActions, 0)
	submitText := ""
	cancelText := ""

	for _, elem := range card.Elements {
		switch e := elem.(type) {
		case core.CardListItem:
			id, selectable, ok := parseDeleteModeListItemAction(e.BtnValue)
			if !ok {
				return nil, false
			}
			text := normalizeDeleteModeCheckerText(e.Text)
			if !selectable {
				formRowElements = append(formRowElements, map[string]any{
					"tag":     "markdown",
					"content": "▶ " + text,
				})
				continue
			}
			row := deleteModeCheckerRow{
				id:      id,
				text:    text,
				checked: strings.Contains(e.Text, "☑"),
			}
			formRowElements = append(formRowElements, map[string]any{
				"tag":     "checker",
				"name":    deleteModeCheckerName(row.id),
				"checked": row.checked,
				"text": map[string]any{
					"tag":     "lark_md",
					"content": row.text,
				},
			})
		case core.CardNote:
			notes = append(notes, e)
		case core.CardActions:
			remaining := make([]core.CardButton, 0, len(e.Buttons))
			for _, btn := range e.Buttons {
				switch btn.Value {
				case "act:/delete-mode confirm":
					submitText = btn.Text
				case "act:/delete-mode cancel":
					cancelText = btn.Text
				default:
					remaining = append(remaining, btn)
				}
			}
			if len(remaining) > 0 {
				navRows = append(navRows, core.CardActions{Buttons: remaining, Layout: e.Layout})
			}
		case core.CardMarkdown, core.CardDivider, core.CardSelect:
			return nil, false
		}
	}

	if len(formRowElements) == 0 || submitText == "" {
		return nil, false
	}

	elements := make([]map[string]any, 0, len(notes)+1+len(navRows))
	for _, n := range notes {
		if n.Text == "" {
			continue
		}
		if n.Tag == "delete-mode-selected-count" {
			continue
		}
		elements = append(elements, v2Note(n.Text))
	}
	submit := v2Button(core.CardButton{Text: submitText, Type: "danger", Value: "act:/delete-mode form-submit"}, "")
	submit["name"] = "delete_mode_submit"
	submit["form_action_type"] = "submit"
	formElements := append(append([]map[string]any{}, formRowElements...), submit)
	elements = append(elements, map[string]any{
		"tag":      "form",
		"name":     "delete_mode_form",
		"elements": formElements,
	})
	// Cancel sits under the form: a plain callback, not a form action.
	if cancelText != "" {
		cancel := v2Button(core.CardButton{Text: cancelText, Value: "act:/delete-mode cancel"}, "")
		cancel["name"] = "delete_mode_cancel"
		elements = append(elements, cancel)
	}
	for _, row := range navRows {
		var buttons []map[string]any
		for _, btn := range row.Buttons {
			buttons = append(buttons, v2Button(btn, ""))
		}
		if len(buttons) > 0 {
			elements = append(elements, v2ButtonRow(buttons, row.Layout))
		}
	}
	return v2CardMap(card.Header, elements), true
}

func normalizeDeleteModeCheckerText(text string) string {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{"☑ ▶", "◻ ▶", "▶", "☑", "◻"} {
		if strings.HasPrefix(trimmed, prefix) {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
			break
		}
	}
	return trimmed
}

func parseDeleteModeListItemAction(action string) (id string, selectable bool, ok bool) {
	const (
		togglePrefix = "act:/delete-mode toggle "
		noopPrefix   = "act:/delete-mode noop "
	)
	switch {
	case strings.HasPrefix(action, togglePrefix):
		id = strings.TrimSpace(strings.TrimPrefix(action, togglePrefix))
		return id, true, id != ""
	case strings.HasPrefix(action, noopPrefix):
		id = strings.TrimSpace(strings.TrimPrefix(action, noopPrefix))
		return id, false, id != ""
	default:
		return "", false, false
	}
}

// renderCard converts a core.Card into the Feishu Interactive Card JSON string.
func renderCard(card *core.Card, sessionKey string) string {
	return marshalCard(renderCardMap(card, sessionKey))
}

func marshalCard(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		slog.Error("feishu: renderCard marshal failed", "error", err)
		return `{"config":{"width_mode":"default"},"elements":[]}`
	}
	return string(b)
}
