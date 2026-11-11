package core

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// TaskEventKind is the step of a task's life an EventTask reports.
type TaskEventKind string

const (
	TaskStarted  TaskEventKind = "started"
	TaskProgress TaskEventKind = "progress"
	TaskUpdated  TaskEventKind = "updated"
	TaskEnded    TaskEventKind = "ended"
)

// TaskEvent is one lifecycle event of a task the agent runs beside the turn
// (a background command, a subagent). Fields an event does not carry are zero.
type TaskEvent struct {
	Kind        TaskEventKind
	TaskID      string
	Description string
	Background  *bool         // set when the event says whether the task runs in the background
	Hidden      bool          // housekeeping task the agent itself keeps out of view
	Step        string        // what the task is doing now, in the user's words
	Summary     string        // progress: one-line status; ended: result summary
	Status      string        // ended: "completed", "failed" or "stopped"
	Elapsed     time.Duration // agent-reported run time; 0 = unknown
}

// TaskStopper is implemented by agent sessions that can stop one task
// without ending the turn or the session. Task cards carry no stop button:
// the person asks the agent, which stops its own tasks.
type TaskStopper interface {
	StopTask(taskID string) error
}

// TaskCardState is what a task card shows.
type TaskCardState string

const (
	TaskCardRunning     TaskCardState = "running"
	TaskCardCompleted   TaskCardState = "completed"
	TaskCardFailed      TaskCardState = "failed"
	TaskCardStopped     TaskCardState = "stopped"
	TaskCardInterrupted TaskCardState = "interrupted"
)

// Live reports whether the task is still running on the card.
func (s TaskCardState) Live() bool { return s == TaskCardRunning }

// TaskCardView is the whole content of a task card; every text is ready to show.
type TaskCardView struct {
	State        TaskCardState
	Title        string // live: "background task · running: <task>"
	Status       string // live: current step; ended: the outcome line, marked as a background task and naming it
	Elapsed      string // live only, whole minutes
	Recent       string // live only: latest progress line
	Hint         string // live only: the task runs aside; the chat stays free
	Summary      string // ended only: up to three lines
	DetailsTitle string
	Details      []string // latest steps, oldest first
	SessionKey   string
	// Interrupted is what the card should say if a restart cuts it off.
	Interrupted string
}

// TaskCardPlatform is implemented by platforms that can show one updatable
// card per background task.
type TaskCardPlatform interface {
	SendTaskCard(ctx context.Context, replyCtx any, view TaskCardView) (any, error)
	UpdateTaskCard(ctx context.Context, handle any, view TaskCardView) error
}

const (
	taskCardDetailRows = 10
	taskCardMaxOpen    = 3 // per session
)

type taskCard struct {
	key, sessionKey, taskID string
	agent                   AgentSession
	platform                TaskCardPlatform
	replyCtx                any

	desc       string
	background bool
	hidden     bool
	start      time.Time
	step       string
	recent     string
	cliRecent  bool // recent came from the agent, not from the quick model
	details    []string
	steps      int // step changes seen, including those rotated out of details
	ended      bool
	state      TaskCardState
	summary    string

	scheduled bool // the open timer is set
	opened    bool // the card is sent (or being sent); run owns updates
	handle    any
	wake      chan struct{}

	summarizing    bool
	lastSummaryAt  time.Time
	summarizedRows int
}

// taskCardManager keeps one card per background task: opened once the task
// has run for openDelay, updated in place at most every minGap, and settled
// to the task's outcome when it ends.
type taskCardManager struct {
	mu    sync.Mutex
	cards map[string]*taskCard

	openDelay  time.Duration // a task ending sooner gets no card
	minGap     time.Duration // between two updates of one card
	tick       time.Duration // how often the elapsed minutes are checked
	summaryGap time.Duration // between two quick-model progress lines
}

func newTaskCardManager() *taskCardManager {
	return &taskCardManager{
		cards:      make(map[string]*taskCard),
		openDelay:  10 * time.Second,
		minGap:     3 * time.Second,
		tick:       20 * time.Second,
		summaryGap: 30 * time.Second,
	}
}

// handleTaskEvent routes an EventTask of the session behind state.
func (e *Engine) handleTaskEvent(state *interactiveState, sessionKey string, ev *TaskEvent) {
	if ev == nil || ev.TaskID == "" {
		return
	}
	state.mu.Lock()
	p, replyCtx, agent := state.platform, state.replyCtx, state.agentSession
	state.mu.Unlock()
	e.taskCards.handle(e, p, replyCtx, agent, sessionKey, ev)
}

