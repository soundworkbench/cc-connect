package modelrouter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// ── fakes ────────────────────────────────────────────────────

type fakeShell struct {
	name string

	mu        sync.Mutex
	model     string
	providers []core.ProviderConfig
	active    string
	starts    []string // session ids StartSession was called with
	sessions  []*fakeSession
	failStart func(id string) error
	next      int
	window    int
	history   map[string][]core.HistoryEntry
}

func (f *fakeShell) Name() string { return f.name }

func (f *fakeShell) StartSession(_ context.Context, id string) (core.AgentSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, id)
	if f.failStart != nil {
		if err := f.failStart(id); err != nil {
			return nil, err
		}
	}
	if id == "" {
		f.next++
		id = fmt.Sprintf("%s-%d", f.name, f.next)
	}
	base := &fakeSession{shell: f.name, id: id, events: make(chan core.Event, 16), alive: true}
	f.sessions = append(f.sessions, base)
	return base, nil
}

func (f *fakeShell) ListSessions(context.Context) ([]core.AgentSessionInfo, error) { return nil, nil }
func (f *fakeShell) Stop() error                                                   { return nil }
func (f *fakeShell) SetModel(m string)                                             { f.mu.Lock(); f.model = m; f.mu.Unlock() }
func (f *fakeShell) GetModel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return core.GetProviderModel(f.providers, f.activeIdxLocked(), f.model)
}
func (f *fakeShell) activeIdxLocked() int {
	for i, p := range f.providers {
		if p.Name == f.active {
			return i
		}
	}
	return -1
}
func (f *fakeShell) AvailableModels(context.Context) []core.ModelOption {
	if f.name == shellACP {
		return []core.ModelOption{{Name: "gemini-4-neon", Alias: "Neon"}, {Name: "gemini-3.8-flash"}, {Name: "claude-opus-5-5"}}
	}
	return []core.ModelOption{{Name: "opus"}}
}
func (f *fakeShell) CompressCommand() string { return "/compact" }
func (f *fakeShell) CLIBinaryName() string   { return f.name }
func (f *fakeShell) CLIDisplayName() string {
	if f.name == shellACP {
		return "Antigravity"
	}
	return "Claude"
}

func (f *fakeShell) lastSession() *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[len(f.sessions)-1]
}

func (f *fakeShell) startedWith() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.starts...)
}

// fakeClaude adds what the Claude Code agent has.
type fakeClaude struct{ *fakeShell }

func (f *fakeClaude) SetProviders(p []core.ProviderConfig) {
	f.mu.Lock()
	f.providers = p
	f.mu.Unlock()
}
func (f *fakeClaude) SetActiveProvider(name string) bool {
	f.mu.Lock()
	f.active = name
	f.mu.Unlock()
	return true
}
func (f *fakeClaude) GetActiveProvider() *core.ProviderConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := f.activeIdxLocked(); i >= 0 {
		p := f.providers[i]
		return &p
	}
	return nil
}
func (f *fakeClaude) ListProviders() []core.ProviderConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.ProviderConfig(nil), f.providers...)
}
func (f *fakeClaude) SwitchCompactLine(string) int { return f.window * 3 / 4 }
func (f *fakeClaude) StartSession(ctx context.Context, id string) (core.AgentSession, error) {
	s, err := f.fakeShell.StartSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return &fakeClaudeSession{s.(*fakeSession)}, nil
}
func (f *fakeClaude) GetSessionHistory(_ context.Context, id string, limit int) ([]core.HistoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.history[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return h, nil
}

// fakeACP adds what the ACP agent's sessions have.
type fakeACP struct{ *fakeShell }

func (f *fakeACP) StartSession(ctx context.Context, id string) (core.AgentSession, error) {
	s, err := f.fakeShell.StartSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return &fakeACPSession{s.(*fakeSession)}, nil
}

type fakeSession struct {
	shell  string
	id     string
	events chan core.Event

	mu    sync.Mutex
	sent  []string
	alive bool
}

func (s *fakeSession) Send(prompt, _ string, _ []core.ImageAttachment, _ []core.FileAttachment) error {
	s.mu.Lock()
	if !s.alive {
		s.mu.Unlock()
		return errors.New("session process is not running")
	}
	s.sent = append(s.sent, prompt)
	s.mu.Unlock()
	last := prompt
	if i := strings.LastIndex(prompt, "\n\n"); i >= 0 {
		last = prompt[i+2:]
	}
	s.events <- core.Event{Type: core.EventToolUse, ToolName: "Bash", ToolInput: "ls -la\necho second line"}
	s.events <- core.Event{Type: core.EventToolResult, ToolResult: "SECRET-OUTPUT"}
	s.events <- core.Event{Type: core.EventResult, Content: s.shell + " answers " + last, SessionID: s.id, Done: true}
	return nil
}
func (s *fakeSession) RespondPermission(string, core.PermissionResult) error { return nil }
func (s *fakeSession) Events() <-chan core.Event                             { return s.events }
func (s *fakeSession) CurrentSessionID() string                              { return s.id }
func (s *fakeSession) Alive() bool                                           { s.mu.Lock(); defer s.mu.Unlock(); return s.alive }
func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alive {
		s.alive = false
		close(s.events)
	}
	return nil
}
func (s *fakeSession) prompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

