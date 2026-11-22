package modelrouter

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// session wraps one shell's session. It relays the shell's events to the
// engine, records the conversation into the thread (user messages, final
// replies, one line per tool call), and puts the handoff in front of the
// first user message.
type session struct {
	r          *Agent
	th         *thread
	shell      string
	inner      core.AgentSession
	compactCmd string // the shell's compact command; passes through untouched

	out    chan core.Event // owned by relay, which closes it
	inject chan core.Event // events of the router itself (notices)
	done   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	pending *handoff
	compact bool     // a compact command is running: its turn is not recorded
	texts   []string // text of the running turn, for a result without content
	lastID  string
	noticed bool // the handoff's notice went out
}

func (r *Agent) wrap(inner core.AgentSession, th *thread, shell string, h *handoff) core.AgentSession {
	s := &session{
		r:       r,
		th:      th,
		shell:   shell,
		inner:   inner,
		out:     make(chan core.Event, max(cap(inner.Events()), 1)),
		inject:  make(chan core.Event, 4),
		done:    make(chan struct{}),
		pending: h,
	}
	if c, ok := r.shells[shell].(core.ContextCompressor); ok {
		s.compactCmd = strings.TrimSpace(c.CompressCommand())
	}
	s.noteID(inner.CurrentSessionID())
	go s.relay(inner.Events())

	// The engine sees the capabilities of the shell's own session.
	if _, ok := inner.(core.AgentSessionSteerer); ok {
		return &claudeShellSession{s}
	}
	if _, ok := inner.(core.AgentSessionCanceller); ok {
		return &acpShellSession{s}
	}
	return s
}

func (s *session) relay(in <-chan core.Event) {
	defer close(s.out)
	for {
		// The router's own events go first: a notice queued before a message
		// is sent precedes everything the shell answers to it.
		select {
		case ev := <-s.inject:
			s.forward(ev)
			continue
		default:
		}
		select {
		case ev, ok := <-in:
			if !ok {
				return
			}
			s.observe(ev)
			s.forward(ev)
		case ev := <-s.inject:
			s.forward(ev)
		}
	}
}

// forward hands ev to the engine; after Close nobody reads, so it is dropped.
func (s *session) forward(ev core.Event) {
	select {
	case s.out <- ev:
	case <-s.done:
	}
}

func (s *session) observe(ev core.Event) {
	if ev.SessionID != "" {
		s.noteID(ev.SessionID)
	}
	s.mu.Lock()
	if s.compact {
		if ev.Type == core.EventResult || ev.Type == core.EventError {
			s.compact = false
		}
		s.mu.Unlock()
		return
	}
	var rec *entry
	switch ev.Type {
	case core.EventText:
		if ev.Content != "" {
			s.texts = append(s.texts, ev.Content)
		}
	case core.EventToolUse:
		if line := toolLine(ev.ToolName, ev.ToolInput); line != "" {
			rec = &entry{Role: roleTool, Content: line}
		}
	case core.EventResult:
		text := ev.Content
		if strings.TrimSpace(text) == "" {
			text = strings.Join(s.texts, "")
		}
		s.texts = nil
		if strings.TrimSpace(text) != "" {
			rec = &entry{Role: roleAssistant, Content: text}
		}
	}
	s.mu.Unlock()
	if rec != nil {
		rec.Time = time.Now()
		rec.Shell = s.shell
		s.r.store.record(s.th, *rec)
	}
}

func (s *session) noteID(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	same := id == s.lastID
	s.lastID = id
	s.mu.Unlock()
	if !same {
		s.r.store.setID(s.th, s.shell, id)
	}
}

func (s *session) Send(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if s.compactCmd != "" && strings.TrimSpace(prompt) == s.compactCmd {
		s.mu.Lock()
		s.compact = true
		s.mu.Unlock()
		return s.inner.Send(prompt, messageID, images, files)
	}
	s.mu.Lock()
	h := s.pending
	notice := h != nil && h.notice != nil && !s.noticed
	if notice {
		s.noticed = true
	}
	s.mu.Unlock()
	text := prompt
	if h != nil {
		text = withHandoff(h.text, prompt)
	}
	if notice {
		select {
		case s.inject <- core.Event{Type: core.EventNotice, Notice: h.notice}:
		default:
		}
	}
	at := time.Now()
	if err := s.inner.Send(text, messageID, images, files); err != nil {
		return err
	}
	s.recordUser(at, prompt, len(images)+len(files))
	if h != nil {
		s.mu.Lock()
		first := s.pending == h
		if first {
			s.pending = nil
		}
		s.mu.Unlock()
		if first && h.from != "" {
			s.r.store.moved(s.th, h.from, s.shell, at)
		}
	}
	return nil
}

func (s *session) recordUser(at time.Time, prompt string, attachments int) {
	text := strings.TrimSpace(prompt)
	if attachments > 0 {
		text = strings.TrimSpace(fmt.Sprintf("%s [%d attachment(s)]", text, attachments))
	}
	s.r.store.record(s.th, entry{Time: at, Shell: s.shell, Role: roleUser, Content: text})
}

func (s *session) RespondPermission(requestID string, result core.PermissionResult) error {
	return s.inner.RespondPermission(requestID, result)
}

func (s *session) Events() <-chan core.Event { return s.out }

