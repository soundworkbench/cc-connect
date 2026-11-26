package modelrouter

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// session wraps shell sessions and starts the selected shell lazily when the
// router is configured to keep automated turns off ACP.
type session struct {
	r          *Agent
	th         *thread
	ctx        context.Context
	shell      string
	modelShell string
	inner      core.AgentSession
	compactCmd string
	out        chan core.Event
	inject     chan core.Event
	done       chan struct{}
	once       sync.Once
	outOnce    sync.Once
	relayWG    sync.WaitGroup
	relayStop  chan struct{}
	startMu    sync.Mutex

	mu            sync.Mutex
	pending       *handoff
	compact       bool
	texts         []string
	lastID        string
	noticed       bool
	turnHuman     bool
	turnHumanSet  bool
	preserveShell bool
	closed        bool
	ended         bool
}

func (r *Agent) wrap(inner core.AgentSession, th *thread, shell string, h *handoff, ctx context.Context) core.AgentSession {
	capacity := 16
	if inner != nil && cap(inner.Events()) > 0 {
		capacity = cap(inner.Events())
	}
	s := &session{
		r: r, th: th, ctx: ctx, shell: shell, modelShell: shell, inner: inner,
		out: make(chan core.Event, capacity), inject: make(chan core.Event, 4), done: make(chan struct{}), pending: h,
	}
	s.setCompactCommand(shell)
	if inner != nil {
		s.noteID(inner.CurrentSessionID())
		s.startRelay(inner)
	}
	if shell == shellClaude {
		return &claudeShellSession{s}
	}
	if shell == shellACP || shell == shellT3 {
		return &acpShellSession{s}
	}
	return s
}

func (s *session) setCompactCommand(shell string) {
	if c, ok := s.r.shells[shell].(core.ContextCompressor); ok {
		s.compactCmd = strings.TrimSpace(c.CompressCommand())
	} else {
		s.compactCmd = ""
	}
}

func (s *session) startRelay(inner core.AgentSession) {
	s.mu.Lock()
	s.startRelayLocked(inner)
	s.mu.Unlock()
}

func (s *session) startRelayLocked(inner core.AgentSession) {
	stop := make(chan struct{})
	s.relayStop = stop
	s.relayWG.Add(1)
	go func() {
		defer s.relayWG.Done()
		for {
			select {
			case ev := <-s.inject:
				s.forward(ev)
				continue
			default:
			}
			select {
			case ev, ok := <-inner.Events():
				if !ok {
					s.mu.Lock()
					current := s.inner == inner && !s.closed
					if current {
						s.ended = true
					}
					s.mu.Unlock()
					if current {
						s.closeOut()
					}
					return
				}
				s.observe(ev)
				s.forward(ev)
			case ev := <-s.inject:
				s.forward(ev)
			case <-s.done:
				return
			case <-stop:
				return
			}
		}
	}()
}

func (s *session) closeOut() { s.outOnce.Do(func() { close(s.out) }) }

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
	shell := s.shell
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
		rec.Shell = shell
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
	shell := s.shell
	s.mu.Unlock()
	if !same {
		s.r.store.setID(s.th, shell, id)
	}
}

func (s *session) currentInner() core.AgentSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner
}

func (s *session) requestedShell() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preserveShell {
		s.preserveShell = false
		return s.shell
	}
	if !s.r.acpHumanOnly || s.modelShell != s.r.googleShell {
		return s.shell
	}
	if s.turnHumanSet && s.turnHuman {
		return s.r.googleShell
	}
	return shellClaude
}

func (s *session) ensureInner(target string) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("model_router: session is closed")
	}
	if s.ended {
		s.mu.Unlock()
		return fmt.Errorf("model_router: shell session ended")
	}
	if s.inner != nil && s.shell == target {
		s.mu.Unlock()
		return nil
	}
	old := s.inner
	oldRelayStop := s.relayStop
	s.inner = nil
	s.relayStop = nil
	s.mu.Unlock()
	if oldRelayStop != nil {
		close(oldRelayStop)
	}
	if old != nil {
		_ = old.Close()
		s.waitRelay()
	}

	snap := s.r.store.snapshot(s.th)
	from := snap.Active
	if from == "" {
		from = target
		s.r.store.setActive(s.th, target)
	}
	var inner core.AgentSession
	var h *handoff
	var err error
	if from == target {
		inner, err = s.r.shells[target].StartSession(s.ctx, snap.IDs[target])
	} else {
		var resumed bool
		inner, resumed, err = s.r.startForHandoff(s.ctx, target, snap.IDs[target])
		if err == nil {
			h = s.r.handoffFor(s.ctx, s.th, from, target, resumed)
		}
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = inner.Close()
		return fmt.Errorf("model_router: session is closed")
	}
	s.inner = inner
	s.shell = target
	s.pending = h
	s.setCompactCommand(target)
	s.startRelayLocked(inner)
	s.mu.Unlock()
	s.noteID(inner.CurrentSessionID())
	return nil
}

