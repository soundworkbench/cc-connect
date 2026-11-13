package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// Secret cards (`cc-connect send --secret <name>`): the assistant asks for a
// password or token with a card in the private chat. The value is typed into
// a password box and written to ~/.secrets/<name>; the card only ever shows
// the name, and the session is told the name and how to read the file.

const (
	secretField     = "secret"
	secretHintMax   = 200
	secretTooLong   = "超过 1000 字或带换行的内容（如私钥）放不进这个框，请按卡片下方的说明填写"
	secretEmptyText = "请先把内容粘贴进框里"
	secretSaveFail  = "保存没成功，请稍后再试"
)

// ErrNotPrivateChat is returned by PrivateChatResolver for a group or topic
// chat.
var ErrNotPrivateChat = errors.New("not a private chat")

// PrivateChatResolver is implemented by platforms that can tell a private
// chat with the bot from a group. PrivateChatUser returns the person a
// private-chat session belongs to, ErrNotPrivateChat for a group, or another
// error when it cannot tell.
type PrivateChatResolver interface {
	PrivateChatUser(ctx context.Context, sessionKey string) (string, error)
}

// SecretMasker is implemented by platforms that mask saved secret values in
// everything they send. The engine hands over its store when the platform
// starts.
type SecretMasker interface {
	SetSecretStore(s *SecretStore)
}

// SecretRequest is the JSON body for POST /secret.
type SecretRequest struct {
	Project    string `json:"project,omitempty"`
	SessionKey string `json:"session_key,omitempty"`
	Name       string `json:"name"`
	Hint       string `json:"hint,omitempty"` // one line: what the secret is for
	URL        string `json:"url,omitempty"`  // optional web page for long or multi-line values
}

// SetSecretStore replaces the engine's secret store (tests, embedding).
func (e *Engine) SetSecretStore(s *SecretStore) { e.secrets = s }

// SecretStore is where this engine keeps saved secrets.
func (e *Engine) SecretStore() *SecretStore { return e.secrets }

// SendSecretCard sends a secret card to a private-chat session. It reports
// whether a secret with this name is already saved (the card says it will be
// replaced).
func (e *Engine) SendSecretCard(req SecretRequest) (bool, error) {
	name := strings.TrimSpace(req.Name)
	if !ValidSecretName(name) {
		return false, secretNameError(name)
	}
	if e.secrets == nil {
		return false, errors.New("secret cards need a home directory for ~/.secrets")
	}
	hint := strings.Join(strings.Fields(req.Hint), " ")
	if utf8.RuneCountInString(hint) > secretHintMax {
		return false, fmt.Errorf("hint is too long (at most %d characters)", secretHintMax)
	}
	url := strings.TrimSpace(req.URL)
	if url != "" && (!strings.HasPrefix(url, "https://") || strings.ContainsAny(url, " ()<>")) {
		return false, errors.New("url must be an https:// address")
	}
	if req.SessionKey == "" {
		return false, errors.New("session_key is required")
	}
	_, p, replyCtx, err := e.resolveOutboundSessionTarget(req.SessionKey, false)
	if err != nil {
		return false, err
	}
	resolver, ok := p.(PrivateChatResolver)
	if !ok {
		return false, fmt.Errorf("platform %s cannot tell a private chat from a group, so it gets no secret cards: %w", p.Name(), ErrNotSupported)
	}
	owner, err := resolver.PrivateChatUser(e.ctx, req.SessionKey)
	if errors.Is(err, ErrNotPrivateChat) {
		return false, errors.New("secret cards are only sent in a private chat, and this session is a group chat. " +
			"Do not ask for the value here: ask the person to message you in their private chat with the bot, and send the card there")
	}
	if err != nil {
		return false, fmt.Errorf("cannot confirm this is a private chat: %w", err)
	}
	if owner == "" {
		return false, errors.New("cannot tell who this private chat belongs to")
	}
	sender, ok := p.(CardSender)
	if !ok {
		return false, fmt.Errorf("platform %s: %w", p.Name(), ErrNotSupported)
	}
	exists := e.secrets.Exists(name)
	if err := e.waitOutgoing(p); err != nil {
		return false, err
	}
	card := secretCard(name, hint, url, owner, exists)
	if err := sender.SendCard(e.ctx, replyCtx, e.renderCardForPlatform(p, card)); err != nil {
		return false, err
	}
	slog.Info("secret: card sent", "name", name, "replaces", exists)
	return exists, nil
}

