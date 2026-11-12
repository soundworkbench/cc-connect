package core

import (
	"log/slog"
	"strings"
)

// AskUserQuestion without holding the turn: when decision cards are on, each
// question the agent asks goes out as a decision card with no recommended
// option and no deadline ("需要你选择"), and the tool is answered at once so
// the agent ends its turn or moves on. The pick arrives later as an ordinary
// message, "【决定】<question>：<option>", like any decision card. Without
// decision cards, or when the first card cannot be sent, the blocking ask
// card is used as before.

const askNoteRunes = 200

// askDecisionRequests turns the questions into decision requests; a question
// a decision card cannot carry (fewer than two options, too many) makes the
// whole call fall back.
func askDecisionRequests(sessionKey string, questions []UserQuestion) []DecisionRequest {
	var reqs []DecisionRequest
	for _, q := range questions {
		question := strings.TrimSpace(q.Question)
		if question == "" {
			question = strings.TrimSpace(q.Header)
		}
		req := DecisionRequest{SessionKey: sessionKey, Question: question, NoDefault: true, Multi: q.MultiSelect}
		var notes []string
		for _, o := range q.Options {
			label := strings.TrimSpace(o.Label)
			req.Options = append(req.Options, label)
			if desc := strings.TrimSpace(o.Description); desc != "" {
				notes = append(notes, label+"："+desc)
			}
		}
		req.Note = clipRunes(strings.Join(notes, "；"), askNoteRunes)
		d := Decision{Question: req.Question, Options: req.Options}
		if d.validate() != nil {
			return nil
		}
		reqs = append(reqs, req)
	}
	return reqs
}

// askAsDecisions sends an AskUserQuestion call as decision cards and answers
// the tool at once. It reports false when the caller should show the blocking
// ask card instead; nothing has been sent then.
func (e *Engine) askAsDecisions(agent AgentSession, sessionKey string, event *Event) bool {
	if e.decisions == nil || agent == nil || event.ToolName != "AskUserQuestion" {
		return false
	}
	reqs := askDecisionRequests(sessionKey, event.Questions)
	if len(reqs) == 0 {
		return false
	}
	var failed []string
	for n, req := range reqs {
		if _, err := e.SendDecisionCard(req); err != nil {
			slog.Warn("ask: decision card not sent", "session", sessionKey, "question", n+1, "error", err)
			if n == 0 {
				return false
			}
			failed = append(failed, req.Question)
		}
	}
	msg := e.i18n.T(MsgAskSentAsCards)
	if len(failed) > 0 {
		msg += e.i18n.Tf(MsgAskCardsNotSent, strings.Join(failed, "；"))
	}
	if err := agent.RespondPermission(event.RequestID, PermissionResult{Behavior: "deny", Message: msg}); err != nil {
		slog.Warn("ask: tool answer failed", "session", sessionKey, "error", err)
	}
	slog.Info("ask: sent as decision cards", "session", sessionKey, "cards", len(reqs)-len(failed))
	return true
}
