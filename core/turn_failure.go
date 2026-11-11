package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// RetryStatus describes one automatic retry of a failed model request
// (EventRetry). Every field is optional: agents report what they know.
type RetryStatus struct {
	Attempt    int    // 1-based retry number; 0 = unknown
	MaxRetries int    // 0 = unknown
	HTTPStatus int    // 0 = no HTTP response (timeout, connection error)
	Kind       string // agent-reported error kind, e.g. "rate_limit", "overloaded"
}

// UsageLimited reports a quota or rate limit rather than an unreachable service.
func (r *RetryStatus) UsageLimited() bool {
	return r != nil && (r.HTTPStatus == 429 || r.Kind == "rate_limit")
}

func (r *RetryStatus) status() int {
	if r == nil {
		return 0
	}
	return r.HTTPStatus
}

func (r *RetryStatus) attempt() int {
	if r == nil {
		return 0
	}
	return r.Attempt
}

var (
	errAgentExited      = errors.New("agent process exited")
	errAgentIdleTimeout = errors.New("agent session timed out (no response)")
)

// ModelFailure is the error of a turn the model service did not answer after
// the agent's own retries ran out.
type ModelFailure struct {
	HTTPStatus int    // 0 = unknown
	Kind       string // agent-reported error kind, e.g. "rate_limit", "server_error"
	Detail     string // agent's error text, for logs
}

func (f *ModelFailure) Error() string {
	var parts []string
	if f.HTTPStatus != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", f.HTTPStatus))
	}
	if f.Kind != "" {
		parts = append(parts, f.Kind)
	}
	if f.Detail != "" {
		parts = append(parts, f.Detail)
	}
	if len(parts) == 0 {
		return "model request failed"
	}
	return "model request failed: " + strings.Join(parts, ": ")
}

// UsageLimited reports a quota or rate limit rather than an unreachable service.
func (f *ModelFailure) UsageLimited() bool {
	return f != nil && (f.HTTPStatus == 429 || f.Kind == "rate_limit")
}

// Unreachable reports a failure of the service itself (overloaded, server
// error, no response) rather than a problem with the request or the account.
func (f *ModelFailure) Unreachable() bool {
	if f == nil || f.UsageLimited() {
		return false
	}
	switch f.Kind {
	case "", "overloaded", "server_error", "unknown":
		return f.HTTPStatus == 0 || f.HTTPStatus >= 500
	}
	return false
}

// PreviewNoticeSetter is implemented by segmented previews that can show a
// temporary status line (a retry, a long wait for the model) in place of the
// running step. An empty text restores the normal running step.
type PreviewNoticeSetter interface {
	SetPreviewNotice(ctx context.Context, previewHandle any, text string) error
}

// FailureCard is what a failed turn's card settles to.
type FailureCard struct {
	Title       string // one line saying the turn failed and, when known, why
	Note        string // optional grey line under the title
	ButtonLabel string
	Action      string // callback value of the retry button; empty = no button
	SessionKey  string // carried in the button value so the callback finds the session
}

// FailurePreviewer is implemented by segmented previews that can settle the
// live card as a failure. The card stays in the chat; it is not recalled.
type FailurePreviewer interface {
	FailPreview(ctx context.Context, previewHandle any, card FailureCard) error
}

// silenceNoticeAfter is how long a turn may wait for the model, with no event
// at all, before the card says it is still waiting. It only changes the card.
var silenceNoticeAfter = 90 * time.Second

// failedTurnTTL bounds how long a retry button stays usable.
const failedTurnTTL = 24 * time.Hour

// retryNotice is the card line for an EventRetry.
func retryNotice(i *I18n, r *RetryStatus) string {
	if r.UsageLimited() {
		return i.T(MsgPreviewUsageLimitWait)
	}
	if r != nil && r.Attempt > 0 {
		return i.Tf(MsgPreviewRetrying, r.Attempt)
	}
	return i.T(MsgPreviewRetryingBare)
}

// setNotice shows text in place of the running step; "" restores the step.
func (sp *streamPreview) setNotice(text string) {
	setter, ok := sp.segmented.(PreviewNoticeSetter)
	if !ok {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.notice == text || sp.previewMsgID == nil || sp.hidden || sp.degraded {
		return
	}
	if err := setter.SetPreviewNotice(sp.ctx, sp.previewMsgID, text); err != nil {
		slog.Warn("preview notice failed", "error", err)
		return
	}
	sp.notice = text
}

// turnWatch decides what the live card says while the turn waits: a retry
// notice, the long-silence notice, or nothing. It is owned by one turn loop.
type turnWatch struct {
	timer       *time.Timer
	waitingTool bool // a tool, a permission or the user is what the turn waits on, not the model
}

func newTurnWatch(enabled bool) *turnWatch {
	w := &turnWatch{}
	if enabled {
		w.timer = time.NewTimer(silenceNoticeAfter)
	}
	return w
}

func (w *turnWatch) C() <-chan time.Time {
	if w.timer == nil {
		return nil
	}
	return w.timer.C
}

func (w *turnWatch) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
}

// observe records one turn event and returns the notice the card should show
// ("" = the normal running step).
func (w *turnWatch) observe(i *I18n, ev Event) string {
	if w.timer != nil {
		if !w.timer.Stop() {
			select {
			case <-w.timer.C:
			default:
			}
		}
		w.timer.Reset(silenceNoticeAfter)
	}
	switch ev.Type {
	case EventRetry:
		w.waitingTool = false
		return retryNotice(i, ev.Retry)
	case EventToolUse, EventPermissionRequest, EventUserInputRequest:
		w.waitingTool = true
	default:
		w.waitingTool = false
	}
	return ""
}