// secretCard is the pending card: the name, what it is for, a password box
// and a save button that only the owner can use.
func secretCard(name, hint, url, owner string, exists bool) *Card {
	cb := NewCard().Title("填写密钥", "blue")
	text := "**" + name + "**"
	if hint != "" {
		text += "\n" + hint
	}
	if exists {
		text += "\n已经保存过，保存后替换旧的。"
	}
	cb.Markdown(text)
	cb.card.Elements = append(cb.card.Elements, CardForm{
		Name: "secret_form",
		Fields: []CardFormField{{Name: secretField, Kind: "input", Placeholder: "粘贴到这里", Required: true,
			Password: true, MaxLength: secretCardMaxLen}},
		SubmitText:  "保存",
		SubmitType:  "primary",
		SubmitValue: "act:/secret-submit " + name,
		Owner:       owner,
	})
	long := "超过 1000 字或带换行的内容（如私钥）放不进这个框，请回复我换个方式填写。"
	if url != "" {
		long = "超过 1000 字或带换行的内容（如私钥）放不进这个框，请用[网页填写](" + url + ")。"
	}
	cb.Note("保存在你自己的工作伙伴里，不进聊天记录。" + long)
	c := cb.Build()
	c.Shared = true
	return c
}

// secretSavedCard replaces the card once the value is saved. It never shows
// the value, nor any part of it.
func secretSavedCard(name string, updated bool, at time.Time) *Card {
	verb := "已保存"
	if updated {
		verb = "已更新"
	}
	cb := NewCard().Title("✅ "+verb+"："+name, "green")
	cb.Note(decisionClock(at) + " " + verb + "。要换或删掉，跟我说一声就行")
	c := cb.Build()
	c.Shared = true
	return c
}

// secretNotice is the message the session receives after a save. It names
// the secret and never carries the value.
func (e *Engine) secretNotice(name string, updated bool) string {
	verb := "保存"
	if updated {
		verb = "更新"
	}
	path := e.secrets.Dir() + "/" + name
	if e.secrets.Dir() == DefaultSecretDir() {
		path = "~/.secrets/" + name
	}
	return fmt.Sprintf("【密钥已%s】用户已%s密钥 %s，命令里用 $(cat %s) 读取，不要显示内容。", verb, verb, name, path)
}

// handleSecretSubmit handles "act:/secret-submit <name> <values>". The
// platform has already checked that the person submitting owns the card.
func (e *Engine) handleSecretSubmit(args, sessionKey string) *Card {
	name, raw, _ := strings.Cut(strings.TrimSpace(args), " ")
	if !ValidSecretName(name) || e.secrets == nil || sessionKey == "" {
		cardActionDropped("secret", name, "bad_request")
		return nil
	}
	values := parseFormValues("secret", name, raw)
	value, _ := values[secretField].(string)
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return &Card{Toast: secretEmptyText}
	case strings.ContainsAny(value, "\r\n") || strings.Contains(value, "-----BEGIN") ||
		utf8.RuneCountInString(value) > secretCardMaxLen:
		// A private key pasted into the one-line box loses its line breaks.
		return &Card{Toast: secretTooLong}
	}
	updated, err := e.secrets.Save(name, value)
	if err != nil {
		slog.Error("secret: save failed", "name", name, "error", err)
		return &Card{Toast: secretSaveFail}
	}
	slog.Info("secret: saved", "name", name, "updated", updated)
	notice := e.secretNotice(name, updated)
	go e.deliverSecretNotice(sessionKey, name, notice)
	return secretSavedCard(name, updated, time.Now())
}

// deliverSecretNotice hands the notice to the session as the bridge's own
// follow-up, retrying a few times while the session cannot take it.
func (e *Engine) deliverSecretNotice(sessionKey, name, notice string) {
	deliver := e.secretNotify
	if deliver == nil {
		silent := true
		deliver = func(sessionKey, text string) error {
			return e.ExecuteTimerJob(&TimerJob{
				ID:          "secret-" + name,
				Project:     e.name,
				SessionKey:  sessionKey,
				ScheduledAt: time.Now(),
				Prompt:      text,
				Description: "secret " + name,
				Silent:      &silent,
				Urgent:      true,
			})
		}
	}
	for attempt := 1; ; attempt++ {
		err := deliver(sessionKey, notice)
		if err == nil {
			return
		}
		slog.Warn("secret: notice delivery failed", "name", name, "attempt", attempt, "error", err)
		if attempt >= 5 {
			return
		}
		select {
		case <-e.ctx.Done():
			return
		case <-time.After(decisionRetryInterval):
		}
	}
}

// maskStreaming wraps a preview transform so a saved value never shows in a
// growing preview, not even its first characters.
func (e *Engine) maskStreaming(transform func(string) string) func(string) string {
	if e.secrets == nil {
		return transform
	}
	s := e.secrets
	return func(text string) string {
		if transform != nil {
			text = transform(text)
		}
		return s.MaskStreaming(text)
	}
}