type fakeClaudeSession struct{ *fakeSession }

func (s *fakeClaudeSession) Steer(p, m string, i []core.ImageAttachment, f []core.FileAttachment) (bool, error) {
	return true, nil
}
func (s *fakeClaudeSession) CarriedContext() (int, string) { return 1234, "claude-opus-5-5" }

type fakeACPSession struct{ *fakeSession }

func (s *fakeACPSession) CancelTurn() error { return nil }

func newTestRouter(t *testing.T, model string) (*Agent, *fakeClaude, *fakeACP) {
	t.Helper()
	cc := &fakeClaude{&fakeShell{name: shellClaude, window: 400_000, history: map[string][]core.HistoryEntry{}}}
	acp := &fakeACP{&fakeShell{name: shellACP}}
	r := newRouter(cc, acp, nil, model, newStore(""))
	return r, cc, acp
}

func newT3TestRouter(t *testing.T, model string) (*Agent, *fakeClaude, *fakeACP) {
	t.Helper()
	cc := &fakeClaude{&fakeShell{name: shellClaude, window: 400_000, history: map[string][]core.HistoryEntry{}}}
	t3 := &fakeACP{&fakeShell{name: shellT3}}
	r := newRouter(cc, t3, nil, model, newStore(""))
	delete(r.shells, shellACP)
	r.shells[shellT3] = t3
	r.names[shellT3] = "T3 Code"
	r.googleShell = shellT3
	r.acpHumanOnly = true
	r.sync()
	return r, cc, t3
}

// turn sends prompt and reads events up to the result; it returns the
// notices seen.
func turn(t *testing.T, s core.AgentSession, prompt string) []*core.Notice {
	t.Helper()
	if err := s.Send(prompt, "", nil, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	var notices []*core.Notice
	timeout := time.After(3 * time.Second)
	gotResult := false
	for {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventNotice:
				notices = append(notices, ev.Notice)
			case core.EventResult:
				gotResult = true
			}
		case <-time.After(50 * time.Millisecond):
			if gotResult {
				return notices
			}
		case <-timeout:
			t.Fatal("no result")
		}
	}
}

func start(t *testing.T, r *Agent, id string) core.AgentSession {
	t.Helper()
	s, err := r.StartSession(context.Background(), id)
	if err != nil {
		t.Fatalf("start %q: %v", id, err)
	}
	return s
}

// ── tests ────────────────────────────────────────────────────

func TestShellForDefaults(t *testing.T) {
	r, _, _ := newTestRouter(t, "")
	cases := map[string]string{
		"claude-opus-5-5":        shellClaude,
		"Claude-Sonnet-5-5[1m]":  shellClaude,
		"claude/claude-opus-5-5": shellClaude,
		"opus":                   shellClaude,
		"sonnet[1m]":             shellClaude,
		"fable":                  shellClaude,
		"gemini-4-neon":          shellACP,
		"gemini-3.8-flash":       shellACP,
		"google/gemini-4-neon":   shellACP,
		"gpt-5":                  shellACP,
		"":                       shellClaude,
	}
	for model, want := range cases {
		if got := r.shellFor(model); got != want {
			t.Errorf("shellFor(%q) = %s, want %s", model, got, want)
		}
	}
}

func TestShellForConfiguredPatterns(t *testing.T) {
	cc := &fakeClaude{&fakeShell{name: shellClaude}}
	acp := &fakeACP{&fakeShell{name: shellACP}}
	r := newRouter(cc, acp, []string{"Claude-*", "kimi*"}, "", newStore(""))
	if r.shellFor("kimi-k3") != shellClaude || r.shellFor("opus") != shellACP || r.shellFor("claude-opus-5-5") != shellClaude {
		t.Fatalf("configured patterns not used: kimi=%s opus=%s", r.shellFor("kimi-k3"), r.shellFor("opus"))
	}
}

func TestSessionsRouteByModel(t *testing.T) {
	r, cc, acp := newTestRouter(t, "claude-opus-5-5")
	s := start(t, r, "")
	if _, ok := s.(core.AgentSessionSteerer); !ok {
		t.Fatal("claude session should keep steering")
	}
	if tc, ok := s.(core.TurnCancelReporter); !ok || tc.CanCancelTurn() {
		t.Fatal("/stop on a Claude Code session must still close it")
	}
	if len(cc.startedWith()) != 1 || len(acp.startedWith()) != 0 {
		t.Fatalf("starts: cc=%v acp=%v", cc.startedWith(), acp.startedWith())
	}
	if cc.GetModel() != "claude-opus-5-5" {
		t.Fatalf("claude model = %q", cc.GetModel())
	}

	r.SetModel("gemini-4-neon")
	if acp.GetModel() != "gemini-4-neon" {
		t.Fatalf("acp model = %q", acp.GetModel())
	}
	if cc.GetModel() != "claude-opus-5-5" {
		t.Fatalf("claude must keep its model, got %q", cc.GetModel())
	}
	s2 := start(t, r, "")
	if tc, ok := s2.(core.TurnCancelReporter); !ok || !tc.CanCancelTurn() {
		t.Fatal("acp session should keep CancelTurn")
	}
	if rc, err := s2.(core.AgentSessionAsyncSteerer).SteerAsync("more", "m1", nil, nil); rc != nil || err != nil {
		t.Fatalf("acp session must decline steering so the message queues: %v, %v", rc, err)
	}
}

