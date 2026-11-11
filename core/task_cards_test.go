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
	"testing"
	"time"
)

type taskCardCall struct {
	handle any
	view   TaskCardView
}

// taskCardPlatform records task cards; each SendTaskCard returns a new handle.
type taskCardPlatform struct {
	failurePlatform
	tmu      sync.Mutex
	sent     []taskCardCall
	updates  []taskCardCall
	onUpdate func()
}

func (p *taskCardPlatform) SendTaskCard(_ context.Context, _ any, v TaskCardView) (any, error) {
	p.tmu.Lock()
	defer p.tmu.Unlock()
	h := fmt.Sprintf("task-card-%d", len(p.sent))
	p.sent = append(p.sent, taskCardCall{h, v})
	return h, nil
}

func (p *taskCardPlatform) UpdateTaskCard(_ context.Context, h any, v TaskCardView) error {
	p.tmu.Lock()
	defer p.tmu.Unlock()
	p.updates = append(p.updates, taskCardCall{h, v})
	if p.onUpdate != nil {
		p.onUpdate()
	}
	return nil
}

func (p *taskCardPlatform) cards() (sent, updates []taskCardCall) {
	p.tmu.Lock()
	defer p.tmu.Unlock()
	return append([]taskCardCall(nil), p.sent...), append([]taskCardCall(nil), p.updates...)
}

type stoppableSession struct {
	*queuedLifecycleSession
	mu      sync.Mutex
	stopped []string
}

func (s *stoppableSession) StopTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = append(s.stopped, id)
	return nil
}

type taskEnv struct {
	t   *testing.T
	p   *taskCardPlatform
	s   *stoppableSession
	e   *Engine
	key string
}

func newTaskEnv(t *testing.T) *taskEnv {
	t.Helper()
	p := &taskCardPlatform{failurePlatform: failurePlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}}
	s := &stoppableSession{queuedLifecycleSession: &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 4)}}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	e.SetStreamPreviewCfg(cfg)
	e.taskCards.openDelay = 60 * time.Millisecond
	e.taskCards.minGap = 20 * time.Millisecond
	e.taskCards.tick = 10 * time.Millisecond
	t.Cleanup(func() { _ = e.Stop() })
	env := &taskEnv{t: t, p: p, s: s, e: e, key: "segmented:user"}
	e.ReceiveMessage(p, &Message{SessionKey: env.key, Platform: p.Name(), MessageID: "1", UserID: "user", Content: "整理文件", ReplyCtx: "ctx"})
	select {
	case <-s.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt missing")
	}
	return env
}

func (env *taskEnv) task(ev TaskEvent) {
	env.s.events <- Event{Type: EventTask, Task: &ev}
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

func TestBackgroundTaskCardOpensUpdatesAndSettles(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "t1", Description: "按客户整理 200 个文件", Background: yes()})
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "t1", Step: "读取文件", Summary: "正在读第 3 个客户的文件"})
	// The foreground turn ends; the task keeps running.
	env.s.events <- Event{Type: EventResult, Content: "已在后台整理", Done: true}
	waitUntil(t, "task card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	sent, _ := env.p.cards()
	v := sent[0].view
	if v.State != TaskCardRunning || v.Title != "后台任务 · 正在进行：按客户整理 200 个文件" || v.Status != "读取文件" || v.Elapsed != "已跑不到 1 分钟" ||
		v.Recent != "最近进展：正在读第 3 个客户的文件" || v.Hint != "在后台进行，不影响聊天，你可以接着跟我说话；做完我会告诉你。要停的话直接跟我说。" || v.SessionKey != env.key {
		t.Fatalf("open view=%+v", v)
	}
	// Between turns the unsolicited reader carries the task events.
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "t1", Step: "编辑文件"})
	waitUntil(t, "step update", func() bool {
		_, up := env.p.cards()
		return len(up) > 0 && up[len(up)-1].view.Status == "编辑文件"
	})
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "t1", Status: "completed", Summary: "整理完成\n共 200 个文件\n分到 12 个客户\n多余的一行"})
	waitUntil(t, "settled card", func() bool {
		_, up := env.p.cards()
		return len(up) > 0 && up[len(up)-1].view.State == TaskCardCompleted
	})
	_, up := env.p.cards()
	last := up[len(up)-1]
	if last.handle != sent[0].handle || last.view.Status != "后台任务 · ✓ 已完成：按客户整理 200 个文件" || last.view.Summary != "整理完成\n共 200 个文件\n分到 12 个客户" ||
		last.view.Hint != "" || strings.Join(last.view.Details, "|") != "读取文件|编辑文件" {
		t.Fatalf("settled=%+v", last)
	}
	time.Sleep(100 * time.Millisecond)
	if _, again := env.p.cards(); len(again) != len(up) {
		t.Fatalf("settled card updated again: %+v", again[len(up):])
	}
}