func (m *taskCardManager) handle(e *Engine, p Platform, replyCtx any, agent AgentSession, sessionKey string, ev *TaskEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sessionKey + "\x00" + ev.TaskID
	c := m.cards[key]
	if c == nil {
		if ev.Kind == TaskEnded {
			return
		}
		tp, ok := p.(TaskCardPlatform)
		if !ok {
			return
		}
		c = &taskCard{key: key, sessionKey: sessionKey, taskID: ev.TaskID, agent: agent, platform: tp, replyCtx: replyCtx,
			start: time.Now().Add(-ev.Elapsed), state: TaskCardRunning, wake: make(chan struct{}, 1)}
		m.cards[key] = c
	}
	if ev.Description != "" {
		c.desc = ev.Description
	}
	if ev.Background != nil {
		c.background = *ev.Background
	}
	if ev.Hidden {
		c.hidden = true
	}
	if ev.Step != "" && ev.Step != c.step {
		c.step = ev.Step
		c.steps++
		c.details = append(c.details, ev.Step)
		if len(c.details) > taskCardDetailRows {
			c.details = c.details[len(c.details)-taskCardDetailRows:]
		}
	}
	switch ev.Kind {
	case TaskProgress:
		if ev.Summary != "" {
			c.recent, c.cliRecent = ev.Summary, true
		}
	case TaskEnded:
		c.ended = true
		c.summary = ev.Summary
		switch ev.Status {
		case "failed":
			c.state = TaskCardFailed
		case "stopped", "killed":
			c.state = TaskCardStopped
		default:
			c.state = TaskCardCompleted
		}
	}
	m.scheduleLocked(e, c)
}

// scheduleLocked moves a card along after any change. Caller holds m.mu.
func (m *taskCardManager) scheduleLocked(e *Engine, c *taskCard) {
	switch {
	case c.opened:
		poke(c.wake)
		m.maybeSummarizeLocked(e, c)
	case c.ended:
		m.dropLocked(c)
	case c.hidden:
		// Kept until it ends, so later frames without the flag do not open a card.
	case c.background && !c.scheduled:
		c.scheduled = true
		time.AfterFunc(max(0, time.Until(c.start.Add(m.openDelay))), func() { m.open(e, c) })
	}
}

func (m *taskCardManager) dropLocked(c *taskCard) {
	if m.cards[c.key] == c {
		delete(m.cards, c.key)
	}
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (m *taskCardManager) open(e *Engine, c *taskCard) {
	m.mu.Lock()
	if c.opened || c.ended || c.hidden || m.cards[c.key] != c || e.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	open := 0
	for _, other := range m.cards {
		if other.sessionKey == c.sessionKey && other.opened {
			open++
		}
	}
	if open >= taskCardMaxOpen {
		m.mu.Unlock()
		slog.Info("task card: too many open cards, not shown", "session", c.sessionKey, "task", c.taskID)
		return
	}
	c.opened = true
	view := m.viewLocked(e, c)
	m.mu.Unlock()

	handle, err := c.platform.SendTaskCard(e.ctx, c.replyCtx, view)
	m.mu.Lock()
	if err != nil {
		m.dropLocked(c)
		m.mu.Unlock()
		slog.Warn("task card: send failed", "task", c.taskID, "error", err)
		return
	}
	c.handle = handle
	m.maybeSummarizeLocked(e, c)
	m.mu.Unlock()
	cardEvent("task_card_open", c.sessionKey, taskCardID(c), "task", c.taskID, "reason", "background_over_open_delay")
	go m.run(e, c)
}

// run owns a card's updates after it opened: changes are merged so the card
// is written at most every minGap, and the elapsed minutes stay current.
func (m *taskCardManager) run(e *Engine, c *taskCard) {
	tick := time.NewTicker(m.tick)
	defer tick.Stop()
	shownMinute := taskMinutes(c.start)
	for {
		if !sleepCtx(e.ctx, m.minGap) {
			return
		}
		select {
		case <-c.wake:
		case <-tick.C:
			if taskMinutes(c.start) == shownMinute {
				continue
			}
		case <-e.ctx.Done():
			return
		}
		m.mu.Lock()
		view := m.viewLocked(e, c)
		final := c.ended
		m.mu.Unlock()
		shownMinute = taskMinutes(c.start)
		if err := c.platform.UpdateTaskCard(e.ctx, c.handle, view); err != nil {
			slog.Warn("task card: update failed", "task", c.taskID, "error", err)
		}
		if final {
			m.mu.Lock()
			m.dropLocked(c)
			m.mu.Unlock()
			cardEvent("task_card_end", c.sessionKey, taskCardID(c), "task", c.taskID, "reason", string(view.State))
			return
		}
		cardEvent("task_card_update", c.sessionKey, taskCardID(c), "task", c.taskID, "reason", string(view.State))
	}
}

func taskCardID(c *taskCard) string {
	if id, ok := c.platform.(PreviewIdentifier); ok && c.handle != nil {
		return id.PreviewID(c.handle)
	}
	return ""
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func taskMinutes(start time.Time) int { return int(time.Since(start) / time.Minute) }

// viewLocked renders the card. Caller holds m.mu.
func (m *taskCardManager) viewLocked(e *Engine, c *taskCard) TaskCardView {
	i := e.i18n
	title := c.desc
	if title == "" {
		title = i.T(MsgTaskUntitled)
	}
	v := TaskCardView{
		State:        TaskCardRunning,
		Title:        i.Tf(MsgTaskRunningTitle, title),
		DetailsTitle: i.T(MsgTaskDetails),
		Details:      append([]string(nil), c.details...),
		SessionKey:   c.sessionKey,
		Interrupted:  i.Tf(MsgTaskInterrupted, title),
	}
	if c.ended {
		v.State = c.state
		switch c.state {
		case TaskCardFailed:
			v.Status = i.Tf(MsgTaskFailed, title)
		case TaskCardStopped:
			v.Status = i.Tf(MsgTaskStopped, title)
		case TaskCardInterrupted:
			v.Status = i.Tf(MsgTaskInterrupted, title)
		default:
			v.Status = i.Tf(MsgTaskCompleted, title)
		}
		v.Summary = clipLines(c.summary, 3, 300)
		return v
	}
	minutes := taskMinutes(c.start)
	if minutes < 1 {
		v.Elapsed = i.T(MsgTaskElapsedShort)
	} else {
		v.Elapsed = i.Tf(MsgTaskElapsed, minutes)
	}
	v.Status = c.step
	if v.Status == "" {
		v.Status = i.T(MsgTaskWorking)
	}
	if c.recent != "" {
		v.Recent = i.Tf(MsgTaskRecent, c.recent)
	}
	v.Hint = i.T(MsgTaskHint)
	return v
}

// clipLines keeps the first n non-empty lines and at most limit runes.
func clipLines(s string, n, limit int) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
		if len(lines) == n {
			break
		}
	}
	out := strings.Join(lines, "\n")
	if r := []rune(out); len(r) > limit {
		out = string(r[:limit]) + "…"
	}
	return out
}