func (s *session) CurrentSessionID() string {
	id := s.inner.CurrentSessionID()
	s.noteID(id)
	return id
}

func (s *session) Alive() bool { return s.inner.Alive() }

func (s *session) Close() error {
	s.once.Do(func() { close(s.done) })
	return s.inner.Close()
}

// claudeShellSession carries the capabilities of a Claude Code session.
type claudeShellSession struct{ *session }

func (c *claudeShellSession) Steer(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) (bool, error) {
	st, ok := c.inner.(core.AgentSessionSteerer)
	if !ok {
		return false, nil
	}
	at := time.Now()
	accepted, err := st.Steer(prompt, messageID, images, files)
	if accepted && err == nil {
		c.recordUser(at, prompt, len(images)+len(files))
	}
	return accepted, err
}

func (c *claudeShellSession) SteerAsync(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) (*core.SteerReceipt, error) {
	st, ok := c.inner.(core.AgentSessionAsyncSteerer)
	if !ok {
		return nil, fmt.Errorf("model_router: steering not supported")
	}
	at := time.Now()
	receipt, err := st.SteerAsync(prompt, messageID, images, files)
	if receipt != nil && err == nil {
		c.recordUser(at, prompt, len(images)+len(files))
	}
	return receipt, err
}

func (c *claudeShellSession) HasBackgroundWork() bool {
	r, ok := c.inner.(core.BackgroundWorkReporter)
	return ok && r.HasBackgroundWork()
}

func (c *claudeShellSession) RunningTasks() int {
	if r, ok := c.inner.(core.RunningTasksReporter); ok {
		return r.RunningTasks()
	}
	return 0
}

func (c *claudeShellSession) StopTask(taskID string) error {
	if t, ok := c.inner.(core.TaskStopper); ok {
		return t.StopTask(taskID)
	}
	return fmt.Errorf("model_router: stopping tasks not supported")
}

func (c *claudeShellSession) InputFailure() *core.InputWriteError {
	if r, ok := c.inner.(core.InputFailureReporter); ok {
		return r.InputFailure()
	}
	return nil
}

func (c *claudeShellSession) CarriedContext() (int, string) {
	if r, ok := c.inner.(core.CarriedContextReporter); ok {
		return r.CarriedContext()
	}
	return 0, ""
}

func (c *claudeShellSession) GetContextUsage() *core.ContextUsage {
	if r, ok := c.inner.(core.ContextUsageReporter); ok {
		return r.GetContextUsage()
	}
	return nil
}

func (c *claudeShellSession) GetUsageSource() string {
	if r, ok := c.inner.(core.UsageSourceReporter); ok {
		return r.GetUsageSource()
	}
	return ""
}

func (c *claudeShellSession) StartupWarning() string {
	if w, ok := c.inner.(core.StartupWarner); ok {
		return w.StartupWarning()
	}
	return ""
}

func (c *claudeShellSession) SetLiveMode(mode string) bool {
	l, ok := c.inner.(core.LiveModeSwitcher)
	return ok && l.SetLiveMode(mode)
}

func (c *claudeShellSession) GetModel() string {
	if g, ok := c.inner.(interface{ GetModel() string }); ok {
		return g.GetModel()
	}
	return ""
}

func (c *claudeShellSession) GetWorkDir() string {
	if g, ok := c.inner.(interface{ GetWorkDir() string }); ok {
		return g.GetWorkDir()
	}
	return ""
}

// acpShellSession carries the capabilities of an ACP session.
type acpShellSession struct{ *session }

func (a *acpShellSession) CancelTurn() error {
	if c, ok := a.inner.(core.AgentSessionCanceller); ok {
		return c.CancelTurn()
	}
	return fmt.Errorf("model_router: cancel not supported")
}

func (a *acpShellSession) GetContextUsage() *core.ContextUsage {
	if r, ok := a.inner.(core.ContextUsageReporter); ok {
		return r.GetContextUsage()
	}
	return nil
}

func (a *acpShellSession) SetLiveMode(mode string) bool {
	l, ok := a.inner.(core.LiveModeSwitcher)
	return ok && l.SetLiveMode(mode)
}

var (
	_ core.AgentSession             = (*session)(nil)
	_ core.AgentSessionSteerer      = (*claudeShellSession)(nil)
	_ core.AgentSessionAsyncSteerer = (*claudeShellSession)(nil)
	_ core.BackgroundWorkReporter   = (*claudeShellSession)(nil)
	_ core.RunningTasksReporter     = (*claudeShellSession)(nil)
	_ core.TaskStopper              = (*claudeShellSession)(nil)
	_ core.InputFailureReporter     = (*claudeShellSession)(nil)
	_ core.CarriedContextReporter   = (*claudeShellSession)(nil)
	_ core.ContextUsageReporter     = (*claudeShellSession)(nil)
	_ core.UsageSourceReporter      = (*claudeShellSession)(nil)
	_ core.StartupWarner            = (*claudeShellSession)(nil)
	_ core.LiveModeSwitcher         = (*claudeShellSession)(nil)
	_ core.AgentSessionCanceller    = (*acpShellSession)(nil)
	_ core.ContextUsageReporter     = (*acpShellSession)(nil)
	_ core.LiveModeSwitcher         = (*acpShellSession)(nil)
)
