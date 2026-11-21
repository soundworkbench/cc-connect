package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// switchCtxAgent is a model-switching agent whose conversation carries a
// configurable context size. The carried size belongs to the conversation
// (Claude Code's transcript), not to one process, so it lives on the agent.
type switchCtxAgent struct {
	*defaultModelAgent
	lines map[string]int

	ctxMu         sync.Mutex
	carried       int
	carriedModel  string
	compactedOn   []string
	failCompactOn string
	procs         []*switchCtxSession
}

func newSwitchCtxAgent(carried int, carriedModel string) *switchCtxAgent {
	return &switchCtxAgent{
		defaultModelAgent: newDefaultModelAgent(),
		lines:             map[string]int{"alpha": 300_000, "beta": 100_000},
		carried:           carried,
		carriedModel:      carriedModel,
	}
}

func (a *switchCtxAgent) SwitchCompactLine(model string) int { return a.lines[model] }
func (a *switchCtxAgent) CompressCommand() string            { return "/compact" }

func (a *switchCtxAgent) StartSession(ctx context.Context, id string) (AgentSession, error) {
	s, err := a.defaultModelAgent.StartSession(ctx, id)
	if err != nil {
		return nil, err
	}
	proc := &switchCtxSession{cujAgentSession: s.(*cujAgentSession), agent: a, model: a.GetModel()}
	a.ctxMu.Lock()
	a.procs = append(a.procs, proc)
	a.ctxMu.Unlock()
	return proc, nil
}

func (a *switchCtxAgent) snapshot() (compactedOn []string, procs []*switchCtxSession) {
	a.ctxMu.Lock()
	defer a.ctxMu.Unlock()
	return append([]string(nil), a.compactedOn...), append([]*switchCtxSession(nil), a.procs...)
}

type switchCtxSession struct {
	*cujAgentSession
	agent *switchCtxAgent
	model string
}

func (s *switchCtxSession) CarriedContext() (int, string) {
	s.agent.ctxMu.Lock()
	defer s.agent.ctxMu.Unlock()
	return s.agent.carried, s.agent.carriedModel
}

func (s *switchCtxSession) Send(prompt, msgID string, images []ImageAttachment, files []FileAttachment) error {
	if prompt != "/compact" {
		s.agent.ctxMu.Lock()
		s.agent.carriedModel = s.model
		s.agent.ctxMu.Unlock()
		return s.cujAgentSession.Send(prompt, msgID, images, files)
	}
	s.mu.Lock()
	s.sentPrompts = append(s.sentPrompts, prompt)
	s.mu.Unlock()
	s.agent.ctxMu.Lock()
	s.agent.compactedOn = append(s.agent.compactedOn, s.model)
	fail := s.agent.failCompactOn == s.model
	if !fail {
		s.agent.carried, s.agent.carriedModel = 20_000, ""
	}
	s.agent.ctxMu.Unlock()
	go func() {
		s.events <- Event{Type: EventCompacting}
		if fail {
			s.events <- Event{Type: EventError, Error: errors.New("503 no available accounts")}
			return
		}
		s.events <- Event{Type: EventResult}
	}()
	return nil
}

func (s *switchCtxSession) prompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sentPrompts...)
}

type switchCtxEnv struct {
	*cujEnv
	agent *switchCtxAgent
	p     *stubPlatformEngine
	e     *Engine
}

const switchKey = "test:chat:user"

func newSwitchCtxEnv(t *testing.T, agent *switchCtxAgent) *switchCtxEnv {
	t.Helper()
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	return &switchCtxEnv{cujEnv: &cujEnv{t: t, engine: e, plat: p}, agent: agent, p: p, e: e}
}

// turn sends content and returns everything the platform got until the reply
// that names a model arrives.
func (env *switchCtxEnv) turn(content string) string {
	env.t.Helper()
	env.p.clearSent()
	env.e.ReceiveMessage(env.p, &Message{SessionKey: switchKey, Platform: "test", UserID: "user", Content: content, ReplyCtx: switchKey})
	env.waitFor("reply to "+content, 3*time.Second, func() bool { return strings.Contains(strings.Join(env.p.getSent(), "\n"), "MODEL=") })
	env.waitFor("idle", 3*time.Second, func() bool { return !env.e.sessions.GetOrCreateActive(switchKey).Busy() })
	return strings.Join(env.p.getSent(), "\n")
}

