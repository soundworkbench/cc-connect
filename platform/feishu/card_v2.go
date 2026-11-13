package feishu

import (
	"strconv"

	"github.com/chenhg5/cc-connect/core"
)

// Card JSON 2.0 rendering of the classic card layout, for shared cards
// (core.Card.Shared: decision and form cards), the delete-mode form, and any
// card that answers a click on a 2.0 card. A 2.0 card is always a shared
// card, and a callback on it must be answered with a 2.0 card (Feishu rejects
// a 1.0 reply with 200830). Every callback value a 2.0 card carries is marked
// with cardSchemaKey, so the reply to a click is rendered as 2.0 too.

const (
	cardSchemaKey = "card_schema"
	cardSchemaV2  = "2"
)

// callbackFromV2 reports whether a callback came from a card rendered as 2.0.
func callbackFromV2(value map[string]any) bool {
	v, _ := value[cardSchemaKey].(string)
	return v == cardSchemaV2
}

// renderCallbackCardMap renders the card that replaces a clicked card.
func renderCallbackCardMap(card *core.Card, sessionKey string, v2 bool) map[string]any {
	if v2 && card != nil && !card.Compact {
		c := *card
		c.Shared = true
		return renderCardMap(&c, sessionKey)
	}
	return renderCardMap(card, sessionKey)
}

// v2CallbackValue is the callback value of a 2.0 component.
func v2CallbackValue(action, sessionKey string, extra map[string]string) map[string]string {
	val := map[string]string{cardSchemaKey: cardSchemaV2}
	if action != "" {
		val["action"] = action
	}
	if sessionKey != "" {
		val["session_key"] = sessionKey
	}
	for k, v := range extra {
		val[k] = v
	}
	return val
}

func v2Button(btn core.CardButton, sessionKey string) map[string]any {
	btnType := btn.Type
	if btnType == "" {
		btnType = "default"
	}
	b := map[string]any{
		"tag":       "button",
		"text":      plainText(btn.Text),
		"type":      btnType,
		"behaviors": []map[string]any{{"type": "callback", "value": v2CallbackValue(btn.Value, sessionKey, btn.Extra)}},
	}
	if c := renderConfirm(btn.Confirm); c != nil {
		b["confirm"] = c
	}
	if btn.Disabled {
		b["disabled"] = true
		if btn.DisabledTip != "" {
			b["disabled_tips"] = plainText(btn.DisabledTip)
		}
	}
	return b
}

// v2Note is the grey footnote of a 2.0 card, which has no note element.
func v2Note(text string) map[string]any {
	return map[string]any{
		"tag":       "markdown",
		"content":   "<font color='grey'>" + text + "</font>",
		"text_size": "notation",
	}
}

// v2ButtonRow lays buttons side by side: one button stands alone; several
// sit in auto-width columns that wrap on a narrow screen; equal columns share
// the width.
func v2ButtonRow(buttons []map[string]any, layout core.CardActionLayout) map[string]any {
	if len(buttons) == 1 && layout != core.CardActionLayoutEqualColumns {
		return buttons[0]
	}
	columns := make([]map[string]any, 0, len(buttons))
	for _, b := range buttons {
		col := map[string]any{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []map[string]any{b}}
		if layout == core.CardActionLayoutEqualColumns {
			b["width"] = "fill"
			col["width"], col["weight"], col["horizontal_align"] = "weighted", 1, "center"
		}
		columns = append(columns, col)
	}
	set := map[string]any{"tag": "column_set", "flex_mode": "flow", "horizontal_spacing": "8px", "columns": columns}
	if layout == core.CardActionLayoutEqualColumns {
		set["flex_mode"] = "none"
		if len(buttons) == 2 {
			set["flex_mode"] = "bisect"
		}
	}
	return set
}