func (s *session) waitRelay() {
	done := make(chan struct{})
	go func() {
		s.relayWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("model_router: relay did not stop before timeout")
	}
}

func (s *session) SetTurnHuman(human bool) {
	s.mu.Lock()
	s.turnHuman, s.turnHumanSet, s.preserveShell = human, true, false
	s.mu.Unlock()
}

func (s *session) SetTurnShellCurrent() {
	s.mu.Lock()
	s.preserveShell = true
	s.mu.Unlock()
}

func (s *session) Send(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	target := s.requestedShell()
	if err := s.ensureInner(target); err != nil {
		return err
	}
	s.mu.Lock()
	inner := s.inner
	compact := s.compactCmd != "" && strings.TrimSpace(prompt) == s.compactCmd
	if compact {
		s.compact = true
	}
	h := s.pending
	notice := h != nil && h.notice != nil && !s.noticed
	if notice {
		s.noticed = true
	}
	shell := s.shell
	s.mu.Unlock()
	if inner == nil {
		return fmt.Errorf("model_router: shell session is unavailable")
	}
	if compact {
		return inner.Send(prompt, messageID, images, files)
	}
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
	if err := inner.Send(text, messageID, images, files); err != nil {
		return err
	}
	s.recordUser(at, shell, prompt, len(images)+len(files))
	if h != nil {
		s.mu.Lock()
		first := s.pending == h
		if first {
			s.pending = nil
		}
		s.mu.Unlock()
		if first && h.from != "" {
			s.r.store.moved(s.th, h.from, shell, at)
		}
	}
	return nil
}

func (s *session) recordUser(at time.Time, shell, prompt string, attachments int) {
	text := strings.TrimSpace(prompt)
	if attachments > 0 {
		text = strings.TrimSpace(fmt.Sprintf("%s [%d attachment(s)]", text, attachments))
	}
	s.r.store.record(s.th, entry{Time: at, Shell: shell, Role: roleUser, Content: text})
}

func (s *session) RespondPermission(requestID string, result core.PermissionResult) error {
	inner := s.currentInner()
	if inner == nil {
		return fmt.Errorf("model_router: no active shell session")
	}
	return inner.RespondPermission(requestID, result)
}

func (s *session) Events() <-chan core.Event { return s.out }

func (s *session) CurrentSessionID() string {
	inner := s.currentInner()
	if inner == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.lastID
	}
	id := inner.CurrentSessionID()
	s.noteID(id)
	return id
}

func (s *session) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && (s.inner == nil || s.inner.Alive())
}

func (s *session) Close() error {
	var err error
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		inner := s.inner
		relayStop := s.relayStop
		s.inner = nil
		s.relayStop = nil
		s.mu.Unlock()
		close(s.done)
		if relayStop != nil {
			close(relayStop)
		}
		if inner != nil {
			err = inner.Close()
		}
		s.waitRelay()
		s.closeOut()
	})
	return err
}

func (s *session) SupportsPermissionApprovalAll() bool {
	s.mu.Lock()
	inner, shell, modelShell := s.inner, s.shell, s.modelShell
	s.mu.Unlock()
	if !s.r.acpHumanOnly || modelShell != s.r.googleShell {
		return true
	}
	if capability, ok := inner.(core.PermissionApprovalAll); ok {
		return capability.SupportsPermissionApprovalAll()
	}
	return shell != s.r.googleShell
}

// RoutesByTurnOrigin lets queue batching split human and automated turns
// before any session exists.
func (r *Agent) RoutesByTurnOrigin() bool {
	return r.acpHumanOnly && r.currentShell() == r.googleShell
}

func (s *session) RoutesByTurnOrigin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.acpHumanOnly && s.modelShell == s.r.googleShell
}

