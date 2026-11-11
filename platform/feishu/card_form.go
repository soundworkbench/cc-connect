package feishu

import "github.com/chenhg5/cc-connect/core"

// renderConfirm is a button's confirm dialog; Feishu requires both texts.
func renderConfirm(c *core.CardConfirm) map[string]any {
	if c == nil || c.Title == "" || c.Text == "" {
		return nil
	}
	return map[string]any{"title": plainText(c.Title), "text": plainText(c.Text)}
}

// renderForm renders a core.CardForm as a form container. Each field and the
// submit button carry a name, as Feishu requires; the submit callback brings
// the values in form_value, keyed by field name (see onCardAction).
func renderForm(f core.CardForm, sessionKey string) map[string]any {
	var els []map[string]any
	for _, field := range f.Fields {
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
		els = append(els, el)
	}
	value := map[string]string{"action": f.SubmitValue}
	if sessionKey != "" {
		value["session_key"] = sessionKey
	}
	submitType := f.SubmitType
	if submitType == "" {
		submitType = "primary"
	}
	submit := map[string]any{
		"tag":              "button",
		"text":             plainText(f.SubmitText),
		"type":             submitType,
		"name":             f.Name + "_submit",
		"form_action_type": "submit",
		"value":            value,
	}
	if c := renderConfirm(f.Confirm); c != nil {
		submit["confirm"] = c
	}
	els = append(els, map[string]any{
		"tag":              "column_set",
		"horizontal_align": "left",
		"columns": []map[string]any{{
			"tag": "column", "width": "auto", "vertical_align": "center",
			"elements": []map[string]any{submit},
		}},
	})
	return map[string]any{"tag": "form", "name": f.Name, "elements": els}
}