func (env *switchCtxEnv) command(content string) {
	env.t.Helper()
	env.p.clearSent()
	env.e.ReceiveMessage(env.p, &Message{SessionKey: switchKey, Platform: "test", UserID: "user", Content: content, ReplyCtx: switchKey})
	env.waitFor(content, 3*time.Second, func() bool { return len(env.p.getSent()) > 0 })
}

var switchNoticeEN = "Long conversation, tidying it up before switching models"

func TestModelSwitch_CompactsOnPreviousModel(t *testing.T) {
	agent := newSwitchCtxAgent(150_000, "alpha")
	env := newSwitchCtxEnv(t, agent)

	if got := env.turn("hello"); !strings.Contains(got, "MODEL=alpha") {
		t.Fatal(got)
	}
	env.command("/model switch beta")
	got := env.turn("next")
	if !strings.Contains(got, "MODEL=beta") {
		t.Fatalf("next turn: %s", got)
	}
	if !strings.Contains(got, switchNoticeEN) {
		t.Fatalf("no switch notice: %s", got)
	}
	compacted, procs := agent.snapshot()
	if strings.Join(compacted, ",") != "alpha" {
		t.Fatalf("compacted on %v, want the previous model", compacted)
	}
	if len(procs) != 2 || !procs[0].closed.Get() {
		t.Fatalf("old process should be recycled after compacting; procs=%d", len(procs))
	}
	if p := procs[1].prompts(); len(p) != 1 || p[0] == "/compact" {
		t.Fatalf("new process prompts = %q, want only the user's message", p)
	}
}

func TestModelSwitch_SmallContextSwitchesWithoutCompaction(t *testing.T) {
	agent := newSwitchCtxAgent(50_000, "alpha")
	env := newSwitchCtxEnv(t, agent)

	env.turn("hello")
	env.command("/model switch beta")
	got := env.turn("next")
	if !strings.Contains(got, "MODEL=beta") || strings.Contains(got, switchNoticeEN) {
		t.Fatalf("next turn: %s", got)
	}
	if compacted, _ := agent.snapshot(); len(compacted) != 0 {
		t.Fatalf("compacted on %v, want none", compacted)
	}
}

func TestModelSwitch_CompactsOnNewModelWithoutLiveProcess(t *testing.T) {
	// The conversation was left by alpha and no process is running (bridge
	// restart, idle close, /stop): compaction happens on beta, before the
	// user's message.
	agent := newSwitchCtxAgent(150_000, "alpha")
	env := newSwitchCtxEnv(t, agent)

	env.command("/model switch beta")
	got := env.turn("next")
	if !strings.Contains(got, "MODEL=beta") || !strings.Contains(got, switchNoticeEN) {
		t.Fatalf("next turn: %s", got)
	}
	compacted, procs := agent.snapshot()
	if strings.Join(compacted, ",") != "beta" {
		t.Fatalf("compacted on %v, want the new model", compacted)
	}
	if p := procs[len(procs)-1].prompts(); len(p) != 2 || p[0] != "/compact" {
		t.Fatalf("prompts = %q, want /compact then the message", p)
	}

	// Once compacted, later turns go straight through.
	if got := env.turn("again"); strings.Contains(got, switchNoticeEN) {
		t.Fatalf("second turn compacted again: %s", got)
	}
	if compacted, _ := agent.snapshot(); len(compacted) != 1 {
		t.Fatalf("compactions = %v", compacted)
	}
}