func (s *session) HasBackgroundWork() bool {
	r, ok := s.currentInner().(core.BackgroundWorkReporter)
	return ok && r.HasBackgroundWork()
}

func (s *session) RunningTasks() int {
	if r, ok := s.currentInner().(core.RunningTasksReporter); ok {
		return r.RunningTasks()
	}
	return 0
}

type claudeShellSession struct{ *session }

func (c *claudeShellSession) Steer(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) (bool, error) {
	st, ok := c.currentInner().(core.AgentSessionSteerer)
	if !ok {
		return false, nil
	}
	at := time.Now()
	accepted, err := st.Steer(prompt, messageID, images, files)
	if accepted && err == nil {
		c.recordUser(at, c.shell, prompt, len(images)+len(files))
	}
	return accepted, err
}

func (c *claudeShellSession) SteerAsync(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) (*core.SteerReceipt, error) {
	st, ok := c.currentInner().(core.AgentSessionAsyncSteerer)
	if !ok {
		return nil, fmt.Errorf("model_router: steering not supported")
	}
	at := time.Now()
	receipt, err := st.SteerAsync(prompt, messageID, images, files)
	if receipt != nil && err == nil {
		c.recordUser(at, c.shell, prompt, len(images)+len(files))
	}
	return receipt, err
}

func (c *claudeShellSession) StopTask(taskID string) error {
	if t, ok := c.currentInner().(core.TaskStopper); ok {
		return t.StopTask(taskID)
	}
	return fmt.Errorf("model_router: stopping tasks not supported")
}
func (c *claudeShellSession) InputFailure() *core.InputWriteError {
	if r, ok := c.currentInner().(core.InputFailureReporter); ok {
		return r.InputFailure()
	}
	return nil
}
func (c *claudeShellSession) CarriedContext() (int, string) {
	if r, ok := c.currentInner().(core.CarriedContextReporter); ok {
		return r.CarriedContext()
	}
	return 0, ""
}
func (c *claudeShellSession) GetContextUsage() *core.ContextUsage {
	if r, ok := c.currentInner().(core.ContextUsageReporter); ok {
		return r.GetContextUsage()
	}
	return nil
}
func (c *claudeShellSession) GetUsageSource() string {
	if r, ok := c.currentInner().(core.UsageSourceReporter); ok {
		return r.GetUsageSource()
	}
	return ""
}
func (c *claudeShellSession) StartupWarning() string {
	if w, ok := c.currentInner().(core.StartupWarner); ok {
		return w.StartupWarning()
	}
	return ""
}
func (c *claudeShellSession) SetLiveMode(mode string) bool {
	l, ok := c.currentInner().(core.LiveModeSwitcher)
	return ok && l.SetLiveMode(mode)
}
func (c *claudeShellSession) GetModel() string {
	if g, ok := c.currentInner().(interface{ GetModel() string }); ok {
		return g.GetModel()
	}
	return ""
}
func (c *claudeShellSession) GetWorkDir() string {
	if g, ok := c.currentInner().(interface{ GetWorkDir() string }); ok {
		return g.GetWorkDir()
	}
	return ""
}

type acpShellSession struct{ *session }

func (a *acpShellSession) CancelTurn() error {
	if c, ok := a.currentInner().(core.AgentSessionCanceller); ok {
		return c.CancelTurn()
	}
	return fmt.Errorf("model_router: cancel not supported")
}
func (a *acpShellSession) GetContextUsage() *core.ContextUsage {
	if r, ok := a.currentInner().(core.ContextUsageReporter); ok {
		return r.GetContextUsage()
	}
	return nil
}
func (a *acpShellSession) SetLiveMode(mode string) bool {
	l, ok := a.currentInner().(core.LiveModeSwitcher)
	return ok && l.SetLiveMode(mode)
}

var (
	_ core.AgentSession             = (*session)(nil)
	_ core.AgentSessionSteerer      = (*claudeShellSession)(nil)
	_ core.AgentSessionAsyncSteerer = (*claudeShellSession)(nil)
	_ core.BackgroundWorkReporter   = (*session)(nil)
	_ core.RunningTasksReporter     = (*session)(nil)
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
	_ core.TurnOriginSetter         = (*session)(nil)
	_ core.TurnShellPreserver       = (*session)(nil)
	_ core.TurnOriginRouter         = (*session)(nil)
	_ core.PermissionApprovalAll    = (*session)(nil)
)