// renderSharedCardMap renders the classic layout as a 2.0 shared card.
func renderSharedCardMap(card *core.Card, sessionKey string) map[string]any {
	var elements []map[string]any
	for _, elem := range card.Elements {
		switch e := elem.(type) {
		case core.CardMarkdown:
			elements = append(elements, map[string]any{"tag": "markdown", "content": e.Content})
		case core.CardDivider:
			elements = append(elements, map[string]any{"tag": "hr"})
		case core.CardActions:
			var buttons []map[string]any
			for _, btn := range e.Buttons {
				buttons = append(buttons, v2Button(btn, sessionKey))
			}
			if len(buttons) > 0 {
				elements = append(elements, v2ButtonRow(buttons, e.Layout))
			}
		case core.CardListItem:
			btn := v2Button(core.CardButton{Text: e.BtnText, Type: e.BtnType, Value: e.BtnValue, Extra: e.Extra}, sessionKey)
			elements = append(elements, map[string]any{
				"tag":       "column_set",
				"flex_mode": "none",
				"columns": []map[string]any{
					{"tag": "column", "width": "weighted", "weight": 5, "vertical_align": "center",
						"elements": []map[string]any{{"tag": "markdown", "content": e.Text}}},
					{"tag": "column", "width": "auto", "vertical_align": "center",
						"elements": []map[string]any{btn}},
				},
			})
		case core.CardSelect:
			var options []map[string]any
			for _, opt := range e.Options {
				options = append(options, map[string]any{"text": plainText(opt.Text), "value": opt.Value})
			}
			sel := map[string]any{
				"tag":         "select_static",
				"placeholder": plainText(e.Placeholder),
				"options":     options,
				"behaviors":   []map[string]any{{"type": "callback", "value": v2CallbackValue("", sessionKey, nil)}},
			}
			if e.InitValue != "" {
				sel["initial_option"] = e.InitValue
			}
			elements = append(elements, sel)
		case core.CardNote:
			if e.Text != "" {
				elements = append(elements, v2Note(e.Text))
			}
		case core.CardForm:
			elements = append(elements, renderFormV2(e, sessionKey))
		}
	}
	return v2CardMap(card.Header, elements)
}

// v2CardMap wraps body elements into a 2.0 shared card with the header.
func v2CardMap(header *core.CardHeader, elements []map[string]any) map[string]any {
	if len(elements) == 0 {
		elements = []map[string]any{{"tag": "markdown", "content": " "}}
	}
	result := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "default", "update_multi": true},
		"body":   map[string]any{"elements": elements},
	}
	if header != nil && header.Title != "" {
		color := header.Color
		if color == "" {
			color = "blue"
		}
		h := map[string]any{"title": plainText(header.Title), "template": color}
		if header.Tag != "" {
			h["text_tag_list"] = []map[string]any{{"tag": "text_tag", "text": plainText(header.Tag), "color": "neutral"}}
		}
		result["header"] = h
	}
	return result
}

// renderFormV2 renders a core.CardForm as a 2.0 form container. Each field and
// the submit button carry a name, as Feishu requires; the submit button is
// bound with form_action_type "submit" and its callback brings the button's
// value plus the fields in form_value, keyed by field name (see onCardAction).
func renderFormV2(f core.CardForm, sessionKey string) map[string]any {
	var els []map[string]any
	for _, field := range f.Fields {
		if field.Kind == "checker" {
			el := map[string]any{"tag": "checker", "name": field.Name, "checked": false,
				"text": map[string]any{"tag": "lark_md", "content": field.Label}}
			if f.Disabled {
				el["disabled"] = true
				if f.DisabledTip != "" {
					el["disabled_tips"] = plainText(f.DisabledTip)
				}
			}
			els = append(els, el)
			continue
		}
		if field.Label != "" {
			els = append(els, map[string]any{"tag": "markdown", "content": "**" + escapeCardText(field.Label) + "**"})
		}
		el := map[string]any{"name": field.Name, "required": field.Required}
		if field.Placeholder != "" {
			el["placeholder"] = plainText(field.Placeholder)
		}
		switch field.Kind {
		case "select", "multi_select":
			el["tag"] = "select_static"
			if field.Kind == "multi_select" {
				el["tag"] = "multi_select_static"
			}
			var options []map[string]any
			for _, o := range field.Options {
				options = append(options, map[string]any{"text": plainText(o.Text), "value": o.Value})
			}
			el["options"] = options
		case "date":
			el["tag"] = "date_picker"
		default:
			el["tag"] = "input"
		}
		if f.Disabled {
			el["disabled"] = true
		}
		els = append(els, el)
	}
	submitType := f.SubmitType
	if submitType == "" {
		submitType = "primary"
	}
	buttons := append([]core.CardButton{{Text: f.SubmitText, Type: submitType, Value: f.SubmitValue, Confirm: f.Confirm}}, f.More...)
	var row []map[string]any
	for i, b := range buttons {
		if f.Disabled {
			b.Disabled, b.DisabledTip, b.Confirm = true, f.DisabledTip, nil
		}
		btn := v2Button(b, sessionKey)
		btn["name"] = f.Name + "_submit"
		if i > 0 {
			btn["name"] = f.Name + "_submit" + strconv.Itoa(i+1)
		}
		btn["form_action_type"] = "submit"
		row = append(row, btn)
	}
	els = append(els, v2ButtonRow(row, core.CardActionLayoutRow))
	return map[string]any{"tag": "form", "name": f.Name, "elements": els}
}
