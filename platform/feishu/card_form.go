package feishu

import "github.com/chenhg5/cc-connect/core"

// renderConfirm is a button's confirm dialog; Feishu requires both texts.
func renderConfirm(c *core.CardConfirm) map[string]any {
	if c == nil || c.Title == "" || c.Text == "" {
		return nil
	}
	return map[string]any{"title": plainText(c.Title), "text": plainText(c.Text)}
}
