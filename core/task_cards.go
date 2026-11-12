package core

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"sort"
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
	StepGeneric bool          // Step is only the tool's generic label ("运行命令"), not the agent's own description
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
	Title        string // one task: "Background task: <name>"; several: "Background tasks: <n>"
	Label        string // the state in a word: running, done, failed, stopped, interrupted
	Line         string // one task — live: "<current step> · <elapsed>"; ended: "<outcome> · took <elapsed>"
	Summary      string // one task, ended only: the outcome part of Line
	DetailsTitle string
	Details      []string // one task: latest steps, oldest first
	FullTitle    string   // one task, ended only: names Full inside the details panel
	Full         string   // one task, ended only: the agent's own text, as is, inside the details panel
	// Rows lists the tasks of a card holding two or more, in the order they
	// joined it; empty for a card of one task, which uses Line and Details.
	Rows []TaskCardRow
	More string // rows kept off the body: "N more under Details"; "" = every row is on the body
	// SessionKey names the chat session the card belongs to.
	SessionKey string
	// Interrupted is what the card should say if a restart cuts it off.
	Interrupted string
}

// TaskCardRow is one task on a card of several tasks.
type TaskCardRow struct {
	State     TaskCardState
	Text      string // "<mark> <name> · <step or outcome> · <time>"
	Shown     bool   // on the card's body; otherwise only in the details panel
	Details   []string
	FullTitle string // failed only: names Full
	Full      string // failed only: the raw error, cut short
}

// TaskCardPlatform is implemented by platforms that can show one updatable
// card of background tasks per session.
type TaskCardPlatform interface {
	SendTaskCard(ctx context.Context, replyCtx any, view TaskCardView) (any, error)
	UpdateTaskCard(ctx context.Context, handle any, view TaskCardView) error
}

const (
	taskCardDetailRows = 10
	taskCardRowsShown  = 8   // rows on the body of a card of several tasks
	taskRowSteps       = 5   // steps per task in the details of a card of several tasks
	taskRowNameRunes   = 24  // a task's name on its row
	taskRowTextRunes   = 30  // a row's step or outcome
	taskRowFullRunes   = 300 // a failed row's raw error in the details
	// taskOpenTogether: tasks due to show within this time of a card opening
	// join it at once, so a batch started together appears together.
	taskOpenTogether = time.Second
)

// cardTask is one background task the bridge follows.
type cardTask struct {
	key, sessionKey, taskID string
	seq                     uint64 // order of first sight; rows follow it
	agent                   AgentSession
	platform                TaskCardPlatform
	replyCtx                any

	desc       string
	background bool
	hidden     bool
	start      time.Time
	step       string
	generic    bool // step is a tool's generic label, not the agent's own description
	recent     string
	still      bool      // the card has been idle: the latest progress line stands in for the step
	cliAt      time.Time // when the agent last wrote recent; zero = never
	details    []string
	steps      int // step changes seen, including those rotated out of details
	ended      bool
	endedAt    time.Time
	state      TaskCardState
	summary    string
	brief      string // completed: the quick model's one line for a long or raw summary
	condensing bool   // the quick model is writing brief; the settled card waits for it

	scheduled bool      // the show timer is set
	card      *taskCard // the card showing the task; nil until it shows

	summarizing    bool
	lastSummaryAt  time.Time
	summarizedRows int
}

// taskCard is the one live card of a session's background tasks. Tasks join
// it while any of its tasks runs; once all have ended it settles, and the
// next task opens a new card.
type taskCard struct {
	sessionKey string
	platform   TaskCardPlatform
	replyCtx   any
	tasks      []*cardTask // in the order they joined
	handle     any
	wake       chan struct{}
}

func (c *taskCard) allEnded() bool {
	for _, t := range c.tasks {
		if !t.ended {
			return false
		}
	}
	return true
}

func (c *taskCard) condensing() bool {
	for _, t := range c.tasks {
		if t.condensing {
			return true
		}
	}
	return false
}

// taskCardManager follows background tasks and keeps one card per session
// for them: opened once a task has run for openDelay, updated in place at
// most every minGap, and settled when all of its tasks have ended.
type taskCardManager struct {
	mu       sync.Mutex
	tasks    map[string]*cardTask // by session and task ID
	open     map[string]*taskCard // the live card of each session
	seq      uint64
	inflight *inflightStore // background tasks in progress, for resume after restart

	openDelay  time.Duration // a task ending sooner shows on no card
	minGap     time.Duration // between two updates of one card
	tick       time.Duration // how often a card is checked for changes
	idleBeat   time.Duration // a card unchanged this long shows a sign that its tasks are alive
	summaryGap time.Duration // between two quick-model progress lines of one task
	cliStale   time.Duration // an agent progress line older than this lets the quick model write
}