func TestShortAndForegroundTasksGetNoCard(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "quick", Description: "很快的命令", Background: yes()})
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "quick", Status: "completed"})
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "fg", Description: "前台子任务", Background: no()})
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "amb", Description: "后台看守", Background: yes(), Hidden: true})
	time.Sleep(300 * time.Millisecond)
	if sent, _ := env.p.cards(); len(sent) != 0 {
		t.Fatalf("unexpected cards: %+v", sent)
	}
	// A foreground task moved to the background gets its card.
	env.task(TaskEvent{Kind: TaskUpdated, TaskID: "fg", Background: yes()})
	waitUntil(t, "moved task card", func() bool {
		sent, _ := env.p.cards()
		return len(sent) == 1 && sent[0].view.Title == "后台任务 · 正在进行：前台子任务"
	})
}

// Each card follows its own task; a task stopped through the agent settles
// only its card. No card offers a stop button.
func TestTaskCardsSettleOneByOne(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "任务甲", Background: yes()})
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "b", Description: "任务乙", Background: yes()})
	waitUntil(t, "two cards", func() bool { sent, _ := env.p.cards(); return len(sent) == 2 })
	sent, _ := env.p.cards()
	handles := map[string]any{}
	for _, c := range sent {
		handles[c.view.Title] = c.handle
	}
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "b", Status: "stopped"})
	waitUntil(t, "stopped card", func() bool {
		_, up := env.p.cards()
		return len(up) > 0 && up[len(up)-1].view.Status == "后台任务 · 已停止：任务乙"
	})
	_, up := env.p.cards()
	if up[len(up)-1].handle != handles["后台任务 · 正在进行：任务乙"] {
		t.Fatalf("stopped task settled another card: %+v", up[len(up)-1])
	}
	env.e.taskCards.mu.Lock()
	open := len(env.e.taskCards.cards)
	env.e.taskCards.mu.Unlock()
	if open != 1 {
		t.Fatalf("open cards=%d, want task a still running", open)
	}
	env.s.mu.Lock()
	defer env.s.mu.Unlock()
	if len(env.s.stopped) != 0 {
		t.Fatalf("a card stopped a task: %v", env.s.stopped)
	}
}

func TestTaskCardsEndWithTheirSession(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "任务甲", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.e.cleanupInteractiveState(env.key)
	waitUntil(t, "interrupted card", func() bool {
		_, up := env.p.cards()
		return len(up) > 0 && up[len(up)-1].view.State == TaskCardInterrupted && up[len(up)-1].view.Status == "后台任务 · 已中断：任务甲"
	})
}

func TestTaskCardProgressLineFromQuickModel(t *testing.T) {
	var prompts []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		prompts = append(prompts, body.Messages[0].Content)
		mu.Unlock()
		fmt.Fprint(w, `{"content":[{"type":"text","text":"正在把文件按客户分组"}]}`)
	}))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := newTaskEnv(t)
	env.e.streamPreview.QuickLine = QuickLineCfg{Enabled: true, BaseURL: srv.URL, Model: "m", KeyFile: keyFile, TimeoutMs: 2000}
	env.e.taskCards.summaryGap = 0
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "整理文件", Background: yes()})
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "读取文件"})
	waitUntil(t, "quick progress line", func() bool {
		_, up := env.p.cards()
		return len(up) > 0 && up[len(up)-1].view.Recent == "最近进展：正在把文件按客户分组"
	})
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(prompts[0], "整理文件") || !strings.Contains(prompts[0], "读取文件") {
		t.Fatalf("prompt=%q", prompts[0])
	}
}

func waitInterrupted(t *testing.T, env *taskEnv, title string) {
	t.Helper()
	waitUntil(t, "interrupted card", func() bool {
		_, up := env.p.cards()
		return len(up) > 0 && up[len(up)-1].view.State == TaskCardInterrupted && up[len(up)-1].view.Status == "后台任务 · 已中断："+title
	})
	_, up := env.p.cards()
	if up[len(up)-1].view.Hint != "" {
		t.Fatalf("interrupted card keeps the running hint: %+v", up[len(up)-1].view)
	}
	env.e.taskCards.mu.Lock()
	defer env.e.taskCards.mu.Unlock()
	if len(env.e.taskCards.cards) != 0 {
		t.Fatalf("interrupted card still holds a slot: %d", len(env.e.taskCards.cards))
	}
}

