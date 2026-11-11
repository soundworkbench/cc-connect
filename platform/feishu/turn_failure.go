package feishu

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

var _ core.PreviewNoticeSetter = (*Platform)(nil)
var _ core.FailurePreviewer = (*Platform)(nil)

// SetPreviewNotice shows text in the running row instead of the current step
// (a retry, a long wait for the model); "" brings the step back. The push loop
// sends it with the next frame.
func (p *Platform) SetPreviewNotice(_ context.Context, handle any, text string) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok || h.live == nil {
		return core.ErrNotSupported
	}
	v := h.live
	v.mu.Lock()
	defer v.mu.Unlock()
	v.notice = stepOneLine(text, 80)
	return nil
}

// FailPreview settles the turn's card as a failure: the reply written so far,
// a red line saying what went wrong and, when the card has one, the retry
// button. The card stays where it is. Calling it again on a settled card
// rewrites the failure part (e.g. after the retry button was used).
func (p *Platform) FailPreview(ctx context.Context, handle any, c core.FailureCard) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok {
		return core.ErrNotSupported
	}
	body, streamed := "", false
	if v := h.live; v != nil {
		v.mu.Lock()
		streamed = !v.stopped && !v.fallback
		v.stopLocked()
		body = v.replyText()
		v.mu.Unlock()
	}
	if streamed && h.cardID != "" {
		p.closeSegmentedStream(ctx, h)
	}
	defer p.liveCards.remove(h.messageID)
	card := failureCardJSON(body, c)
	if h.cardID != "" {
		return p.updateCardEntity(ctx, h, card)
	}
	return p.patchCardMessage(ctx, h.messageID, card)
}

// replyText is all the reply text the card has received, revealed or not.
// Caller holds v.mu.
func (v *segmentedPreview) replyText() string {
	if v.cfg.RevealCps <= 0 {
		return v.body
	}
	v.pendMu.Lock()
	defer v.pendMu.Unlock()
	if v.pendDirty {
		return v.pend
	}
	return v.target
}

func failureCardJSON(body string, c core.FailureCard) string {
	var els []map[string]any
	if strings.TrimSpace(body) != "" {
		els = append(els, map[string]any{"tag": "markdown", "content": body})
	}
	els = append(els, map[string]any{"tag": "markdown", "content": "<font color='red'>" + escapeCardText(c.Title) + "</font>"})
	if c.Note != "" {
		els = append(els, map[string]any{"tag": "markdown", "content": "<font color='grey'>" + escapeCardText(c.Note) + "</font>", "text_size": "notation"})
	}
	if c.Action != "" {
		els = append(els, callbackButton(c.ButtonLabel, "primary", c.Action, c.SessionKey))
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "default", "summary": map[string]any{"content": c.Title}},
		"body":   map[string]any{"elements": els},
	}
	b, _ := json.Marshal(card)
	return string(b)
}

// callbackButton is a schema 2.0 button whose click arrives at onCardAction
// with value as the action.
func callbackButton(label, style, action, sessionKey string) map[string]any {
	value := map[string]string{"action": action}
	if sessionKey != "" {
		value["session_key"] = sessionKey
	}
	return map[string]any{
		"tag":       "button",
		"text":      plainText(label),
		"type":      style,
		"behaviors": []map[string]any{{"type": "callback", "value": value}},
	}
}

// escapeCardText keeps plain text from being read as card markup.
func escapeCardText(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(s)
}
