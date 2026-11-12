package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// reactPlatform records emoji reactions.
type reactPlatform struct {
	cujSegmentedPlatform
	rmu       sync.Mutex
	reactions []string
}

func (p *reactPlatform) AddReaction(_ context.Context, _ any, emoji string) error {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	p.reactions = append(p.reactions, emoji)
	return nil
}

func (p *reactPlatform) SendTaskCard(context.Context, any, TaskCardView) (any, error) {
	return "task-card", nil
}

func (p *reactPlatform) UpdateTaskCard(context.Context, any, TaskCardView) error { return nil }

func (p *reactPlatform) reacted() []string {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	return append([]string(nil), p.reactions...)
}

type triageEnv struct {
	t        *testing.T
	p        *reactPlatform
	s        *queuedLifecycleSession
	e        *Engine
	key      string
	label    atomic.Value // what the quick model answers
	rewrite  atomic.Value // what the quick model answers to a Chinese rewrite; unset = gateway error
	rewrites atomic.Int32
	fail     atomic.Bool
	delay    atomic.Int64 // how long the quick model takes, in ns
	calls    atomic.Int32
	n        int
}

func newTriageEnv(t *testing.T) *triageEnv {
	t.Helper()
	env := &triageEnv{t: t, key: "segmented:user"}
	env.label.Store("OTHER")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System string `json:"system"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.System == outboundChinesePrompt {
			env.rewrites.Add(1)
			if v, ok := env.rewrite.Load().(string); ok {
				fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, v)
				return
			}
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if body.System != triagePrompt {
			fmt.Fprint(w, `{"content":[{"type":"text","text":"收到"}]}`) // the turn's quick line
			return
		}
		env.calls.Add(1)
		time.Sleep(time.Duration(env.delay.Load()))
		if env.fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, env.label.Load().(string))
	}))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.p = &reactPlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}
	env.s = &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 8)}
	env.e = NewEngine("segmented", &controllableAgent{nextSession: env.s}, []Platform{env.p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	cfg.QuickLine = QuickLineCfg{Enabled: true, BaseURL: srv.URL, Model: "m", KeyFile: keyFile, TimeoutMs: 5000}
	env.e.SetStreamPreviewCfg(cfg)
	t.Cleanup(func() { _ = env.e.Stop() })
	return env
}

func (env *triageEnv) send(content string) {
	env.n++
	env.e.ReceiveMessage(env.p, &Message{SessionKey: env.key, Platform: env.p.Name(), MessageID: fmt.Sprint(env.n), UserID: "user", Content: content, ReplyCtx: "ctx"})
}

// reachesAgent reports whether the last message was written to the agent.
func (env *triageEnv) reachesAgent() bool {
	select {
	case <-env.s.sent:
		return true
	case <-time.After(500 * time.Millisecond):
		return false
	}
}

func (env *triageEnv) finishTurn(reply string) {
	env.t.Helper()
	env.s.events <- Event{Type: EventResult, Content: reply, Done: true}
	deadline := time.Now().Add(3 * time.Second)
	for env.e.sessions.GetOrCreateActive(env.key).Busy() {
		if time.Now().After(deadline) {
			env.t.Fatal("turn hung")
		}
		time.Sleep(time.Millisecond)
	}
}

// firstTurn runs one ordinary turn, so the session and its state exist.
func (env *triageEnv) firstTurn(reply string) {
	env.t.Helper()
	env.send("先整理一下文件")
	if !env.reachesAgent() {
		env.t.Fatal("first message did not reach the agent")
	}
	env.finishTurn(reply)
}

// backgroundTask starts a background task card for the session.
func (env *triageEnv) backgroundTask(id, desc, step string) *interactiveState {
	env.t.Helper()
	env.e.interactiveMu.Lock()
	state := env.e.interactiveStates[env.key]
	env.e.interactiveMu.Unlock()
	b := true
	env.e.handleTaskEvent(state, env.key, &TaskEvent{Kind: TaskStarted, TaskID: id, Description: desc, Background: &b})
	env.e.handleTaskEvent(state, env.key, &TaskEvent{Kind: TaskProgress, TaskID: id, Step: step})
	return state
}

// A fully idle chat never waits on the quick model.
func TestIdleChatSkipsTheQuickModel(t *testing.T) {
	env := newTriageEnv(t)
	env.firstTurn("整理好了。")
	env.label.Store("GREETING")
	calls := env.calls.Load()
	for _, content := range []string{"查一下日志", "谢谢", "好了吗"} {
		env.send(content)
		if !env.reachesAgent() {
			t.Fatalf("%q in an idle chat did not reach the agent", content)
		}
		env.finishTurn("好")
	}
	if env.calls.Load() != calls || len(env.p.reacted()) != 0 {
		t.Fatalf("idle chat was triaged: calls=%d reactions=%v", env.calls.Load()-calls, env.p.reacted())
	}
}

func TestThanksDuringABackgroundTaskOnlyGetsAnEmoji(t *testing.T) {
	env := newTriageEnv(t)
	env.firstTurn("已在后台整理。")
	env.backgroundTask("t1", "按客户整理 200 个文件", "读取文件")
	env.label.Store("GREETING")
	before := len(env.p.stubPlatformEngine.getSent())
	env.send("谢谢")
	if env.reachesAgent() {
		t.Fatal("thanks woke the agent")
	}
	if r := env.p.reacted(); len(r) != 1 || r[0] != triageReaction {
		t.Fatalf("reactions=%v", r)
	}
	if after := len(env.p.stubPlatformEngine.getSent()); after != before {
		t.Fatalf("a card or message was sent: %v", env.p.stubPlatformEngine.getSent()[before:])
	}
	// The next real request goes to the agent as usual.
	env.label.Store("OTHER")
	env.send("再把客户名单导出来")
	if !env.reachesAgent() {
		t.Fatal("request after thanks did not reach the agent")
	}
	env.finishTurn("好的")
}

// "好的" / "可以" / "谢谢" may answer a question; with one pending they always
// reach the agent, even when the quick model calls them small talk.
func TestShortAnswersToAPendingQuestionAreNeverSwallowed(t *testing.T) {
	env := newTriageEnv(t)
	env.firstTurn("做成表格还是文档？")
	env.backgroundTask("t1", "按客户整理 200 个文件", "读取文件")
	env.label.Store("GREETING")
	for _, answer := range []string{"好的", "可以", "谢谢", "嗯"} {
		env.send(answer)
		if !env.reachesAgent() {
			t.Fatalf("%q after a question was swallowed", answer)
		}
		env.finishTurn("那就做表格，要不要加图表？")
	}
	if len(env.p.reacted()) != 0 {
		t.Fatalf("reactions=%v", env.p.reacted())
	}

	// A pending decision card is a pending question too.
	store, err := NewDecisionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.e.SetDecisionStore(store, 0)
	env.send("好")
	if !env.reachesAgent() {
		t.Fatal("setup turn missing")
	}
	env.finishTurn("我发了一张卡片给你选。")
	if err := store.Add(&Decision{ID: "d1", Project: env.e.name, SessionKey: env.key, Question: "用哪个方案", Options: []string{"甲", "乙"}, State: decisionPending, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	env.send("可以")
	if !env.reachesAgent() {
		t.Fatal("answer with a pending decision card was swallowed")
	}
	env.finishTurn("好")
	if len(env.p.reacted()) != 0 {
		t.Fatalf("triaged with a pending decision card: reactions=%v", env.p.reacted())
	}
}

// A progress question never answers a card or a question, so old unanswered
// cards do not send it to the agent.
func TestProgressQuestionAnsweredDespitePendingCards(t *testing.T) {
	env := newTriageEnv(t)
	env.firstTurn("已在后台整理，要不要顺便出个汇总？")
	env.backgroundTask("t1", "按客户整理 200 个文件", "读取文件")
	store, err := NewDecisionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.e.SetDecisionStore(store, 0)
	old := time.Now().Add(-3 * time.Hour)
	if err := store.Add(&Decision{ID: "d1", Project: env.e.name, SessionKey: env.key, Question: "用哪个方案", Options: []string{"甲", "乙"}, State: decisionPending, CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(&Decision{ID: "f1", Project: env.e.name, SessionKey: env.key, Form: &FormSpec{Fields: []FormField{{Label: "日期", Type: "date"}}}, State: decisionPending, CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	env.label.Store("PROGRESS")
	for _, q := range []string{"进度怎么样", "好了吗", "在忙什么"} {
		before := len(env.p.stubPlatformEngine.getSent())
		env.send(q)
		if env.reachesAgent() {
			t.Fatalf("%q with pending cards woke the agent", q)
		}
		sent := env.p.stubPlatformEngine.getSent()
		if len(sent) != before+1 || !strings.Contains(sent[before], "后台任务「按客户整理 200 个文件」：") {
			t.Fatalf("%q: sent=%q", q, sent[before:])
		}
	}
	// A thank-you may still be the answer: it goes to the agent.
	env.label.Store("GREETING")
	env.send("谢谢")
	if !env.reachesAgent() {
		t.Fatal("thanks with pending cards was swallowed")
	}
	env.finishTurn("不客气")
	if len(env.p.reacted()) != 0 {
		t.Fatalf("reactions=%v", env.p.reacted())
	}
}

func TestProgressQuestionAnsweredFromBridgeState(t *testing.T) {
	env := newTriageEnv(t)
	env.firstTurn("已在后台整理。")
	state := env.backgroundTask("t1", "按客户整理 200 个文件", "读取文件")
	env.label.Store("PROGRESS")
	env.send("好了吗")
	if env.reachesAgent() {
		t.Fatal("progress question woke the agent")
	}
	sent := env.p.stubPlatformEngine.getSent()
	answer := sent[len(sent)-1]
	if !strings.Contains(answer, "后台任务「按客户整理 200 个文件」：读取文件 · 不到 1 分钟。") || !strings.Contains(answer, "看上面的任务卡") {
		t.Fatalf("answer=%q", answer)
	}
	h := env.e.sessions.GetOrCreateActive(env.key).GetHistory(2)
	if len(h) != 2 || h[0].Role != "user" || h[0].Content != "好了吗" || h[1].Role != "assistant" || h[1].Content != answer {
		t.Fatalf("history=%+v", h)
	}
	// With nothing running the same question goes straight to the agent.
	env.e.abandonTasks(state.agentSession)
	time.Sleep(50 * time.Millisecond)
	calls := env.calls.Load()
	env.send("在忙什么")
	if !env.reachesAgent() {
		t.Fatal("progress question with nothing running was swallowed")
	}
	if env.calls.Load() != calls {
		t.Fatal("the quick model was asked with nothing running")
	}
	env.finishTurn("没在忙")
}

func TestClassifierFailureGoesToTheAgent(t *testing.T) {
	env := newTriageEnv(t)
	env.firstTurn("已在后台整理。")
	env.backgroundTask("t1", "按客户整理 200 个文件", "读取文件")
	env.fail.Store(true)
	env.send("谢谢")
	if !env.reachesAgent() {
		t.Fatal("a failed classification swallowed the message")
	}
	env.finishTurn("不客气")
}

func TestSlowClassifierGivesUpAfterTheCap(t *testing.T) {
	env := newTriageEnv(t)
	env.firstTurn("已在后台整理。")
	env.backgroundTask("t1", "按客户整理 200 个文件", "读取文件")
	env.label.Store("GREETING")
	env.delay.Store(int64(5 * time.Second))
	start := time.Now()
	env.send("谢谢")
	select {
	case <-env.s.sent:
	case <-time.After(4 * time.Second):
		t.Fatal("a slow classification held the message")
	}
	if waited := time.Since(start); waited < triageMaxTimeout {
		t.Fatalf("gave up after %v, before the %v cap", waited, triageMaxTimeout)
	}
	if len(env.p.reacted()) != 0 {
		t.Fatalf("reactions=%v", env.p.reacted())
	}
	env.finishTurn("不客气")
}
func TestNoTriageWhileATurnRuns(t *testing.T) {
	env := newTriageEnv(t)
	env.send("写一份周报")
	if !env.reachesAgent() {
		t.Fatal("first message missing")
	}
	env.label.Store("GREETING")
	calls := env.calls.Load()
	env.send("谢谢")
	time.Sleep(200 * time.Millisecond)
	if env.calls.Load() != calls || len(env.p.reacted()) != 0 {
		t.Fatalf("triaged during a turn: calls=%d reactions=%v", env.calls.Load()-calls, env.p.reacted())
	}
	// The queued message starts the next turn at once, so the session never
	// goes idle in between.
	env.s.events <- Event{Type: EventResult, Content: "周报写好了。", Done: true}
	if !env.reachesAgent() {
		t.Fatal("the message sent during the turn was dropped")
	}
	env.finishTurn("不客气")
}

func TestIsProgressAsk(t *testing.T) {
	for text, want := range map[string]bool{
		"更新进度":     true,
		"更新":       true,
		"更新一下":     true,
		"好了吗":      true,
		"好了吗？":     true,
		"构建什么进度":   true,
		"现在怎么样了":   true,
		"到哪了":      true,
		"加速":       true,
		"加速吧！":     true,
		"进度怎么样":    true,
		"更新一下文档":   false,
		"更新README": false,
		"谢谢":       false,
		"查一下日志":    false,
		"进度怎么样了，顺便把客户名单导出来发我": false,
	} {
		if got := isProgressAsk(text); got != want {
			t.Errorf("isProgressAsk(%q) = %v, want %v", text, got, want)
		}
	}
}

// Jason's own wording is answered at once, without waiting on a slow quick model.
func TestProgressKeywordsSkipTheQuickModel(t *testing.T) {
	env := newTriageEnv(t)
	env.firstTurn("已在后台整理。")
	env.backgroundTask("t1", "按客户整理 200 个文件", "读取八份纪要正文")
	env.label.Store("OTHER")
	env.delay.Store(int64(3 * time.Second)) // past the classifier's 2.5s cap
	calls := env.calls.Load()
	for _, q := range []string{"更新进度", "更新", "好了吗", "加速"} {
		before := len(env.p.stubPlatformEngine.getSent())
		start := time.Now()
		env.send(q)
		if took := time.Since(start); took > time.Second {
			t.Fatalf("%q waited %v", q, took)
		}
		if env.reachesAgent() {
			t.Fatalf("%q woke the agent", q)
		}
		sent := env.p.stubPlatformEngine.getSent()
		if len(sent) != before+1 || !strings.Contains(sent[before], "：读取八份纪要正文 · 不到 1 分钟。") {
			t.Fatalf("%q: sent=%q", q, sent[before:])
		}
	}
	if env.calls.Load() != calls {
		t.Fatalf("quick model asked %d times", env.calls.Load()-calls)
	}
}

// The answer names the step in the agent's own words, never a tool's generic label.
func TestProgressAnswerNamesTheStepInTheAgentsWords(t *testing.T) {
	env := newTriageEnv(t)
	env.firstTurn("我先在后台跑构建，再核对产物。")
	env.e.interactiveMu.Lock()
	state := env.e.interactiveStates[env.key]
	env.e.interactiveMu.Unlock()
	b := true
	env.e.handleTaskEvent(state, env.key, &TaskEvent{Kind: TaskStarted, TaskID: "b1", Description: "构建候选二进制", Background: &b, Step: "运行命令", StepGeneric: true})
	ask := func() string {
		before := len(env.p.stubPlatformEngine.getSent())
		env.send("进度")
		if env.reachesAgent() {
			t.Fatal("progress question woke the agent")
		}
		return env.p.stubPlatformEngine.getSent()[before]
	}
	for range 2 { // the bridge's own answer in the history is not the agent's words
		if got := ask(); strings.Contains(got, "运行命令") || !strings.Contains(got, "：我先在后台跑构建，再核对产物 · ") {
			t.Fatalf("no own step: %q", got)
		}
	}
	env.e.handleTaskEvent(state, env.key, &TaskEvent{Kind: TaskProgress, TaskID: "b1", Summary: "正在编译飞书平台包"})
	if got := ask(); !strings.Contains(got, "：正在编译飞书平台包 · ") {
		t.Fatalf("progress line not used: %q", got)
	}
	env.e.handleTaskEvent(state, env.key, &TaskEvent{Kind: TaskProgress, TaskID: "b1", Step: "打包二进制"})
	if got := ask(); !strings.Contains(got, "：打包二进制 · ") {
		t.Fatalf("own step not used: %q", got)
	}
}

func TestProgressAnswerLeavesOutAnUnknownStep(t *testing.T) {
	e := NewEngine("p", &stubAgent{}, nil, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	got := e.progressAnswer(triageView{tasks: []triageTask{{title: "构建", elapsed: "不到 1 分钟"}}}, nil)
	if !strings.HasPrefix(got, "后台任务「构建」进行中 · 不到 1 分钟。") || strings.Contains(got, "：") {
		t.Fatalf("got %q", got)
	}
}