// silent returns the notice for a turn that saw no event for silenceNoticeAfter,
// or "" while a tool, a permission request or the user is what it waits on.
func (w *turnWatch) silent(i *I18n) string {
	if w.waitingTool {
		return ""
	}
	return i.T(MsgPreviewWaitingModel)
}

type failedTurn struct {
	platform Platform
	input    Message
	handle   any
	card     FailureCard
	expires  time.Time
}

type failedTurns struct {
	mu    sync.Mutex
	turns map[string]*failedTurn
}

func (f *failedTurns) put(token string, t *failedTurn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.turns == nil {
		f.turns = make(map[string]*failedTurn)
	}
	now := time.Now()
	for k, v := range f.turns {
		if now.After(v.expires) {
			delete(f.turns, k)
		}
	}
	f.turns[token] = t
}

// take removes and returns the turn, so one button admits one retry.
func (f *failedTurns) take(token string) *failedTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.turns[token]
	delete(f.turns, token)
	if t == nil || time.Now().After(t.expires) {
		return nil
	}
	return t
}

func randomToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// retryableInput copies what a fresh admission of msg needs. Engine-internal
// admission state and the platform message ID stay behind: the retry is a new
// request, and the old ID would be dropped as a duplicate.
func retryableInput(msg *Message) *Message {
	return &Message{
		SessionKey:       msg.SessionKey,
		Platform:         msg.Platform,
		ChannelID:        msg.ChannelID,
		UserID:           msg.UserID,
		UserName:         msg.UserName,
		ChatName:         msg.ChatName,
		Content:          msg.Content,
		Images:           msg.Images,
		Files:            msg.Files,
		ChannelKey:       msg.ChannelKey,
		LegacyChannelKey: msg.LegacyChannelKey,
		ReplyCtx:         msg.ReplyCtx,
		FromVoice:        msg.FromVoice,
	}
}

func (q queuedMessage) retryableInput() *Message {
	return &Message{
		SessionKey: q.msgSessionKey,
		Platform:   q.msgPlatform,
		UserID:     q.userID,
		UserName:   q.userName,
		Content:    q.content,
		Images:     q.images,
		Files:      q.files,
		ChannelKey: q.channelKey,
		ReplyCtx:   q.replyCtx,
		FromVoice:  q.fromVoice,
	}
}

// failureTitle says what went wrong in one line; note carries the raw error for
// failures that are not the model service's.
func (e *Engine) failureTitle(cause error, display string) (title, note string) {
	var mf *ModelFailure
	if errors.As(cause, &mf) {
		switch {
		case mf.UsageLimited():
			return e.i18n.T(MsgTurnFailedLimit), ""
		case mf.Unreachable():
			return e.i18n.T(MsgTurnFailedModel), ""
		}
		if mf.Detail != "" {
			display = mf.Detail
		}
	}
	return e.i18n.T(MsgTurnFailed), clipRunes(display, 200)
}

// failTurnPreview settles the turn's live card as a failure with a retry
// button. It returns false when there is no live card to settle; the caller
// then reports the failure its usual way.
func (e *Engine) failTurnPreview(state *interactiveState, sp *streamPreview, sessionKey string, cause error, display string, turnStart time.Time) bool {
	fp, ok := sp.segmented.(FailurePreviewer)
	if !ok || sp.hidden {
		return false
	}
	state.mu.Lock()
	input := state.turnInput
	state.mu.Unlock()

	card := FailureCard{SessionKey: sessionKey}
	card.Title, card.Note = e.failureTitle(cause, display)
	token := ""
	if input != nil {
		if t, err := randomToken(); err == nil {
			token = t
			card.ButtonLabel = e.i18n.T(MsgRetryButton)
			card.Action = "act:/retry " + token
		} else {
			slog.Warn("retry token failed", "error", err)
		}
	}

	sp.mu.Lock()
	handle := sp.previewMsgID
	if handle == nil || sp.degraded {
		sp.mu.Unlock()
		return false
	}
	sp.cancelTimerLocked()
	select {
	case <-sp.timerStop:
	default:
		close(sp.timerStop)
	}
	if token != "" {
		e.failedTurns.put(token, &failedTurn{platform: sp.platform, input: *input, handle: handle, card: card, expires: time.Now().Add(failedTurnTTL)})
	}
	if err := fp.FailPreview(sp.ctx, handle, card); err != nil {
		sp.mu.Unlock()
		if token != "" {
			e.failedTurns.take(token)
		}
		slog.Warn("failure card failed", "error", err)
		return false
	}
	sp.previewMsgID = nil
	sp.degraded = true
	p, replyCtx := sp.platform, sp.replyCtx
	sp.mu.Unlock()

	StreamTrace("turn_end", map[string]any{"session": sessionKey, "outcome": "failed"})
	// Editing a card raises no notification; after a long wait the person
	// has likely looked away, so the failure also arrives as a message.
	if time.Since(turnStart) >= time.Minute {
		e.send(p, replyCtx, card.Title)
	}
	return true
}

// retryFailedTurn handles the failure card's retry button: the original
// request is admitted again, once.
func (e *Engine) retryFailedTurn(token string) {
	t := e.failedTurns.take(strings.TrimSpace(token))
	if t == nil {
		return
	}
	if fp, ok := t.platform.(FailurePreviewer); ok {
		card := t.card
		card.Note, card.ButtonLabel, card.Action = e.i18n.T(MsgRetrySent), "", ""
		if err := fp.FailPreview(e.ctx, t.handle, card); err != nil {
			slog.Warn("failure card update failed", "error", err)
		}
	}
	msg := t.input
	go e.ReceiveMessage(t.platform, &msg)
}