func TestModelSwitch_FailedCompactionOnPreviousModelFallsBackToNew(t *testing.T) {
	agent := newSwitchCtxAgent(150_000, "alpha")
	agent.failCompactOn = "alpha"
	env := newSwitchCtxEnv(t, agent)

	env.turn("hello")
	env.command("/model switch beta")
	if got := env.turn("next"); !strings.Contains(got, "MODEL=beta") {
		t.Fatalf("next turn: %s", got)
	}
	if compacted, _ := agent.snapshot(); strings.Join(compacted, ",") != "alpha,beta" {
		t.Fatalf("compacted on %v, want alpha then beta", compacted)
	}
}

func TestModelSwitch_FailedCompactionIsNotRepeatedEveryTurn(t *testing.T) {
	agent := newSwitchCtxAgent(150_000, "alpha")
	agent.failCompactOn = "beta"
	env := newSwitchCtxEnv(t, agent)

	env.command("/model switch beta")
	env.turn("next")
	env.turn("again")
	if compacted, _ := agent.snapshot(); strings.Join(compacted, ",") != "beta" {
		t.Fatalf("compacted on %v, want a single attempt", compacted)
	}
}

func TestModelSwitch_SameModelResumeDoesNotCompact(t *testing.T) {
	agent := newSwitchCtxAgent(150_000, "beta")
	agent.SetProviders([]ProviderConfig{{Name: "wp", Model: "beta"}})
	env := newSwitchCtxEnv(t, agent)

	if got := env.turn("hello"); !strings.Contains(got, "MODEL=beta") || strings.Contains(got, switchNoticeEN) {
		t.Fatalf("turn: %s", got)
	}
	if compacted, _ := agent.snapshot(); len(compacted) != 0 {
		t.Fatalf("compacted on %v, want none", compacted)
	}
}

func TestModelSwitch_DefaultModelAPICompactsOnPreviousModel(t *testing.T) {
	agent := newSwitchCtxAgent(150_000, "alpha")
	env := newSwitchCtxEnv(t, agent)

	env.turn("hello")
	if _, err := env.e.SetDefaultModel("beta"); err != nil {
		t.Fatal(err)
	}
	if got := env.turn("next"); !strings.Contains(got, "MODEL=beta") || !strings.Contains(got, switchNoticeEN) {
		t.Fatalf("next turn: %s", got)
	}
	if compacted, _ := agent.snapshot(); strings.Join(compacted, ",") != "alpha" {
		t.Fatalf("compacted on %v", compacted)
	}
}

func TestModelsDiffer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"gemini-4-neon", "claude-opus-5-5", true},
		{"claude-opus-5-5", "claude/claude-opus-5-5", false},
		{"claude-opus-5-5", "claude-opus-5-5[1m]", false},
		{"opus", "claude-opus-5-5", false},
		{"codex/gpt-6.1-sol", "codex/gpt-6-astra", true},
		{"", "claude-opus-5-5", false},
	} {
		if got := modelsDiffer(tc.a, tc.b); got != tc.want {
			t.Errorf("modelsDiffer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestTurnWatch_CompactingNotice(t *testing.T) {
	i := NewI18n(LangChinese)
	w := newTurnWatch(false)
	if got := w.observe(i, Event{Type: EventCompacting}); got != "对话较长，正在整理" {
		t.Fatalf("observe = %q", got)
	}
	if got := w.silent(i); got != "对话较长，正在整理" {
		t.Fatalf("silent while compacting = %q", got)
	}
	if got := w.observe(i, Event{Type: EventText, Content: "x"}); got != "" {
		t.Fatalf("after compaction = %q", got)
	}
	if got := w.silent(i); got != i.T(MsgPreviewWaitingModel) {
		t.Fatalf("silent after compaction = %q", got)
	}
}

func TestCompactingEventShowsNoticeOnCard(t *testing.T) {
	f := newFailureEnv(t)
	f.send("1", "请求")
	f.s.events <- Event{Type: EventCompacting}
	f.s.events <- Event{Type: EventTextDelta, Content: "好的"}
	f.s.events <- Event{Type: EventResult, Content: "好的", Done: true}
	f.waitIdle()
	notices, _ := f.p.snapshot()
	if want := []string{"对话较长，正在整理", ""}; strings.Join(notices, "|") != strings.Join(want, "|") {
		t.Fatalf("notices=%q want %q", notices, want)
	}
}

func TestModelSwitch_NoticeShowsOnCard(t *testing.T) {
	agent := newSwitchCtxAgent(150_000, "alpha")
	p := &failurePlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}
	e := NewEngine("segmented", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	e.SetStreamPreviewCfg(cfg)
	t.Cleanup(func() { _ = e.Stop() })
	const key = "segmented:user"
	idle := func() bool { return !e.sessions.GetOrCreateActive(key).Busy() }

	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "m1", UserID: "user", Content: "/model switch beta", ReplyCtx: "ctx"})
	waitUntil(t, "model switched", func() bool { return agent.GetModel() == "beta" })
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "m2", UserID: "user", Content: "继续", ReplyCtx: "ctx"})
	waitUntil(t, "turn on beta", func() bool {
		_, procs := agent.snapshot()
		return len(procs) == 1 && len(procs[0].prompts()) == 2
	})
	waitUntil(t, "idle", idle)

	notices, _ := p.snapshot()
	if len(notices) == 0 || notices[0] != "对话较长，正在整理后切换" {
		t.Fatalf("notices=%q, want the switch notice first", notices)
	}
	if notices[len(notices)-1] != "" {
		t.Fatalf("notices=%q, want the notice cleared once compaction ends", notices)
	}
	if sent := strings.Join(p.getSent(), "\n"); strings.Contains(sent, "对话较长") {
		t.Fatalf("notice also sent as a message: %s", sent)
	}
}

