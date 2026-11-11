package core

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
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
	FullTitle    string   // ended only: names Full inside the details panel
	Full         string   // ended only: the agent's own text, as is, inside the details panel
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
	still      bool      // the step line says the task is still on the same step
	cliAt      time.Time // when the agent last wrote recent; zero = never
	details    []string
	steps      int // step changes seen, including those rotated out of details
	ended      bool
	state      TaskCardState
	summary    string
	brief      string // completed: the quick model's one line for a long or raw summary
	condensing bool   // the quick model is writing brief; the settled card waits for it

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
	tick       time.Duration // how often a card is checked for changes
	idleBeat   time.Duration // a card unchanged this long shows a sign that the task is alive
	summaryGap time.Duration // between two quick-model progress lines
	cliStale   time.Duration // an agent progress line older than this lets the quick model write
}

func newTaskCardManager() *taskCardManager {
	return &taskCardManager{
		cards:      make(map[string]*taskCard),
		openDelay:  10 * time.Second,
		minGap:     3 * time.Second,
		tick:       5 * time.Second,
		idleBeat:   25 * time.Second,
		summaryGap: 30 * time.Second,
		cliStale:   60 * time.Second,
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
		c.step, c.still = ev.Step, false
		c.steps++
		c.details = append(c.details, ev.Step)
		if len(c.details) > taskCardDetailRows {
			c.details = c.details[len(c.details)-taskCardDetailRows:]
		}
	}
	switch ev.Kind {
	case TaskProgress:
		if ev.Summary != "" {
			c.recent, c.cliAt = ev.Summary, time.Now()
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
			if c.opened {
				m.condenseLocked(e, c)
			}
		}
	}
	m.scheduleLocked(e, c)
}