func TestStopCommandSettlesTaskCards(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "任务甲", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.e.ReceiveMessage(env.p, &Message{SessionKey: env.key, Platform: env.p.Name(), MessageID: "stop", UserID: "user", Content: "/stop", ReplyCtx: "ctx"})
	// The person asked for the stop: the card says stopped, not interrupted.
	waitUntil(t, "stopped card", func() bool {
		_, up := env.p.cards()
		return len(up) > 0 && up[len(up)-1].view.State == TaskCardStopped && up[len(up)-1].view.Status == "后台任务 · 已停止：任务甲"
	})
}

func TestIdleReclaimSettlesTaskCards(t *testing.T) {
	env := newTaskEnv(t)
	env.e.SetAgentSessionIdleTimeout(300 * time.Millisecond)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "任务甲", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.s.events <- Event{Type: EventResult, Content: "已在后台做", Done: true}
	waitInterrupted(t, env, "任务甲")
	if !env.s.closed.Get() {
		t.Fatal("idle reclaim did not close the agent session")
	}
}

// quickTaskServer answers every quick-model call with reply and records when
// each call came.
func quickTaskServer(t *testing.T, env *taskEnv, reply func(n int) string) func() []time.Time {
	t.Helper()
	var mu sync.Mutex
	var calls []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, time.Now())
		n := len(calls)
		mu.Unlock()
		b, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": reply(n)}}})
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.e.streamPreview.QuickLine = QuickLineCfg{Enabled: true, BaseURL: srv.URL, Model: "m", KeyFile: keyFile, TimeoutMs: 2000}
	return func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Time(nil), calls...)
	}
}

func lastRecent(env *taskEnv) string {
	_, up := env.p.cards()
	if len(up) == 0 {
		return ""
	}
	return up[len(up)-1].view.Recent
}

// The agent's progress line holds while it is fresh; once it is older than
// cliStale the quick model may write one, at most every summaryGap. In the
// 11-11 test one line stayed for two minutes because the agent's line, once
// seen, blocked the quick model for good.
func TestTaskCardQuickModelTakesOverStaleAgentLine(t *testing.T) {
	env := newTaskEnv(t)
	calls := quickTaskServer(t, env, func(n int) string {
		// A reply that says its sentence twice is shown once.
		return fmt.Sprintf("正在整理第%d批纪要正在整理第%d批纪要", n, n)
	})
	env.e.taskCards.cliStale = 300 * time.Millisecond
	env.e.taskCards.summaryGap = 400 * time.Millisecond
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "整理纪要", Background: yes()})
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "搜索会议", Summary: "在找本周的会议"})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "读取纪要"})
	time.Sleep(100 * time.Millisecond)
	if n := len(calls()); n != 0 {
		t.Fatalf("quick model called %d times while the agent's line was fresh", n)
	}
	time.Sleep(300 * time.Millisecond)
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "比对纪要"})
	waitUntil(t, "quick line over the stale agent line", func() bool { return lastRecent(env) == "最近进展：正在整理第1批纪要" })
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "写清单"})
	time.Sleep(100 * time.Millisecond)
	if n := len(calls()); n != 1 {
		t.Fatalf("quick model called %d times within summaryGap", n)
	}
	time.Sleep(400 * time.Millisecond)
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "核对负责人"})
	waitUntil(t, "second quick line", func() bool { return lastRecent(env) == "最近进展：正在整理第2批纪要" })
	if c := calls(); c[1].Sub(c[0]) < 400*time.Millisecond {
		t.Fatalf("two quick lines %v apart, want at least summaryGap", c[1].Sub(c[0]))
	}
	// A fresh agent line takes the slot back.
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Summary: "在核对负责人"})
	waitUntil(t, "agent line back", func() bool { return lastRecent(env) == "最近进展：在核对负责人" })
}

func TestCollapseRepeat(t *testing.T) {
	for in, want := range map[string]string{
		"正在运行命令搜索本周的视频会议正在运行命令搜索本周的视频会议": "正在运行命令搜索本周的视频会议",
		"正在读纪要。正在读纪要。正在读纪要。":             "正在读纪要。",
		"正在读第 3 份纪要 正在读第 3 份纪要":          "正在读第 3 份纪要",
		"正在比对两份纪要":                       "正在比对两份纪要",
		"哈哈哈哈":                           "哈哈哈哈",
		"读取纪要，读取纪要附件":                    "读取纪要，读取纪要附件",
	} {
		if got := collapseRepeat(in); got != want {
			t.Errorf("collapseRepeat(%q)=%q, want %q", in, got, want)
		}
	}
}