func TestACPHumanOnlyRoutesAutomationToClaude(t *testing.T) {
	r, cc, acp := newTestRouter(t, "gemini-4-neon")
	r.acpHumanOnly = true
	s := start(t, r, "")
	if len(cc.startedWith()) != 0 || len(acp.startedWith()) != 0 {
		t.Fatalf("lazy start violated: cc=%v acp=%v", cc.startedWith(), acp.startedWith())
	}

	if origin, ok := s.(core.TurnOriginSetter); ok {
		origin.SetTurnHuman(false)
	} else {
		t.Fatal("router session does not expose turn origin")
	}
	turn(t, s, "scheduled task")
	if len(cc.startedWith()) != 1 || len(acp.startedWith()) != 0 {
		t.Fatalf("automation starts: cc=%v acp=%v", cc.startedWith(), acp.startedWith())
	}

	s.(core.TurnOriginSetter).SetTurnHuman(true)
	turn(t, s, "owner message")
	if len(acp.startedWith()) != 1 {
		t.Fatalf("human message should start ACP once: acp=%v", acp.startedWith())
	}

	s.(core.TurnOriginSetter).SetTurnHuman(false)
	turn(t, s, "another scheduled task")
	if len(cc.startedWith()) != 2 || len(acp.startedWith()) != 1 {
		t.Fatalf("later automation starts: cc=%v acp=%v", cc.startedWith(), acp.startedWith())
	}
}

func TestT3HumanOnlyRoutesAutomationToClaude(t *testing.T) {
	r, cc, t3 := newT3TestRouter(t, "gemini-4-neon")
	s := start(t, r, "")
	if r.RoutesByTurnOrigin() == false {
		t.Fatal("T3 human-only router must advertise origin routing")
	}
	if origin, ok := s.(core.TurnOriginSetter); ok {
		origin.SetTurnHuman(false)
	} else {
		t.Fatal("router session does not expose turn origin")
	}
	turn(t, s, "scheduled task")
	if len(cc.startedWith()) != 1 || len(t3.startedWith()) != 0 {
		t.Fatalf("automation starts: claude=%v t3=%v", cc.startedWith(), t3.startedWith())
	}
	s.(core.TurnOriginSetter).SetTurnHuman(true)
	turn(t, s, "owner message")
	if len(t3.startedWith()) != 1 {
		t.Fatalf("human message should start T3 once: t3=%v", t3.startedWith())
	}
	if capability, ok := s.(core.PermissionApprovalAll); !ok || capability.SupportsPermissionApprovalAll() {
		t.Fatal("T3 shell must expose allow-once permission semantics")
	}
}

func TestT3HumanOnlyAdvertisesMixedBatchBoundary(t *testing.T) {
	r, _, _ := newT3TestRouter(t, "gemini-4-neon")
	s := start(t, r, "")
	if !r.RoutesByTurnOrigin() {
		t.Fatal("router must split mixed human and automation batches")
	}
	if routed, ok := s.(core.TurnOriginRouter); !ok || !routed.RoutesByTurnOrigin() {
		t.Fatal("T3 session must split mixed human and automation batches")
	}
}

func TestDeadInnerSendDoesNotRestartOrWriteClosedEvents(t *testing.T) {
	r, cc, _ := newTestRouter(t, "claude-opus-5-5")
	s := start(t, r, "")
	_ = cc.lastSession().Close()
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("session events stayed open after inner exit")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session events did not close after inner exit")
	}
	if err := s.Send("queued", "", nil, nil); err == nil {
		t.Fatal("send after inner exit unexpectedly succeeded")
	}
	if got := len(cc.startedWith()); got != 1 {
		t.Fatalf("dead inner was restarted, starts=%v", cc.startedWith())
	}
}

func TestCloseDuringLazyStartClosesReturnedInner(t *testing.T) {
	r, _, acp := newTestRouter(t, "gemini-4-neon")
	r.acpHumanOnly = true
	acp.failStart = func(string) error {
		time.Sleep(300 * time.Millisecond)
		return nil
	}
	s := start(t, r, "")
	s.(core.TurnOriginSetter).SetTurnHuman(true)
	done := make(chan error, 1)
	go func() { done <- s.Send("owner", "", nil, nil) }()
	time.Sleep(100 * time.Millisecond)
	_ = s.Close()
	if err := <-done; err == nil {
		t.Fatal("send succeeded after session close during lazy start")
	}
	if acp.lastSession().Alive() {
		t.Fatal("lazy-started ACP session leaked after close")
	}
}

func TestFirstAutomatedTurnDoesNotAnnounceShellSwitch(t *testing.T) {
	r, _, _ := newTestRouter(t, "gemini-4-neon")
	r.acpHumanOnly = true
	s := start(t, r, "")
	s.(core.TurnOriginSetter).SetTurnHuman(false)
	if notices := turn(t, s, "cron job"); len(notices) != 0 {
		t.Fatalf("automated turn announced a shell switch: %v", notices)
	}
}