func newTaskCardManager() *taskCardManager {
	return &taskCardManager{
		tasks:      make(map[string]*cardTask),
		open:       make(map[string]*taskCard),
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
	t := m.tasks[key]
	if t == nil {
		if ev.Kind == TaskEnded {
			return
		}
		tp, ok := p.(TaskCardPlatform)
		if !ok {
			return
		}
		m.seq++
		t = &cardTask{key: key, sessionKey: sessionKey, taskID: ev.TaskID, seq: m.seq, agent: agent, platform: tp, replyCtx: replyCtx,
			start: time.Now().Add(-ev.Elapsed), state: TaskCardRunning}
		m.tasks[key] = t
	}
	if ev.Description != "" {
		t.desc = ev.Description
	}
	if ev.Background != nil {
		t.background = *ev.Background
	}
	if ev.Hidden {
		t.hidden = true
	}
	if ev.Step != "" && ev.Step != t.step {
		t.step, t.still, t.generic = ev.Step, false, ev.StepGeneric
		t.steps++
		t.details = append(t.details, ev.Step)
		if len(t.details) > taskCardDetailRows {
			t.details = t.details[len(t.details)-taskCardDetailRows:]
		}
	}
	switch ev.Kind {
	case TaskProgress:
		if ev.Summary != "" {
			t.recent, t.cliAt = ev.Summary, time.Now()
		}
	case TaskEnded:
		if t.ended {
			break
		}
		t.ended, t.endedAt = true, time.Now()
		t.summary = ev.Summary
		switch ev.Status {
		case "failed":
			t.state = TaskCardFailed
		case "stopped", "killed":
			t.state = TaskCardStopped
		default:
			t.state = TaskCardCompleted
			if t.card != nil {
				m.condenseLocked(e, t)
			}
		}
	}
	m.scheduleLocked(e, t)
}

// scheduleLocked moves a task along after any change. Caller holds m.mu.
func (m *taskCardManager) scheduleLocked(e *Engine, t *cardTask) {
	switch {
	case t.card != nil:
		if t.ended {
			m.inflight.taskEnd(t.sessionKey, t.taskID)
		}
		poke(t.card.wake)
		m.maybeSummarizeLocked(e, t, false)
	case t.ended:
		m.dropLocked(t)
	case t.hidden:
		// Kept until it ends, so later frames without the flag do not show it.
	case t.background && !t.scheduled:
		t.scheduled = true
		time.AfterFunc(max(0, time.Until(t.start.Add(m.openDelay))), func() { m.show(e, t) })
	}
}

func (m *taskCardManager) dropLocked(t *cardTask) {
	if m.tasks[t.key] == t {
		delete(m.tasks, t.key)
		m.inflight.taskEnd(t.sessionKey, t.taskID)
	}
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// show puts a task that has run for openDelay on its session's live card,
// or opens a card for it with every task of the session due within
// taskOpenTogether.
func (m *taskCardManager) show(e *Engine, t *cardTask) {
	m.mu.Lock()
	if t.card != nil || t.ended || t.hidden || m.tasks[t.key] != t || e.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	if c := m.open[t.sessionKey]; c != nil {
		m.joinLocked(c, t)
		poke(c.wake)
		m.mu.Unlock()
		cardEvent("task_card_join", t.sessionKey, taskCardID(c), "task", t.taskID, "tasks", len(c.tasks))
		return
	}
	c := &taskCard{sessionKey: t.sessionKey, platform: t.platform, replyCtx: t.replyCtx, wake: make(chan struct{}, 1)}
	m.open[t.sessionKey] = c
	batch := []*cardTask{t}
	for _, o := range m.tasks {
		if o != t && o.sessionKey == t.sessionKey && o.card == nil && o.scheduled && !o.ended && !o.hidden && o.background &&
			time.Until(o.start.Add(m.openDelay)) <= taskOpenTogether {
			batch = append(batch, o)
		}
	}
	sort.Slice(batch, func(a, b int) bool { return batch[a].seq < batch[b].seq })
	for _, o := range batch {
		m.joinLocked(c, o)
	}
	view := m.viewLocked(e, c)
	m.mu.Unlock()

	handle, err := c.platform.SendTaskCard(e.ctx, c.replyCtx, view)
	m.mu.Lock()
	if err != nil {
		if m.open[c.sessionKey] == c {
			delete(m.open, c.sessionKey)
		}
		for _, o := range c.tasks {
			m.dropLocked(o)
		}
		m.mu.Unlock()
		slog.Warn("task card: send failed", "task", t.taskID, "error", err)
		return
	}
	c.handle = handle
	for _, o := range c.tasks {
		m.maybeSummarizeLocked(e, o, false)
	}
	n := len(c.tasks)
	m.mu.Unlock()
	cardEvent("task_card_open", c.sessionKey, taskCardID(c), "task", t.taskID, "tasks", n, "reason", "background_over_open_delay")
	go m.run(e, c, view)
}

// joinLocked puts t on c and records it for resume after a restart. Caller holds m.mu.
func (m *taskCardManager) joinLocked(c *taskCard, t *cardTask) {
	t.card = c
	c.tasks = append(c.tasks, t)
	m.inflight.taskOpen(t.sessionKey, t.taskID, t.desc)
}

// run owns a card's updates after it opened: changes are merged so the card
// is written at most every minGap, and the elapsed minutes stay current. A
// card that has not changed for idleBeat (a subagent writing, not calling
// tools) gets a sign of life: the quick model's next progress line when it
// can write one, and each running task's line flips between its step and its
// latest progress. Once all of its tasks have ended the card settles and
// leaves the session, so the next task opens a new card.
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
		if !c.allEnded() && sameTaskView(view, shown) && time.Since(changed) >= m.idleBeat {
			for _, t := range c.tasks {
				if !t.ended {
					t.still = !t.still
					m.maybeSummarizeLocked(e, t, true)
				}
			}
			view = m.viewLocked(e, c)
		}
		final, waiting := c.allEnded(), c.condensing()
		if final && !waiting {
			if m.open[c.sessionKey] == c {
				delete(m.open, c.sessionKey)
			}
			for _, t := range c.tasks {
				m.dropLocked(t)
			}
		}
		m.mu.Unlock()
		if waiting || !final && sameTaskView(view, shown) {
			continue
		}
		if err := c.platform.UpdateTaskCard(e.ctx, c.handle, view); err != nil {
			slog.Warn("task card: update failed", "card", taskCardID(c), "error", err)
		}
		shown, changed = view, time.Now()
		if final {
			cardEvent("task_card_end", c.sessionKey, taskCardID(c), "tasks", len(view.Rows), "reason", string(view.State))
			return
		}
		cardEvent("task_card_update", c.sessionKey, taskCardID(c), "reason", string(view.State))
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
	return a.State == b.State && a.Title == b.Title && a.Label == b.Label && a.Line == b.Line && a.More == b.More &&
		slices.Equal(a.Details, b.Details) && slices.EqualFunc(a.Rows, b.Rows, func(x, y TaskCardRow) bool {
		return x.State == y.State && x.Text == y.Text && x.Shown == y.Shown && x.Full == y.Full && slices.Equal(x.Details, y.Details)
	})
}

func taskMinutes(start time.Time) int { return int(time.Since(start) / time.Minute) }

// taskShown is how one task reads on a card: its name, state, and a line
// made of a step or outcome and a time.
type taskShown struct {
	name, label     string
	state           TaskCardState
	text, time      string // the line's two parts: step or outcome, elapsed or took
	fullTitle, full string // ended: the agent's own text when the line shows something shorter
}

// taskShownLocked reads t for its card. Caller holds m.mu.
func taskShownLocked(i *I18n, t *cardTask) taskShown {
	s := taskShown{name: t.desc, state: TaskCardRunning, label: i.T(MsgTaskWorking)}
	if s.name == "" {
		s.name = i.T(MsgTaskUntitled)
	}
	if !t.ended {
		s.text, s.time = liveStep(t), taskElapsed(i, taskMinutes(t.start))
		return s
	}
	s.state = t.state
	s.text = clipLines(t.summary, 1, 300)
	switch t.state {
	case TaskCardFailed:
		s.label = i.T(MsgTaskLabelFailed)
		s.text = failureLine(i, t.summary)
	case TaskCardStopped:
		s.label = i.T(MsgTaskLabelStopped)
		if !readableLine(t.summary) {
			s.text = ""
		}
	case TaskCardInterrupted:
		s.label = i.T(MsgTaskLabelCutOff)
	default:
		s.label = i.T(MsgTaskLabelDone)
		if !briefEnough(t.summary) {
			s.text = t.brief
			if s.text == "" {
				s.text = firstSentence(t.summary, taskBriefRunes)
			}
		}
	}
	if t.summary != "" && s.text != t.summary {
		s.fullTitle, s.full = i.T(MsgTaskFullText), clipRunesKeepLines(t.summary, taskCardFullRunes)
	}
	s.time = i.T(MsgTaskTookShort)
	if n := int(t.endedAt.Sub(t.start) / time.Minute); n >= 1 {
		s.time = i.Tf(MsgTaskTook, n)
	}
	return s
}

// viewLocked renders the card. Caller holds m.mu.
func (m *taskCardManager) viewLocked(e *Engine, c *taskCard) TaskCardView {
	if len(c.tasks) == 1 {
		return oneTaskView(e.i18n, c.sessionKey, c.tasks[0])
	}
	return manyTasksView(e.i18n, c.sessionKey, c.tasks)
}

// oneTaskView is the card of a single task: the task's name as the title, its
// state as a label, and one line under it.
func oneTaskView(i *I18n, sessionKey string, t *cardTask) TaskCardView {
	s := taskShownLocked(i, t)
	v := TaskCardView{
		State:        s.state,
		Title:        i.Tf(MsgTaskTitle, s.name),
		Label:        s.label,
		Line:         joinTaskLine(s.text, s.time),
		DetailsTitle: i.T(MsgTaskDetails),
		Details:      append([]string(nil), t.details...),
		SessionKey:   sessionKey,
		Interrupted:  i.Tf(MsgTaskInterrupted, s.name),
	}
	if t.ended {
		v.Summary, v.FullTitle, v.Full = s.text, s.fullTitle, s.full
	}
	return v
}

// taskMark is the sign in front of a task's row.
func taskMark(s TaskCardState) string {
	switch s {
	case TaskCardCompleted:
		return "✓"
	case TaskCardFailed:
		return "✗"
	case TaskCardStopped:
		return "■"
	case TaskCardInterrupted:
		return "⊘"
	}
	return "◐"
}

// taskRow is a task's row: "<mark> <name> · <step or outcome> · <time>".
func taskRow(s taskShown) string {
	return taskMark(s.state) + " " + clipRunes(s.name, taskRowNameRunes) + " · " + joinTaskLine(clipRunes(s.text, taskRowTextRunes), s.time)
}

// manyTasksView is the card of several tasks: their count as the title, one
// row per task, and each task's steps in the folded details. While tasks run
// the label counts those ended; once all have ended the header says how
// they went: done when all completed, failed when any failed, else
// interrupted or stopped.
func manyTasksView(i *I18n, sessionKey string, tasks []*cardTask) TaskCardView {
	v := TaskCardView{
		Title:        i.Tf(MsgTaskTitleCount, len(tasks)),
		DetailsTitle: i.T(MsgTaskDetails),
		SessionKey:   sessionKey,
		Rows:         make([]TaskCardRow, len(tasks)),
	}
	count := map[TaskCardState]int{}
	cut := []string{i.Tf(MsgTaskInterrupted, v.Title)}
	for k, t := range tasks {
		s := taskShownLocked(i, t)
		count[s.state]++
		row := TaskCardRow{State: s.state, Text: taskRow(s)}
		steps := t.details
		if len(steps) > taskRowSteps {
			steps = steps[len(steps)-taskRowSteps:]
		}
		for _, step := range steps {
			row.Details = append(row.Details, clipRunes(step, 2*taskRowTextRunes))
		}
		if s.state == TaskCardFailed && s.full != "" {
			row.FullTitle, row.Full = s.fullTitle, clipRunesKeepLines(t.summary, taskRowFullRunes)
		}
		v.Rows[k] = row
		if s.state.Live() {
			cut = append(cut, taskMark(TaskCardInterrupted)+" "+clipRunes(s.name, taskRowNameRunes))
		} else {
			cut = append(cut, row.Text)
		}
	}
	v.Interrupted = strings.Join(cut, "\n")
	ended := len(tasks) - count[TaskCardRunning]
	switch {
	case count[TaskCardRunning] > 0:
		v.State, v.Label = TaskCardRunning, i.T(MsgTaskWorking)
		if ended > 0 {
			v.Label = i.Tf(MsgTaskWorkingCount, ended, len(tasks))
		}
	case count[TaskCardFailed] > 0:
		v.State, v.Label = TaskCardFailed, i.Tf(MsgTaskLabelFailedCount, count[TaskCardFailed])
	case count[TaskCardCompleted] == len(tasks):
		v.State, v.Label = TaskCardCompleted, i.T(MsgTaskLabelDone)
	case count[TaskCardInterrupted] > 0:
		v.State, v.Label = TaskCardInterrupted, i.T(MsgTaskLabelCutOff)
	default:
		v.State, v.Label = TaskCardStopped, i.T(MsgTaskLabelStopped)
	}
	showRows(&v)
	if hidden := len(v.Rows) - taskCardRowsShown; hidden > 0 {
		v.More = i.Tf(MsgTaskMoreRows, hidden)
	}
	return v
}

// showRows picks the rows for the card's body, at most taskCardRowsShown:
// running and failed tasks first, then the rest, each in joining order.
func showRows(v *TaskCardView) {
	left := taskCardRowsShown
	for _, first := range []bool{true, false} {
		for k := range v.Rows {
			r := &v.Rows[k]
			if left == 0 {
				return
			}
			if !r.Shown && (r.State.Live() || r.State == TaskCardFailed) == first {
				r.Shown = true
				left--
			}
		}
	}
}

// liveStep is what a running task is doing: the current step, or the latest
// progress line when there is no step, the step is only a tool's generic
// label, or the card has been idle. A generic label alone says nothing the
// card's state label does not, so then the line is the time alone.
func liveStep(t *cardTask) string {
	switch {
	case t.recent != "" && (t.step == "" || t.generic || t.still):
		return t.recent
	case t.step != "" && !t.generic:
		return t.step
	}
	return ""
}

// runningRows are the card rows of the session's running background tasks,
// in the order they started, for the progress answer.
func (m *taskCardManager) runningRows(i *I18n, sessionKey string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var tasks []*cardTask
	for _, t := range m.tasks {
		if t.sessionKey == sessionKey && t.background && !t.hidden && !t.ended {
			tasks = append(tasks, t)
		}
	}
	sort.Slice(tasks, func(a, b int) bool { return tasks[a].seq < tasks[b].seq })
	rows := make([]string, len(tasks))
	for k, t := range tasks {
		rows[k] = taskRow(taskShownLocked(i, t))
	}
	return rows
}

// taskElapsed is a running time in whole minutes: "3 min", "under a minute".
func taskElapsed(i *I18n, minutes int) string {
	if minutes < 1 {
		return i.T(MsgTaskElapsedShort)
	}
	return i.Tf(MsgTaskElapsed, minutes)
}

// joinTaskLine is "<text> · <time>", or the time alone when text is empty.
func joinTaskLine(text, time string) string {
	if text = strings.TrimRight(strings.TrimSpace(text), "。.；;，, "); text == "" {
		return time
	}
	return text + " · " + time
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
func (m *taskCardManager) condenseLocked(e *Engine, c *cardTask) {
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
		poke(c.card.wake)
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
	for _, c := range m.tasks {
		if c.agent != agent || c.ended {
			continue
		}
		c.ended, c.endedAt, c.state = true, time.Now(), end
		m.scheduleLocked(e, c)
	}
}

const taskSummaryPrompt = "你在给用户看的后台任务卡片上写一行进展。只写一句中文，不超过 25 个字，说明这个任务现在在做什么。" +
	"只依据给出的步骤，不要编造结果、数量或进度百分比，不要加引号或前缀。这句话只写一遍。"

// maybeSummarizeLocked asks the quick model for a progress line when the
// agent's own line is missing or has not changed for cliStale: at most every
// summaryGap, and only after new steps unless the card has been idle. A
// failure leaves the line as it was. Caller holds m.mu.
func (m *taskCardManager) maybeSummarizeLocked(e *Engine, c *cardTask, idle bool) {
	cfg := e.streamPreview.QuickLine
	if !cfg.Enabled || c.ended || c.summarizing || c.card == nil || c.card.handle == nil ||
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
			poke(c.card.wake)
		}
	}()
}