// A task that adds no step (a subagent writing, not calling tools) still
// changes its card about every idleBeat, never faster than minGap. In the 11-11
// test a card stayed the same for 47 s.
func TestIdleTaskCardShowsItIsAlive(t *testing.T) {
	env := newTaskEnv(t)
	env.e.taskCards.idleBeat = 150 * time.Millisecond
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "整理纪要", Background: yes(), Step: "写清单"})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	waitUntil(t, "three idle beats", func() bool { _, up := env.p.cards(); return len(up) >= 3 })
	sent, up := env.p.cards()
	if up[0].view.Status != "仍在进行：写清单" || up[1].view.Status != "写清单" || up[2].view.Status != "仍在进行：写清单" {
		t.Fatalf("idle statuses: %q %q %q", up[0].view.Status, up[1].view.Status, up[2].view.Status)
	}
	prev := sent[0].view
	for _, u := range up {
		if sameTaskView(u.view, prev) {
			t.Fatalf("idle update changed nothing: %+v", u.view)
		}
		prev = u.view
	}
	// A new step shows as it is.
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "核对负责人"})
	waitUntil(t, "new step", func() bool {
		_, up := env.p.cards()
		return up[len(up)-1].view.Status == "核对负责人"
	})
}

// Updates of a busy card stay at least minGap apart, idle beats included.
func TestTaskCardUpdatesKeepMinGap(t *testing.T) {
	env := newTaskEnv(t)
	env.e.taskCards.minGap = 80 * time.Millisecond
	env.e.taskCards.idleBeat = 100 * time.Millisecond
	var mu sync.Mutex
	var at []time.Time
	env.p.onUpdate = func() { mu.Lock(); at = append(at, time.Now()); mu.Unlock() }
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "整理纪要", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	for i := 0; i < 20; i++ {
		env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: fmt.Sprintf("第 %d 步", i)})
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(at) < 3 {
		t.Fatalf("updates=%d", len(at))
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < 70*time.Millisecond {
			t.Fatalf("updates %d and %d %v apart, want at least minGap", i-1, i, gap)
		}
	}
}

// When the card is idle the quick model may write a line even without a new
// step.
func TestIdleTaskCardAsksTheQuickModel(t *testing.T) {
	env := newTaskEnv(t)
	calls := quickTaskServer(t, env, func(n int) string { return fmt.Sprintf("正在写第%d部分", n) })
	env.e.taskCards.idleBeat = 150 * time.Millisecond
	env.e.taskCards.summaryGap = 0
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "写报告", Background: yes()})
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "写报告正文"})
	waitUntil(t, "first quick line", func() bool { return lastRecent(env) == "最近进展：正在写第1部分" })
	waitUntil(t, "idle quick line", func() bool { return lastRecent(env) == "最近进展：正在写第2部分" })
	if n := len(calls()); n < 2 {
		t.Fatalf("quick calls=%d", n)
	}
}

// settledView ends the foreground turn, so the engine stops at once, and
// returns the task card's settled view.
func settledView(t *testing.T, env *taskEnv) TaskCardView {
	t.Helper()
	env.s.events <- Event{Type: EventResult, Content: "好了", Done: true}
	waitUntil(t, "settled card", func() bool {
		_, up := env.p.cards()
		return len(up) > 0 && !up[len(up)-1].view.State.Live()
	})
	_, up := env.p.cards()
	return up[len(up)-1].view
}

// A failed task's card carries a short Chinese line; the CLI's English error
// goes to the details panel as written.
func TestFailedTaskCardShowsAChineseLine(t *testing.T) {
	for _, tc := range []struct{ summary, line string }{
		{"Background command \"清理临时目录\" failed with exit code 2", "命令出错结束（退出码 2），原始信息在“详情”里。"},
		{"Agent \"整理纪要\" failed: API Error: 500", "出错结束，原始信息在“详情”里。"},
		{"没找到这一周的纪要", "没找到这一周的纪要"},
	} {
		env := newTaskEnv(t)
		env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "清理", Background: yes()})
		waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
		env.task(TaskEvent{Kind: TaskEnded, TaskID: "a", Status: "failed", Summary: tc.summary})
		v := settledView(t, env)
		if v.State != TaskCardFailed || v.Summary != tc.line {
			t.Fatalf("%q: state=%s summary=%q, want %q", tc.summary, v.State, v.Summary, tc.line)
		}
		if tc.line == tc.summary {
			if v.Full != "" {
				t.Fatalf("readable summary also copied to details: %+v", v)
			}
		} else if v.FullTitle != "原文" || v.Full != tc.summary {
			t.Fatalf("%q: details full=%q title=%q", tc.summary, v.Full, v.FullTitle)
		}
	}
}

// A stopped task's card is grey and drops the CLI's English text.
func TestStoppedTaskCardHidesTheRawText(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "搜索会议", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a", Status: "stopped", Summary: "Background command \"搜索会议\" failed with exit code 144"})
	v := settledView(t, env)
	if v.State != TaskCardStopped || v.Status != "后台任务 · 已停止：搜索会议" || v.Summary != "" || !strings.Contains(v.Full, "exit code 144") {
		t.Fatalf("stopped=%+v", v)
	}
}