func TestModelSwitch_DuringTurnQueuedMessageRunsOnNewModel(t *testing.T) {
	// /model arrives while a turn runs and the next message queues behind it:
	// the running turn finishes on the old model, the queued one runs on the
	// new model after compacting on the old one, and nothing is dropped.
	agent := newSwitchCtxAgent(150_000, "alpha")
	agent.delayMs = 300
	env := newSwitchCtxEnv(t, agent)

	env.p.clearSent()
	env.e.ReceiveMessage(env.p, &Message{SessionKey: switchKey, Platform: "test", UserID: "user", Content: "long task", ReplyCtx: switchKey})
	env.waitFor("turn started", 2*time.Second, func() bool { _, procs := agent.snapshot(); return len(procs) == 1 })
	env.e.ReceiveMessage(env.p, &Message{SessionKey: switchKey, Platform: "test", UserID: "user", Content: "/model switch beta", ReplyCtx: switchKey})
	env.waitFor("model switched", 2*time.Second, func() bool { return agent.GetModel() == "beta" })
	env.e.ReceiveMessage(env.p, &Message{SessionKey: switchKey, Platform: "test", UserID: "user", Content: "queued question", ReplyCtx: switchKey})

	env.waitFor("both replies", 5*time.Second, func() bool {
		got := strings.Join(env.p.getSent(), "\n")
		return strings.Contains(got, "MODEL=alpha") && strings.Contains(got, "MODEL=beta")
	})
	env.waitFor("idle", 3*time.Second, func() bool { return !env.e.sessions.GetOrCreateActive(switchKey).Busy() })
	got := strings.Join(env.p.getSent(), "\n")
	if strings.Index(got, "MODEL=alpha") > strings.Index(got, "MODEL=beta") {
		t.Fatalf("running turn should answer on alpha first: %s", got)
	}
	if !strings.Contains(got, switchNoticeEN) {
		t.Fatalf("no switch notice: %s", got)
	}
	compacted, procs := agent.snapshot()
	if strings.Join(compacted, ",") != "alpha" {
		t.Fatalf("compacted on %v, want the previous model", compacted)
	}
	if len(procs) != 2 || !procs[0].closed.Get() {
		t.Fatalf("procs=%d, want the alpha process recycled", len(procs))
	}
	if p := procs[1].prompts(); len(p) != 1 || !strings.Contains(p[0], "queued question") {
		t.Fatalf("beta prompts = %q", p)
	}
}
