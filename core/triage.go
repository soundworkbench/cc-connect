package core

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// Front-desk triage: while no turn runs but background tasks or queued
// messages exist, the quick model sorts a short message once. A question
// about progress is answered from the bridge's own state; pure small talk or
// thanks gets an emoji. A fully idle chat skips the quick model, so a short
// command never waits on it. Everything else, and every failure, goes to the
// agent as before. A pending question always goes to the agent: "好的" or
// "可以" may be its answer.

const (
	triageMaxRunes     = 60                      // longer messages are never triaged
	triageMaxTimeout   = 2500 * time.Millisecond // the fastest quick model takes 1.1–1.3s to first token
	triageReaction     = "THUMBSUP"
	triageQuestionTail = 200 // runes of the last reply searched for a question mark
)

type triageClass string

const (
	triageProgress triageClass = "progress"
	triageGreeting triageClass = "greeting"
	triageOther    triageClass = "other"
)

const triagePrompt = "判断用户发来的这条消息属于哪一类，只回一个英文词，不要别的内容：\n" +
	"PROGRESS：在问进度、问你在忙什么、问好了没有。\n" +
	"GREETING：纯寒暄或致谢，例如“谢谢”“辛苦了”“早上好”，不带任何要求、问题或对上文的回答。\n" +
	"OTHER：其余一切，包括“好的”“可以”“行”“嗯”这类对上文的确认或回答，以及任何要求和问题。\n" +
	"拿不准就回 OTHER。"

// triageTask is a background task as the progress answer describes it.
type triageTask struct {
	title, step string
	minutes     int
}

// triageView is what the bridge knows about the session right now.
type triageView struct {
	tasks           []triageTask
	queued          int
	pendingQuestion string // why a question is pending; "" = none
}

func (v triageView) busy() bool { return len(v.tasks) > 0 || v.queued > 0 }

// triageIdleMessage handles msg without the agent when the triage allows it.
// It reports whether msg was handled. Called with the session locked and no
// turn running.
func (e *Engine) triageIdleMessage(p Platform, msg *Message, session *Session, sessions *SessionManager, interactiveKey string) bool {
	cfg := e.streamPreview.QuickLine
	content := strings.TrimSpace(msg.Content)
	if !cfg.Enabled || !isHumanTurn(msg) || msg.scheduledMute || content == "" ||
		len(msg.Images) > 0 || len(msg.Files) > 0 || msg.Audio != nil ||
		strings.HasPrefix(content, "/") || strings.HasPrefix(content, "!") ||
		utf8.RuneCountInString(content) > triageMaxRunes {
		return false
	}
	view := e.triageView(msg, session, interactiveKey)
	if !view.busy() {
		return false // nothing to wait for: the agent answers at once
	}
	if view.pendingQuestion != "" {
		triageLog(interactiveKey, triageOther, "pending_question_"+view.pendingQuestion)
		return false
	}
	class, err := e.classifyIdleMessage(cfg, content)
	switch {
	case err != nil:
		triageLog(interactiveKey, triageOther, "classifier_failed")
		slog.Info("card: triage classifier failed", "session", interactiveKey, "error", err)
		return false
	case class == triageProgress:
		answer := e.progressAnswer(view)
		if err := e.sendWithError(p, msg.ReplyCtx, answer); err != nil {
			triageLog(interactiveKey, triageOther, "answer_send_failed")
			return false
		}
		session.AddHistory("user", msg.Content)
		session.AddHistory("assistant", answer)
		sessions.Save()
		triageLog(interactiveKey, triageProgress, "answered_from_bridge_state")
		return true
	case class == triageGreeting:
		reactor, ok := p.(MessageReactor)
		if !ok {
			triageLog(interactiveKey, triageOther, "greeting_no_reactions")
			return false
		}
		if err := reactor.AddReaction(e.ctx, msg.ReplyCtx, triageReaction); err != nil {
			triageLog(interactiveKey, triageOther, "reaction_failed")
			return false
		}
		session.AddHistory("user", msg.Content)
		sessions.Save()
		triageLog(interactiveKey, triageGreeting, "reaction_only")
		return true
	default:
		triageLog(interactiveKey, triageOther, "classified_other")
	}
	return false
}

