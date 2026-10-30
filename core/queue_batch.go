package core

import (
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// QueueBatchConfig is resolved at startup; a zero Hold disables idle waiting.
type QueueBatchConfig struct {
	Enabled          bool
	MaxMessages      int
	MaxChars         int
	Hold             time.Duration
	ImmediateSources []string
}

func DefaultQueueBatchConfig() QueueBatchConfig {
	return QueueBatchConfig{Enabled: true, MaxMessages: 20, MaxChars: 20000, Hold: 60 * time.Second, ImmediateSources: []string{"human"}}
}

func (e *Engine) SetQueueBatchConfig(cfg QueueBatchConfig) {
	if cfg.MaxMessages <= 0 {
		cfg.MaxMessages = 20
	}
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = 20000
	}
	cfg.ImmediateSources = append([]string(nil), cfg.ImmediateSources...)
	e.queueBatch = cfg
}

type queueHold struct {
	until time.Time
	wake  chan struct{}
}

func messageSource(msg *Message) string {
	if source := strings.TrimSpace(msg.Source); source != "" {
		return strings.ToLower(source)
	}
	if msg.scheduledPrompt {
		kind, _, _ := strings.Cut(msg.scheduledInputID, ":")
		switch kind {
		case "timer", "cron", "assistant", "rotation":
			return kind
		}
		return "scheduled"
	}
	if strings.EqualFold(msg.UserID, "relay") || strings.HasPrefix(msg.UserID, "relay:") {
		return "relay"
	}
	return "human"
}

// automationUserIDs are the sender IDs engine-side producers stamp on the
// messages they inject (engine_timer, engine_cron, engine_heartbeat, webhook,
// assistant_api). messageSource does not know heartbeat or webhook.
var automationUserIDs = map[string]bool{"timer": true, "cron": true, "heartbeat": true, "webhook": true, "assistant": true, "relay": true}

// isHumanTurn reports whether a person typed this message in chat. Anything
// carrying an automation marker is not: scheduler/assistant admission
// (timer, cron, relay via timer, assistant inject, rotation), an explicit
// non-human Source, app-sent Feishu messages (relay), and the synthetic
// heartbeat/webhook senders. An unmarked platform message counts as human; in
// this deployment that is exactly a Feishu message from a real user.
func isHumanTurn(msg *Message) bool {
	if msg != nil && msg.humanBatch {
		return true
	}
	if msg == nil || msg.scheduledPrompt || messageSource(msg) != "human" {
		return false
	}
	id := strings.ToLower(strings.TrimSpace(msg.UserID))
	return !automationUserIDs[id] && !strings.HasPrefix(id, "relay:")
}

// isHumanQueuedTurn treats a merged batch as human when any part is human, so
// a person's message never loses its live preview by riding along with a tell.
func isHumanQueuedTurn(q queuedMessage) bool {
	if len(q.mergedFrom) == 0 {
		return isHumanTurn(queuedInput(q))
	}
	for _, m := range q.mergedFrom {
		if isHumanTurn(queuedInput(m)) {
			return true
		}
	}
	return false
}

func (e *Engine) shouldHold(msg *Message) bool {
	if !e.queueBatch.Enabled || e.queueBatch.Hold <= 0 || msg.Urgent || msg.individualReply || msg.ModeOverride != "" || msg.IsPermissionResponse || msg.preflightSent || strings.HasPrefix(strings.TrimSpace(msg.Content), "/") || strings.HasPrefix(strings.TrimSpace(msg.Content), "!") {
		return false
	}
	source := messageSource(msg)
	if source == "human" || source == "rotation" {
		return false
	}
	for _, immediate := range e.queueBatch.ImmediateSources {
		if strings.EqualFold(source, strings.TrimSpace(immediate)) {
			return false
		}
	}
	return true
}

// The caller owns the session lock. Admission persists before the timer starts.
func (e *Engine) holdIdleMessage(p Platform, msg *Message, session *Session, sessions *SessionManager, agent Agent, key, workspace string) (bool, error) {
	if !e.shouldHold(msg) {
		return false, nil
	}
	e.interactiveMu.Lock()
	state := e.ensureInteractiveStateForQueueingLocked(key, p, msg.ReplyCtx, session)
	state.mu.Lock()
	state.workspaceDir = workspace
	hold := &queueHold{until: time.Now().Add(e.queueBatch.Hold), wake: make(chan struct{}, 1)}
	state.hold = hold
	if state.stopCh == nil {
		state.stopCh = make(chan struct{})
	}
	state.mu.Unlock()
	e.interactiveMu.Unlock()
	handled, err := e.admitBusyMessage(p, msg, key, session, sessions)
	if err != nil || !handled {
		state.mu.Lock()
		if state.hold == hold {
			state.hold = nil
		}
		state.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("queue admission failed")
		}
		return true, err
	}
	if !e.launchAcceptedWork(func() { e.drainOrphanedQueue(session, sessions, key, agent, workspace) }) {
		session.Unlock()
	}
	return true, nil
}