// scheduleLocked moves a card along after any change. Caller holds m.mu.
func (m *taskCardManager) scheduleLocked(e *Engine, c *taskCard) {
	switch {
	case c.opened:
		poke(c.wake)
		m.maybeSummarizeLocked(e, c, false)
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
	m.maybeSummarizeLocked(e, c, false)
	m.mu.Unlock()
	cardEvent("task_card_open", c.sessionKey, taskCardID(c), "task", c.taskID, "reason", "background_over_open_delay")
	go m.run(e, c, view)
}

// run owns a card's updates after it opened: changes are merged so the card
// is written at most every minGap, and the elapsed minutes stay current. A
// card that has not changed for idleBeat (a subagent writing, not calling
// tools) gets a sign of life: the quick model's next progress line when it
// can write one, and the step line flips to "still on: <step>" and back.
func (m *taskCardManager) run(e *Engine, c *taskCard, shown TaskCardView) {
	tick := time.NewTicker(m.tick)
	defer tick.Stop()
	changed := time.Now()
	for {
		if !sleepCtx(e.ctx, m.minGap) {
			return
		}
		select {
		case <-c.wake:
		case <-tick.C:
		case <-e.ctx.Done():
			return
		}
		m.mu.Lock()
		view := m.viewLocked(e, c)
		if !c.ended && sameTaskView(view, shown) && time.Since(changed) >= m.idleBeat {
			c.still = !c.still
			m.maybeSummarizeLocked(e, c, true)
			view = m.viewLocked(e, c)
		}
		final, waiting := c.ended, c.condensing
		m.mu.Unlock()
		if waiting || !final && sameTaskView(view, shown) {
			continue
		}
		if err := c.platform.UpdateTaskCard(e.ctx, c.handle, view); err != nil {
			slog.Warn("task card: update failed", "task", c.taskID, "error", err)
		}
		shown, changed = view, time.Now()
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

func sameTaskView(a, b TaskCardView) bool {
	return a.State == b.State && a.Title == b.Title && a.Status == b.Status && a.Elapsed == b.Elapsed &&
		a.Recent == b.Recent && a.Summary == b.Summary && slices.Equal(a.Details, b.Details)
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
		v.Summary = clipLines(c.summary, 3, 300)
		switch c.state {
		case TaskCardFailed:
			v.Status = i.Tf(MsgTaskFailed, title)
			v.Summary = failureLine(i, c.summary)
		case TaskCardStopped:
			v.Status = i.Tf(MsgTaskStopped, title)
			if !readableLine(c.summary) {
				v.Summary = ""
			}
		case TaskCardInterrupted:
			v.Status = i.Tf(MsgTaskInterrupted, title)
		default:
			v.Status = i.Tf(MsgTaskCompleted, title)
			if !briefEnough(c.summary) {
				v.Summary = c.brief
				if v.Summary == "" {
					v.Summary = firstSentence(c.summary, taskBriefRunes)
				}
			}
		}
		if c.summary != "" && v.Summary != c.summary {
			v.FullTitle, v.Full = i.T(MsgTaskFullText), clipRunesKeepLines(c.summary, taskCardFullRunes)
		}
		return v
	}
	minutes := taskMinutes(c.start)
	if minutes < 1 {
		v.Elapsed = i.T(MsgTaskElapsedShort)
	} else {
		v.Elapsed = i.Tf(MsgTaskElapsed, minutes)
	}
	v.Status = c.step
	if c.still {
		if v.Status == "" {
			v.Status = title
		}
		v.Status = i.Tf(MsgTaskStill, v.Status)
	} else if v.Status == "" {
		v.Status = i.T(MsgTaskWorking)
	}
	if c.recent != "" {
		v.Recent = i.Tf(MsgTaskRecent, c.recent)
	}
	v.Hint = i.T(MsgTaskHint)
	return v
}

const taskCardFullRunes = 1500

var (
	exitCodeRe = regexp.MustCompile(`exit code (\d+)`)
	pathRe     = regexp.MustCompile(`(?:^|[^\p{Han}\d])/[A-Za-z0-9_.~-]`)
)

// failureLine is the card's one Chinese line for a failed task. The agent's
// own text shows only when it already reads as such a line; raw errors (the
// CLI's "Background command ... failed with exit code 1") go to the details.
func failureLine(i *I18n, summary string) string {
	if readableLine(summary) {
		return summary
	}
	if m := exitCodeRe.FindStringSubmatch(summary); m != nil {
		return i.Tf(MsgTaskFailedExit, m[1])
	}
	return i.T(MsgTaskFailedGeneric)
}

// readableLine reports whether s can sit on the card as written: one short
// Chinese line, with no paths or code in it. A line that is mostly English
// ("Background command "清理" failed with exit code 1") is not.
func readableLine(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "\n`{}\\") || utf8.RuneCountInString(s) > 80 || pathRe.MatchString(s) {
		return false
	}
	han, latin := 0, 0
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			han++
		case r < 128 && unicode.IsLetter(r):
			latin++
		}
	}
	return han > 0 && latin <= 3*han
}

const taskBriefRunes = 40

var sentenceEnd = regexp.MustCompile(`[。！？!?]|\.(\s|$)`)

// briefEnough reports whether a completed task's summary can sit on the card
// as written: one readable line of at most two sentences.
func briefEnough(s string) bool {
	return readableLine(s) && len(sentenceEnd.FindAllStringIndex(s, -1)) <= 2
}

// firstSentence is the first sentence of s's first line that is not a
// Markdown heading, without list marks, cut to limit runes. A text of
// headings only gives its first heading.
func firstSentence(s string, limit int) string {
	heading := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		isHeading := strings.HasPrefix(line, "#")
		if line = strings.TrimSpace(strings.TrimLeft(line, "#*->` ")); line == "" {
			continue
		}
		if isHeading {
			if heading == "" {
				heading = line
			}
			continue
		}
		if loc := sentenceEnd.FindStringIndex(line); loc != nil {
			line = strings.TrimSpace(line[:loc[1]])
		}
		return clipRunesKeepLines(line, limit)
	}
	return clipRunesKeepLines(heading, limit)
}