func TestNormalACPStillAllowsPersistentApproval(t *testing.T) {
	r, _, _ := newTestRouter(t, "gemini-4-neon")
	s := start(t, r, "")
	if capability, ok := s.(core.PermissionApprovalAll); !ok || !capability.SupportsPermissionApprovalAll() {
		t.Fatal("normal ACP model_router session lost persistent approval")
	}
}

func TestHandoffToACPCanBeDisabled(t *testing.T) {
	r, _, acp := newTestRouter(t, "claude-opus-5-5")
	r.handoffToACP = false
	s := start(t, r, "")
	turn(t, s, "before switch")
	ccID := s.CurrentSessionID()
	_ = s.Close()

	r.SetModel("gemini-4-neon")
	s = start(t, r, ccID)
	s.(core.TurnOriginSetter).SetTurnHuman(true)
	turn(t, s, "after switch")
	prompt := acp.lastSession().prompts()[0]
	if prompt != "after switch" {
		t.Fatalf("ACP prompt contains disabled handoff: %q", prompt)
	}
}

func TestHandoffBothWaysOnlyIncrements(t *testing.T) {
	r, cc, acp := newTestRouter(t, "claude-opus-5-5")

	s := start(t, r, "")
	if n := turn(t, s, "first question"); len(n) != 0 {
		t.Fatalf("no notice expected on a new conversation, got %v", n)
	}
	ccID := s.CurrentSessionID()
	_ = s.Close()

	// Claude → Antigravity: new session, gets the conversation so far.
	r.SetModel("gemini-4-neon")
	s = start(t, r, ccID)
	n := turn(t, s, "second question")
	if len(n) != 1 || n[0].Key != core.MsgShellSwitchedCarried || n[0].Args[0] != "Antigravity" {
		t.Fatalf("notice = %+v", n)
	}
	got := acp.lastSession().prompts()[0]
	for _, want := range []string{`<handoff from="Claude Code">`, "User: first question", "Assistant: claudecode answers first question", "Tool: Bash — ls -la …", "</handoff>\n\nsecond question"} {
		if !strings.Contains(got, want) {
			t.Fatalf("handoff to acp lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "SECRET-OUTPUT") || strings.Contains(got, "second line") {
		t.Fatalf("tool output leaked into handoff:\n%s", got)
	}
	if strings.HasPrefix(acp.lastSession().prompts()[0], "second question") {
		t.Fatal("handoff missing")
	}
	turn(t, s, "third question")
	if p := acp.lastSession().prompts()[1]; p != "third question" {
		t.Fatalf("only the first message carries the handoff, got %q", p)
	}
	acpID := s.CurrentSessionID()
	_ = s.Close()

	// Antigravity → Claude: resumes the Claude session, gets only what
	// happened in Antigravity.
	r.SetModel("claude-opus-5-5")
	s = start(t, r, acpID)
	n = turn(t, s, "fourth question")
	if starts := cc.startedWith(); starts[len(starts)-1] != ccID {
		t.Fatalf("claude should resume %s, started with %v", ccID, starts)
	}
	if len(n) != 1 || n[0].Key != core.MsgShellSwitchedCarried || n[0].Args[0] != "Claude Code" {
		t.Fatalf("notice = %+v", n)
	}
	got = cc.lastSession().prompts()[0]
	if !strings.Contains(got, `<handoff from="Antigravity">`) || !strings.Contains(got, "since you last took part") {
		t.Fatalf("resume handoff header wrong:\n%s", got)
	}
	for _, want := range []string{"User: second question", "User: third question", "Assistant: acp answers third question"} {
		if !strings.Contains(got, want) {
			t.Fatalf("handoff to claude lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "first question") {
		t.Fatalf("claude already has the first question:\n%s", got)
	}
	_ = s.Close()

	// Claude → Antigravity again: resumes, gets only the fourth exchange.
	r.SetModel("gemini-4-neon")
	s = start(t, r, ccID)
	turn(t, s, "fifth question")
	if starts := acp.startedWith(); starts[len(starts)-1] != acpID {
		t.Fatalf("acp should resume %s, started with %v", acpID, starts)
	}
	got = acp.lastSession().prompts()[0]
	if !strings.Contains(got, "User: fourth question") || !strings.Contains(got, "Assistant: claudecode answers fourth question") {
		t.Fatalf("increment missing:\n%s", got)
	}
	for _, old := range []string{"first question", "second question", "third question"} {
		if strings.Contains(got, old) {
			t.Fatalf("increment repeats %q:\n%s", old, got)
		}
	}
	_ = s.Close()

	// Same shell again: no handoff, no notice.
	s = start(t, r, acpID)
	if n := turn(t, s, "sixth question"); len(n) != 0 {
		t.Fatalf("same shell must not notify: %+v", n)
	}
	if p := acp.lastSession().prompts()[0]; p != "sixth question" {
		t.Fatalf("same shell must not hand off, got %q", p)
	}
}

func TestHandoffTruncatesKeepingRecent(t *testing.T) {
	r, _, acp := newTestRouter(t, "claude-opus-5-5")
	s := start(t, r, "")
	long := strings.Repeat("字", 3000)
	for i := 0; i < 30; i++ {
		turn(t, s, fmt.Sprintf("q%02d %s", i, long))
	}
	id := s.CurrentSessionID()
	_ = s.Close()

	r.SetModel("gemini-4-neon")
	s = start(t, r, id)
	turn(t, s, "go on")
	got := acp.lastSession().prompts()[0]
	h := got[:strings.Index(got, handoffClose)]
	budget := handoffBudget(0, 0)
	if runeLen(h) > budget+500 {
		t.Fatalf("handoff %d chars over budget %d", runeLen(h), budget)
	}
	if !strings.Contains(h, "earlier messages omitted") {
		t.Fatal("dropped messages not noted")
	}
	if !strings.Contains(h, "q29 ") || strings.Contains(h, "q00 ") {
		t.Fatal("handoff should keep the newest messages and drop the oldest")
	}
}

func TestHandoffBudget(t *testing.T) {
	if got := handoffBudget(0, 0); got != 20_000 {
		t.Fatalf("default budget = %d", got)
	}
	if got := handoffBudget(0, 50_000); got != 50_000 {
		t.Fatalf("configured budget = %d", got)
	}
	if got := handoffBudget(1_000_000, 20_000); got != 20_000 {
		t.Fatalf("big window must not raise the budget: %d", got)
	}
	if got := handoffBudget(16_000, 20_000); got != 8_000 {
		t.Fatalf("small window caps the budget: %d", got)
	}
	if got := handoffBudget(1_000, 20_000); got != handoffMinRunes {
		t.Fatalf("budget floor: %d", got)
	}
}

func TestHandoffKeepsRecentMessagesWhole(t *testing.T) {
	var entries []entry
	for i := 0; i < 10; i++ {
		entries = append(entries,
			entry{Role: roleUser, Content: fmt.Sprintf("q%d %s", i, strings.Repeat("问", 1500))},
			entry{Role: roleAssistant, Content: fmt.Sprintf("a%d %s", i, strings.Repeat("答", 1500))})
	}
	h := buildHandoff("Claude Code", entries, false, 7000)
	if runeLen(h) > 7000 {
		t.Fatalf("handoff %d chars over budget", runeLen(h))
	}
	if !strings.HasPrefix(h, handoffOpen) || !strings.HasSuffix(h, handoffClose) {
		t.Fatal("handoff frame broken")
	}
	// Every kept message is whole: its full text is there.
	for _, l := range strings.Split(h, "\n") {
		if strings.HasPrefix(l, "User: q") || strings.HasPrefix(l, "Assistant: a") {
			if runeLen(l) < 1500 {
				t.Fatalf("message cut short: %d chars", runeLen(l))
			}
		}
	}
	if !strings.Contains(h, "a9 ") || strings.Contains(h, "q0 ") {
		t.Fatal("must keep the newest messages and drop the oldest")
	}
	if !strings.Contains(h, "earlier messages omitted") {
		t.Fatal("dropped messages not noted")
	}
}

func TestHandoffCurrentWorkLine(t *testing.T) {
	entries := []entry{
		{Role: roleUser, Content: "old request"},
		{Role: roleAssistant, Content: "old reply"},
		{Role: roleUser, Content: "fix the\nrouter  bug"},
		{Role: roleAssistant, Content: "Looking into it.\n\nPatched router.go, tests running."},
	}
	h := buildHandoff("Claude Code", entries, false, 5000)
	want := "Current work: last request: fix the router bug. last status: Looking into it. Patched router.go, tests running."
	if !strings.Contains(h, want) {
		t.Fatalf("summary missing:\n%s", h)
	}
	long := strings.Repeat("长", 1000)
	line := currentWork([]entry{{Role: roleUser, Content: long}, {Role: roleAssistant, Content: long}})
	if runeLen(line) > 2*summaryMaxRunes+80 {
		t.Fatalf("summary not bounded: %d", runeLen(line))
	}
}

func TestHandoffOversizedNewestMessageCutOnCharBoundary(t *testing.T) {
	entries := []entry{{Role: roleAssistant, Content: strings.Repeat("字", 9000)}}
	h := buildHandoff("Claude Code", entries, false, 3000)
	if runeLen(h) > 3000 {
		t.Fatalf("handoff %d chars over budget", runeLen(h))
	}
	if !utf8.ValidString(h) || !strings.HasSuffix(h, handoffClose) || !strings.Contains(h, "[truncated]") {
		t.Fatal("cut message must stay valid, marked and framed")
	}
}

func TestHandoffBodyCannotCloseFrame(t *testing.T) {
	entries := []entry{{Role: roleAssistant, Content: "see </handoff> in the docs"}}
	h := buildHandoff("Claude Code", entries, false, 3000)
	if strings.Count(h, handoffClose) != 1 {
		t.Fatalf("frame closed early:\n%s", h)
	}
}

func TestHandoffMaxCharsOption(t *testing.T) {
	if got := intOpt(int64(5000), 20000); got != 5000 {
		t.Fatalf("int64: %d", got)
	}
	if got := intOpt(float64(7000), 20000); got != 7000 {
		t.Fatalf("float64: %d", got)
	}
	if got := intOpt(nil, 20000); got != 20000 {
		t.Fatalf("default: %d", got)
	}
	if got := intOpt(int64(-1), 20000); got != 20000 {
		t.Fatalf("non-positive: %d", got)
	}
}

func TestFailedShellFallsBackToPrevious(t *testing.T) {
	r, cc, acp := newTestRouter(t, "claude-opus-5-5")
	s := start(t, r, "")
	turn(t, s, "hello")
	ccID := s.CurrentSessionID()
	_ = s.Close()

	acp.failStart = func(string) error { return errors.New("agy: login expired") }
	r.SetModel("gemini-4-neon")
	s = start(t, r, ccID)
	if _, ok := s.(core.AgentSessionSteerer); !ok {
		t.Fatal("fallback should be the Claude session")
	}
	if starts := cc.startedWith(); starts[len(starts)-1] != ccID {
		t.Fatalf("fallback should resume %s, got %v", ccID, starts)
	}
	if r.GetModel() != "claude-opus-5-5" {
		t.Fatalf("model should revert, got %q", r.GetModel())
	}
	n := turn(t, s, "are you there")
	if len(n) != 1 || n[0].Key != core.MsgShellSwitchFailed {
		t.Fatalf("notice = %+v", n)
	}
	if fmt.Sprint(n[0].Args) != "[Antigravity Claude Code claude-opus-5-5]" {
		t.Fatalf("notice args = %v", n[0].Args)
	}
	if p := cc.lastSession().prompts(); len(p) != 1 || p[0] != "are you there" {
		t.Fatalf("message must reach the previous shell unchanged, got %q", p)
	}
}

func TestBothShellsFailingReturnsError(t *testing.T) {
	r, cc, acp := newTestRouter(t, "claude-opus-5-5")
	s := start(t, r, "")
	turn(t, s, "hello")
	id := s.CurrentSessionID()
	_ = s.Close()
	acp.failStart = func(string) error { return errors.New("acp down") }
	cc.failStart = func(string) error { return errors.New("claude down") }
	r.SetModel("gemini-4-neon")
	if _, err := r.StartSession(context.Background(), id); err == nil {
		t.Fatal("expected an error")
	}
}

func TestResumeFailureStartsFreshWithRecentConversation(t *testing.T) {
	r, _, acp := newTestRouter(t, "claude-opus-5-5")
	s := start(t, r, "")
	turn(t, s, "q1")
	ccID := s.CurrentSessionID()
	_ = s.Close()
	r.SetModel("gemini-4-neon")
	s = start(t, r, ccID)
	turn(t, s, "q2")
	acpID := s.CurrentSessionID()
	_ = s.Close()
	r.SetModel("claude-opus-5-5")
	s = start(t, r, acpID)
	turn(t, s, "q3")
	_ = s.Close()

	acp.failStart = func(id string) error {
		if id != "" {
			return errors.New("session not found")
		}
		return nil
	}
	r.SetModel("gemini-4-neon")
	s = start(t, r, ccID)
	turn(t, s, "q4")
	got := acp.lastSession().prompts()[0]
	if !strings.Contains(got, "Conversation so far") {
		t.Fatalf("fresh session should get the conversation so far:\n%s", got)
	}
	for _, want := range []string{"User: q1", "User: q2", "User: q3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("fresh handoff lacks %q:\n%s", want, got)
		}
	}
}

func TestCompactCommandPassesThrough(t *testing.T) {
	r, _, acp := newTestRouter(t, "claude-opus-5-5")
	s := start(t, r, "")
	turn(t, s, "q1")
	id := s.CurrentSessionID()
	_ = s.Close()
	r.SetModel("gemini-4-neon")
	s = start(t, r, id)
	turn(t, s, "/compact")
	turn(t, s, "q2")
	p := acp.lastSession().prompts()
	if p[0] != "/compact" {
		t.Fatalf("compact command must pass untouched, got %q", p[0])
	}
	if !strings.Contains(p[1], "User: q1") {
		t.Fatalf("handoff should ride on the first user message:\n%s", p[1])
	}
	hist, err := r.GetSessionHistory(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hist {
		if strings.Contains(h.Content, "compact") {
			t.Fatalf("compact turn recorded: %+v", h)
		}
	}
}

func TestHandoffFallsBackToShellHistory(t *testing.T) {
	r, cc, acp := newTestRouter(t, "gemini-4-neon")
	// A Claude session from before the router: known only to Claude Code.
	r.SetModel("claude-opus-5-5")
	old := time.Now().Add(-time.Hour)
	cc.history["pre-1"] = []core.HistoryEntry{
		{Role: "user", Content: "old question", Timestamp: old},
		{Role: "assistant", Content: "old answer", Timestamp: old.Add(time.Second)},
	}
	r.store.threadFor("pre-1", shellClaude)
	r.SetModel("gemini-4-neon")
	s := start(t, r, "pre-1")
	turn(t, s, "new question")
	got := acp.lastSession().prompts()[0]
	if !strings.Contains(got, "User: old question") || !strings.Contains(got, "Assistant: old answer") {
		t.Fatalf("handoff should use Claude's own history:\n%s", got)
	}
}

func TestThreadsPersist(t *testing.T) {
	dir := t.TempDir()
	st := newStore(dir)
	th := st.threadFor("", "")
	st.setID(th, shellClaude, "cc-1")
	st.record(th, entry{Time: time.Now(), Shell: shellClaude, Role: roleUser, Content: "hi"})
	st.setID(th, shellACP, "acp-1")
	st.moved(th, shellClaude, shellACP, time.Now())

	again := newStore(dir)
	again.load()
	got := again.lookup("acp-1")
	if got == nil || got != again.lookup("cc-1") {
		t.Fatal("both ids should find the same thread after reload")
	}
	if got.Active != shellACP || got.IDs[shellClaude] != "cc-1" || got.Left[shellClaude].IsZero() {
		t.Fatalf("thread state lost: %+v", got)
	}
	if tr := again.transcript(got); len(tr) != 1 || tr[0].Content != "hi" {
		t.Fatalf("transcript lost: %+v", tr)
	}
}

func TestTranscriptTrim(t *testing.T) {
	old := transcriptMaxBytes
	transcriptMaxBytes = 4096
	defer func() { transcriptMaxBytes = old }()
	st := newStore(t.TempDir())
	th := st.threadFor("", "")
	for i := 0; i < 200; i++ {
		st.record(th, entry{Time: time.Now(), Shell: shellClaude, Role: roleUser, Content: fmt.Sprintf("message %03d", i)})
	}
	tr := st.transcript(th)
	if len(tr) == 0 || len(tr) >= 200 || tr[len(tr)-1].Content != "message 199" {
		t.Fatalf("trim kept %d entries, last %+v", len(tr), tr[len(tr)-1])
	}
}

func TestProvidersKeepClaudeModelsForClaudeCode(t *testing.T) {
	r, cc, acp := newTestRouter(t, "")
	r.SetProviders([]core.ProviderConfig{{Name: "main", Model: "claude-opus-5-5", Models: []core.ModelOption{{Name: "claude-opus-5-5", Alias: "opus"}, {Name: "gemini-4-neon", Alias: "neon"}}}})
	r.SetActiveProvider("main")
	if cc.GetModel() != "claude-opus-5-5" {
		t.Fatalf("claude model = %q", cc.GetModel())
	}

	// What the engine does on /model gemini-4-neon with an active provider.
	updated, _ := core.SetProviderModel(r.ListProviders(), "main", "gemini-4-neon")
	r.SetProviders(updated)
	r.SetModel("gemini-4-neon")
	r.SetActiveProvider("main")
	if r.GetModel() != "gemini-4-neon" || acp.GetModel() != "gemini-4-neon" {
		t.Fatalf("router=%q acp=%q", r.GetModel(), acp.GetModel())
	}
	if cc.GetModel() != "claude-opus-5-5" {
		t.Fatalf("Claude Code must not get a Gemini model, got %q", cc.GetModel())
	}

	// The configured list is the whole menu: models the ACP agent reports
	// beyond it (gemini-3.8-flash) are not added.
	models := r.AvailableModels(context.Background())
	if len(models) != 2 || models[0].Name != "claude-opus-5-5" || models[1].Name != "gemini-4-neon" {
		t.Fatalf("models = %+v", models)
	}
	if cm := r.ConfiguredModels(); len(cm) != 2 {
		t.Fatalf("configured models = %+v", cm)
	}
	if r.SwitchCompactLine("gemini-4-neon") != 0 || r.SwitchCompactLine("claude-opus-5-5") == 0 {
		t.Fatal("only same-shell switches compact")
	}
	if r.AvailableReasoningEfforts() != nil {
		t.Fatal("no reasoning efforts while the ACP agent does not publish any")
	}
}

func TestCLIDisplayNamesAndOptions(t *testing.T) {
	r, _, _ := newTestRouter(t, "gemini-4-neon")
	if r.CLIDisplayName() != "Antigravity" {
		t.Fatalf("display name = %q", r.CLIDisplayName())
	}
	r.state = map[string]any{"cc_data_dir": "/data", "cc_project": "p"}
	snap := r.WorkspaceAgentOptions()
	if snap["cc_data_dir"] != "/data" || snap["model"] != "gemini-4-neon" || snap[shellClaude] == nil || snap[shellACP] == nil {
		t.Fatalf("snapshot = %v", snap)
	}
	if _, err := shellOpts(map[string]any{"claudecode": map[string]any{}}, shellACP); err == nil {
		t.Fatal("missing [acp] table should fail")
	}
	got, err := shellOpts(map[string]any{"work_dir": "/w", "model": "x", "acp": map[string]any{"cmd": "agy"}}, shellACP)
	if err != nil || got["work_dir"] != "/w" || got["cmd"] != "agy" || got["model"] != nil {
		t.Fatalf("shell options = %v, %v", got, err)
	}
}

func TestStripHandoff(t *testing.T) {
	h := buildHandoff("Claude Code", []entry{{Role: roleUser, Content: "a"}}, false, 1000)
	if got := stripHandoff(withHandoff(h, "real message")); got != "real message" {
		t.Fatalf("strip = %q", got)
	}
	if buildHandoff("X", nil, false, 1000) != "" {
		t.Fatal("empty handoff expected")
	}
}

// effortShell records the reasoning effort it was given.
type effortShell struct {
	levels []string
	effort string
}

func (e *effortShell) SetReasoningEffort(v string)         { e.effort = v }
func (e *effortShell) GetReasoningEffort() string          { return e.effort }
func (e *effortShell) AvailableReasoningEfforts() []string { return e.levels }

func TestReasoningEffortFollowsCurrentShell(t *testing.T) {
	ccBase := &fakeClaude{&fakeShell{name: shellClaude, window: 400_000, history: map[string][]core.HistoryEntry{}}}
	acpBase := &fakeACP{&fakeShell{name: shellACP}}
	cc := &effortClaudeShell{fakeClaude: ccBase, effortShell: &effortShell{levels: []string{"low", "high", "max"}}}
	acp := &effortACPShell{fakeACP: acpBase, effortShell: &effortShell{levels: []string{"low", "high"}}}
	r := newRouter(cc, acp, nil, "claude-opus-5-5", newStore(""))

	if got := r.AvailableReasoningEfforts(); len(got) != 3 {
		t.Fatalf("claude levels = %v", got)
	}
	r.SetReasoningEffort("low")
	if cc.effort != "low" || acp.effort != "" {
		t.Fatalf("claude=%q acp=%q", cc.effort, acp.effort)
	}

	r.SetModel("gemini-4-neon")
	if got := r.AvailableReasoningEfforts(); len(got) != 2 {
		t.Fatalf("acp levels = %v", got)
	}
	r.SetReasoningEffort("high")
	if acp.effort != "high" || cc.effort != "low" {
		t.Fatalf("each shell keeps its own value: claude=%q acp=%q", cc.effort, acp.effort)
	}
	if r.GetReasoningEffort() != "high" {
		t.Fatalf("get = %q", r.GetReasoningEffort())
	}

	r.SetModel("claude-opus-5-5")
	if r.GetReasoningEffort() != "low" {
		t.Fatalf("claude value lost: %q", r.GetReasoningEffort())
	}
	if !r.KeepsSessionOnReasoningChange() {
		t.Fatal("router must keep the conversation on a reasoning change")
	}
}

type effortClaudeShell struct {
	*fakeClaude
	*effortShell
}

type effortACPShell struct {
	*fakeACP
	*effortShell
}

// A session keeps its wrapper when it moves between shells, so /stop and
// steering must follow the shell it is on now.
func TestStopAndSteerFollowTheCurrentShell(t *testing.T) {
	r, _, _ := newT3TestRouter(t, "gemini-4-neon")
	s := start(t, r, "")
	tc, ok := s.(core.TurnCancelReporter)
	if !ok || !tc.CanCancelTurn() {
		t.Fatal("a session with no shell yet has nothing to stop and must keep itself")
	}
	s.(core.TurnOriginSetter).SetTurnHuman(false)
	turn(t, s, "scheduled task")
	if tc.CanCancelTurn() {
		t.Fatal("Claude Code turns must still stop by closing the session")
	}
	if rc, err := s.(core.AgentSessionAsyncSteerer).SteerAsync("more", "m1", nil, nil); rc == nil || err != nil {
		t.Fatalf("Claude Code steering = %v, %v", rc, err)
	}
	s.(core.TurnOriginSetter).SetTurnHuman(true)
	turn(t, s, "owner message")
	if !tc.CanCancelTurn() || s.(core.AgentSessionCanceller).CancelTurn() != nil {
		t.Fatal("the T3 shell's turn must be cancellable")
	}
	if rc, err := s.(core.AgentSessionAsyncSteerer).SteerAsync("more", "m2", nil, nil); rc != nil || err != nil {
		t.Fatalf("a shell without steering must decline: %v, %v", rc, err)
	}
}

// The ACP shell cancels a turn in place but cannot take steering, so a
// message sent mid-turn must decline and wait in the queue.
func TestStopAndSteerFollowTheCurrentShellACP(t *testing.T) {
	r, _, _ := newTestRouter(t, "gemini-4-neon")
	r.acpHumanOnly = true
	s := start(t, r, "")
	tc, ok := s.(core.TurnCancelReporter)
	if !ok || !tc.CanCancelTurn() {
		t.Fatal("a session with no shell yet has nothing to stop and must keep itself")
	}
	s.(core.TurnOriginSetter).SetTurnHuman(false)
	turn(t, s, "scheduled task")
	if tc.CanCancelTurn() {
		t.Fatal("Claude Code turns must still stop by closing the session")
	}
	s.(core.TurnOriginSetter).SetTurnHuman(true)
	turn(t, s, "owner message")
	if !tc.CanCancelTurn() || s.(core.AgentSessionCanceller).CancelTurn() != nil {
		t.Fatal("the ACP shell's turn must be cancellable")
	}
	if rc, err := s.(core.AgentSessionAsyncSteerer).SteerAsync("more", "m1", nil, nil); rc != nil || err != nil {
		t.Fatalf("a shell without steering must decline: %v, %v", rc, err)
	}
	if ok, err := s.(core.AgentSessionSteerer).Steer("more", "m2", nil, nil); ok || err != nil {
		t.Fatalf("a shell without steering must decline: %v, %v", ok, err)
	}
}