// abandonTasks settles the cards of an agent session that is closing or gone:
// its tasks go with it. Every close path (/stop, /new, /switch, idle reclaim,
// queue reset, a turn's process exit) calls it through closeAgentSession.
func (e *Engine) abandonTasks(agent AgentSession) {
	if agent == nil {
		return
	}
	m := e.taskCards
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.cards {
		if c.agent != agent || c.ended {
			continue
		}
		c.ended, c.state = true, TaskCardInterrupted
		m.scheduleLocked(e, c)
	}
}

const taskSummaryPrompt = "你在给用户看的后台任务卡片上写一行进展。只写一句中文，不超过 25 个字，说明这个任务现在在做什么。" +
	"只依据给出的步骤，不要编造结果、数量或进度百分比，不要加引号或前缀。"

// maybeSummarizeLocked asks the quick model for a progress line when the agent
// gives none: at most every summaryGap, and only after new steps. A failure
// leaves the line as it was. Caller holds m.mu.
func (m *taskCardManager) maybeSummarizeLocked(e *Engine, c *taskCard) {
	cfg := e.streamPreview.QuickLine
	if !cfg.Enabled || c.cliRecent || c.ended || c.summarizing || c.handle == nil ||
		c.steps == 0 || c.steps == c.summarizedRows ||
		time.Since(c.lastSummaryAt) < m.summaryGap {
		return
	}
	c.summarizing = true
	c.lastSummaryAt = time.Now()
	c.summarizedRows = c.steps
	prompt := "任务：" + clipRunes(c.desc, 200) + "\n最近的步骤：\n- " + strings.Join(c.details, "\n- ")
	go func() {
		ctx, cancel := context.WithTimeout(e.ctx, quickLineTimeout(cfg))
		defer cancel()
		line, err := fetchQuickText(ctx, cfg, taskSummaryPrompt, []map[string]string{{"role": "user", "content": prompt}})
		m.mu.Lock()
		defer m.mu.Unlock()
		c.summarizing = false
		if err != nil {
			slog.Info("task card: progress line skipped", "error", err)
			return
		}
		if !c.cliRecent && !c.ended {
			c.recent = line
			poke(c.wake)
		}
	}()
}