const taskResultPrompt = "你在给用户看的后台任务卡片上写这个任务的结果。只写一句中文，不超过 25 个字，概括任务做成了什么。" +
	"只依据给出的结果，不要编造；不要写路径、命令、代码或文件名，不要加引号或前缀。这句话只写一遍。"

// condenseLocked asks the quick model for one line when a completed task's
// summary is long or raw output (several lines, paths, code). The settled card
// waits for it, at most the quick line's timeout; on failure it shows the
// summary's first sentence. Caller holds m.mu.
func (m *taskCardManager) condenseLocked(e *Engine, c *taskCard) {
	cfg := e.streamPreview.QuickLine
	if !cfg.Enabled || c.summary == "" || briefEnough(c.summary) {
		return
	}
	c.condensing = true
	prompt := "任务：" + clipRunes(c.desc, 200) + "\n结果：\n" + clipRunesKeepLines(c.summary, 3000)
	go func() {
		ctx, cancel := context.WithTimeout(e.ctx, quickLineTimeout(cfg))
		defer cancel()
		line, err := fetchQuickText(ctx, cfg, taskResultPrompt, []map[string]string{{"role": "user", "content": prompt}})
		m.mu.Lock()
		defer m.mu.Unlock()
		c.condensing = false
		if err != nil {
			slog.Info("task card: result line skipped", "error", err)
		} else if readableLine(line) {
			c.brief = line
		}
		poke(c.wake)
	}()
}

// clipRunesKeepLines keeps s as written, cut to at most limit runes.
func clipRunesKeepLines(s string, limit int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
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
	e.settleTasks(agent, TaskCardInterrupted)
}

// stopTasksOf settles the cards of the agent session behind interactiveKey as
// stopped: /stop ends the agent process, and its background tasks with it,
// because the person asked for it.
func (e *Engine) stopTasksOf(interactiveKey string) {
	e.interactiveMu.Lock()
	state := e.interactiveStates[interactiveKey]
	e.interactiveMu.Unlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	agent := state.agentSession
	state.mu.Unlock()
	e.settleTasks(agent, TaskCardStopped)
}

func (e *Engine) settleTasks(agent AgentSession, end TaskCardState) {
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
		c.ended, c.state = true, end
		m.scheduleLocked(e, c)
	}
}

const taskSummaryPrompt = "你在给用户看的后台任务卡片上写一行进展。只写一句中文，不超过 25 个字，说明这个任务现在在做什么。" +
	"只依据给出的步骤，不要编造结果、数量或进度百分比，不要加引号或前缀。这句话只写一遍。"

// maybeSummarizeLocked asks the quick model for a progress line when the
// agent's own line is missing or has not changed for cliStale: at most every
// summaryGap, and only after new steps unless the card has been idle. A
// failure leaves the line as it was. Caller holds m.mu.
func (m *taskCardManager) maybeSummarizeLocked(e *Engine, c *taskCard, idle bool) {
	cfg := e.streamPreview.QuickLine
	if !cfg.Enabled || c.ended || c.summarizing || c.handle == nil ||
		time.Since(c.cliAt) < m.cliStale ||
		c.steps == 0 || (!idle && c.steps == c.summarizedRows) ||
		time.Since(c.lastSummaryAt) < m.summaryGap {
		return
	}
	c.summarizing = true
	asked := time.Now()
	c.lastSummaryAt = asked
	c.summarizedRows = c.steps
	prompt := "任务：" + clipRunes(c.desc, 200) + "\n最近的步骤：\n- " + strings.Join(c.details, "\n- ")
	if idle {
		prompt += "\n（最近一段时间没有新步骤，任务还在做最后这一步。）"
	}
	if c.recent != "" {
		prompt += "\n上一句进展：" + c.recent + "\n不要重复上一句。"
	}
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
		// The agent's own line wins if it arrived while the quick model wrote.
		if !c.ended && !c.cliAt.After(asked) && line != c.recent {
			c.recent = line
			poke(c.wake)
		}
	}()
}