func (e *Engine) waitQueueHold(state *interactiveState) {
	state.mu.Lock()
	hold, stop := state.hold, state.stopCh
	state.mu.Unlock()
	if hold == nil {
		return
	}
	timer := time.NewTimer(time.Until(hold.until))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-hold.wake:
	case <-stop:
	case <-e.ctx.Done():
	}
	state.mu.Lock()
	if state.hold == hold {
		state.hold = nil
	}
	state.mu.Unlock()
}

func (e *Engine) wakeQueueHoldLocked(state *interactiveState, msg *Message) {
	if state.hold != nil && !e.shouldHold(msg) {
		select {
		case state.hold.wake <- struct{}{}:
		default:
		}
	}
}

func batchable(q queuedMessage) bool {
	text := strings.TrimSpace(q.content)
	return q.modeOverride == "" && !q.separateReply && !strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "!") && q.platform != nil
}

func (e *Engine) batchContent(messages []queuedMessage) string {
	var b strings.Builder
	for i, q := range messages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		source := q.source
		if source == "" {
			source = messageSource(queuedInput(q))
		}
		label := source
		if q.scheduledInputID != "" {
			label += " " + q.scheduledInputID
		} else if q.msgPlatform != "" {
			label += " " + q.msgPlatform
		}
		if q.userName != "" {
			label += " / " + q.userName
		}
		if label == "human" {
			label = e.i18n.T(MsgQueueBatchSource)
		}
		at := q.queuedAt
		if q.userMessageTimeMs > 0 {
			at = time.UnixMilli(q.userMessageTimeMs)
		}
		stamp := "?"
		if !at.IsZero() {
			stamp = at.Local().Format("15:04")
		}
		b.WriteString(e.i18n.Tf(MsgQueueBatchMarker, i+1, len(messages), strings.Join(strings.Fields(label), " "), stamp))
		b.WriteByte('\n')
		b.WriteString(q.content)
	}
	return b.String()
}

func (e *Engine) queuedBatchSize(messages []queuedMessage) int {
	if !e.queueBatch.Enabled || !batchable(messages[0]) {
		return 1
	}
	n := 1
	for n < len(messages) && n < e.queueBatch.MaxMessages {
		if !batchable(messages[n]) {
			break
		}
		if utf8.RuneCountInString(e.batchContent(messages[:n+1])) > e.queueBatch.MaxChars {
			break
		}
		n++
	}
	return n
}

func (e *Engine) mergeQueuedMessages(messages []queuedMessage) queuedMessage {
	if len(messages) == 1 {
		return messages[0]
	}
	q := messages[len(messages)-1]
	q.images = nil
	q.files = nil
	q.userMessageTimeMs = 0
	q.scheduledMute = true
	for _, m := range messages {
		q.images = append(q.images, m.images...)
		q.files = append(q.files, m.files...)
		q.scheduledMute = q.scheduledMute && m.scheduledMute
		if m.userMessageTimeMs > q.userMessageTimeMs {
			q.userMessageTimeMs = m.userMessageTimeMs
		}
	}
	q.content = e.batchContent(messages)
	q.mergedFrom = append([]queuedMessage(nil), messages...)
	return q
}

func (e *Engine) acknowledgeQueuedBatch(q queuedMessage) {
	for _, m := range q.mergedFrom[:max(0, len(q.mergedFrom)-1)] {
		if m.platform.Name() == q.platform.Name() && reflect.DeepEqual(m.replyCtx, q.replyCtx) {
			continue
		}
		if _, ok := m.platform.(SteerReaction); ok {
			e.steerReaction(m.platform, m.replyCtx, nil)
		} else if done, ok := m.platform.(TypingIndicatorDone); ok {
			done.AddDoneReaction(m.replyCtx)
		}
	}
}

func (e *Engine) wakeQueueHolds() {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	for _, state := range e.interactiveStates {
		state.mu.Lock()
		if state.hold != nil {
			select {
			case state.hold.wake <- struct{}{}:
			default:
			}
		}
		state.mu.Unlock()
	}
}