func triageLog(sessionKey string, class triageClass, reason string) {
	slog.Info("card: triage", "session", sessionKey, "class", string(class), "reason", reason)
	if StreamTraceOn() {
		StreamTrace("triage", map[string]any{"session": sessionKey, "class": string(class), "reason": reason})
	}
}

func (e *Engine) classifyIdleMessage(cfg QuickLineCfg, content string) (triageClass, error) {
	timeout := quickLineTimeout(cfg)
	if timeout > triageMaxTimeout {
		timeout = triageMaxTimeout
	}
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	out, err := fetchQuickText(ctx, cfg, triagePrompt, []map[string]string{{"role": "user", "content": content}})
	if err != nil {
		return triageOther, err
	}
	switch strings.ToUpper(strings.Trim(strings.TrimSpace(out), "。.：: \"'")) {
	case "PROGRESS":
		return triageProgress, nil
	case "GREETING":
		return triageGreeting, nil
	}
	return triageOther, nil
}

// triageView collects background tasks, queued messages and pending questions.
func (e *Engine) triageView(msg *Message, session *Session, interactiveKey string) triageView {
	var v triageView
	e.interactiveMu.Lock()
	state := e.interactiveStates[interactiveKey]
	e.interactiveMu.Unlock()
	if state != nil {
		state.mu.Lock()
		v.queued = len(state.pendingMessages)
		switch {
		case state.pending != nil:
			v.pendingQuestion = "permission_or_ask"
		case state.pendingUserInput != nil:
			v.pendingQuestion = "user_input"
		}
		state.mu.Unlock()
	}
	if v.pendingQuestion == "" && e.decisions != nil && e.decisions.hasPending(msg.SessionKey) {
		v.pendingQuestion = "decision_card"
	}
	if v.pendingQuestion == "" && lastReplyAsks(session) {
		v.pendingQuestion = "last_reply_asks"
	}
	m := e.taskCards
	m.mu.Lock()
	for _, c := range m.cards {
		if c.sessionKey == interactiveKey && c.background && !c.hidden && !c.ended {
			title := c.desc
			if title == "" {
				title = e.i18n.T(MsgTaskUntitled)
			}
			v.tasks = append(v.tasks, triageTask{title: title, step: c.step, minutes: taskMinutes(c.start)})
		}
	}
	m.mu.Unlock()
	return v
}

// lastReplyAsks reports whether the agent's last reply ends on a question,
// whose answer may be as short as "好的".
func lastReplyAsks(session *Session) bool {
	h := session.GetHistory(1)
	if len(h) == 0 || h[0].Role == "user" {
		return false
	}
	text := []rune(strings.TrimSpace(h[0].Content))
	if len(text) > triageQuestionTail {
		text = text[len(text)-triageQuestionTail:]
	}
	tail := string(text)
	return strings.ContainsAny(tail, "?？")
}

// hasPending reports whether the session has an unanswered decision or form.
func (s *DecisionStore) hasPending(sessionKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.list {
		if d.SessionKey == sessionKey && d.State == decisionPending {
			return true
		}
	}
	return false
}

// progressAnswer describes the running tasks and the queue in the bridge's
// own words; every number comes from the bridge.
func (e *Engine) progressAnswer(v triageView) string {
	i := e.i18n
	var lines []string
	for n, t := range v.tasks {
		if n == 3 {
			lines = append(lines, i.Tf(MsgTriageMoreTasks, len(v.tasks)-3))
			break
		}
		elapsed := i.T(MsgTaskElapsedShort)
		if t.minutes >= 1 {
			elapsed = i.Tf(MsgTaskElapsed, t.minutes)
		}
		step := t.step
		if step == "" {
			step = i.T(MsgTaskWorking)
		}
		lines = append(lines, i.Tf(MsgTriageTask, t.title, elapsed, step))
	}
	if len(v.tasks) > 0 {
		lines = append(lines, i.T(MsgTriageSeeCards))
	}
	if v.queued > 0 {
		lines = append(lines, i.Tf(MsgTriageQueued, v.queued))
	}
	return strings.Join(lines, "\n")
}
