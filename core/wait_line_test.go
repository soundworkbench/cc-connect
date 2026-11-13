package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortWaitUnit makes a "minute" of the waiting lines last d for one test.
func shortWaitUnit(t *testing.T, d time.Duration) {
	t.Helper()
	old := waitUnit
	waitUnit = d
	t.Cleanup(func() { waitUnit = old })
}

// quickLinePlatform is a step-list card that also takes the quick line.
type quickLinePlatform struct {
	failurePlatform
	quick atomic.Int32
}

func (p *quickLinePlatform) SetPreviewQuickLine(context.Context, any, string) error {
	p.quick.Add(1)
	return nil
}

// stalledQuickLine points the quick line at a gateway that never answers, as
// the company line did on 11-13, and returns how many calls it got.
func stalledQuickLine(t *testing.T, e *Engine) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.streamPreview.QuickLine = QuickLineCfg{Enabled: true, BaseURL: srv.URL, Model: "m", KeyFile: keyFile, TimeoutMs: 30}
	return &calls
}

func noticeIndex(notices []string, want string) int { return slices.Index(notices, want) }

// 11-13: the quick line timed out and the model took minutes; the card kept
// "思考中" the whole time. Now the running row counts the minutes, says the
// model is slow from the second minute, and goes back to the step once the
// turn shows progress. A running tool gets no waiting line, and the clock
// starts again when the tool returns.
func TestTurnCardCountsTheWaitAfterTheQuickLineTimesOut(t *testing.T) {
	shortWaitUnit(t, 60*time.Millisecond)
	p := &quickLinePlatform{failurePlatform: failurePlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}}
	s := &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 4)}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	e.SetStreamPreviewCfg(cfg)
	t.Cleanup(func() { _ = e.Stop() })
	calls := stalledQuickLine(t, e)
	f := &failureEnv{t: t, p: &p.failurePlatform, s: s, e: e, key: "segmented:user"}
	e.ReceiveMessage(p, &Message{SessionKey: f.key, Platform: p.Name(), MessageID: "1", UserID: "user", Content: "整理本周纪要", ReplyCtx: "ctx"})
	select {
	case <-s.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt missing")
	}

	const (
		one   = "仍在处理 · 已等待 1 分钟"
		two   = "仍在处理 · 模型响应较慢，已等待 2 分钟"
		three = "仍在处理 · 模型响应较慢，已等待 3 分钟"
	)
	waitUntil(t, "third minute", func() bool {
		notices, _ := f.p.snapshot()
		return noticeIndex(notices, three) >= 0
	})
	notices, _ := f.p.snapshot()
	if calls.Load() == 0 || p.quick.Load() != 0 {
		t.Fatalf("quick line calls=%d written=%d, want a call that timed out", calls.Load(), p.quick.Load())
	}
	if notices[0] != one || noticeIndex(notices, two) != 1 || noticeIndex(notices, three) != 2 {
		t.Fatalf("notices=%q, want %q then %q then %q", notices, one, two, three)
	}

	// A tool call is progress: the step comes back, and no waiting line
	// shows while the tool runs.
	f.s.events <- Event{Type: EventToolUse, ToolName: "Read", ToolInput: "读取纪要"}
	waitUntil(t, "step restored", func() bool {
		notices, _ := f.p.snapshot()
		return notices[len(notices)-1] == ""
	})
	time.Sleep(3 * waitUnit)
	notices, _ = f.p.snapshot()
	if notices[len(notices)-1] != "" {
		t.Fatalf("a running tool got a waiting line: %q", notices)
	}

	// The tool returned; the model's new wait starts from one minute.
	n := len(notices)
	f.s.events <- Event{Type: EventToolResult, ToolName: "Read"}
	waitUntil(t, "new wait", func() bool {
		notices, _ := f.p.snapshot()
		return len(notices) > n
	})
	if notices, _ := f.p.snapshot(); notices[n] != one {
		t.Fatalf("after the tool: %q, want the count to start over at %q", notices[n:], one)
	}

	// Reply text is progress too.
	f.s.events <- Event{Type: EventTextDelta, Content: "本周"}
	waitUntil(t, "reply restores the row", func() bool {
		notices, _ := f.p.snapshot()
		return notices[len(notices)-1] == ""
	})
	f.s.events <- Event{Type: EventResult, Content: "本周纪要已整理", Done: true}
	f.waitIdle()
}

// Events the card does not show (hidden thinking) do not reset the clock,
// and a retry notice wins over the waiting line until the model answers.
func TestWaitingLineIgnoresHiddenEventsAndYieldsToRetries(t *testing.T) {
	shortWaitUnit(t, time.Hour)
	i := NewI18n(LangChinese)
	w := newTurnWatch(false, false)
	w.since = time.Now().Add(-2*time.Hour - time.Minute)
	if got := w.observe(i, Event{Type: EventThinking, Content: "想一想"}); got != "仍在处理 · 模型响应较慢，已等待 2 分钟" {
		t.Fatalf("hidden thinking: %q", got)
	}
	if got := w.observe(i, Event{Type: EventRetry, Retry: &RetryStatus{Attempt: 2}}); got != "模型服务连不上，正在重试（第 2 次）" {
		t.Fatalf("retry: %q", got)
	}
	if got := w.observe(i, Event{Type: EventThinking, Content: "再想"}); got != "仍在处理 · 模型响应较慢，已等待 2 分钟" {
		t.Fatalf("after retry: %q", got)
	}
	shown := newTurnWatch(false, true)
	shown.since = w.since
	if got := shown.observe(i, Event{Type: EventThinking, Content: "想一想"}); got != "" {
		t.Fatalf("thinking on the card is progress: %q", got)
	}
}

// A subagent whose step has not moved for two minutes says the model is
// slow, with the minutes it has waited, while the quick model writes no
// progress line; a new step brings the normal line back. The progress answer
// says the same.
func TestTaskCardSaysTheModelIsSlowOnAStalledStep(t *testing.T) {
	shortWaitUnit(t, 80*time.Millisecond)
	env := newTaskEnv(t)
	env.e.streamPreview.QuickLine = QuickLineCfg{Enabled: true, BaseURL: "http://127.0.0.1:1", Model: "m", KeyFile: filepath.Join(t.TempDir(), "missing"), TimeoutMs: 30}
	env.e.taskCards.summaryGap, env.e.taskCards.cliStale = 0, 0
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "整理纪要", Background: yes()})
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "读取会议纪要"})
	line := func() string {
		_, up := env.p.cards()
		if len(up) == 0 {
			return ""
		}
		return up[len(up)-1].view.Line
	}
	waitUntil(t, "slow at two", func() bool { return line() == "读取会议纪要 · 模型响应较慢，已等待 2 分钟" })

	session := env.e.sessions.GetOrCreateActive(env.key)
	answer := env.e.progressAnswer(env.e.triageView(&Message{SessionKey: env.key}, session, env.key), session)
	if !strings.HasPrefix(answer, "后台任务「整理纪要」：读取会议纪要 · 模型响应较慢，已等待 ") {
		t.Fatalf("progress answer=%q", answer)
	}

	waitUntil(t, "slow at three", func() bool { return line() == "读取会议纪要 · 模型响应较慢，已等待 3 分钟" })
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "汇总要点"})
	waitUntil(t, "normal line back", func() bool { return line() == "汇总要点 · 不到 1 分钟" })

	// The agent's own new progress line is progress as well.
	waitUntil(t, "slow again", func() bool { return strings.Contains(line(), "模型响应较慢") })
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Summary: "已读完 8 份纪要"})
	waitUntil(t, "progress line resets", func() bool { return line() == "汇总要点 · 不到 1 分钟" })
	env.s.events <- Event{Type: EventResult, Content: "已在后台整理", Done: true}
}

// A task with nothing on it yet reads "still working" in front of the wait.
// A shell command is not the model: a long one keeps its own running time.
func TestTaskWaitLineWithoutStepAndForCommands(t *testing.T) {
	shortWaitUnit(t, time.Minute)
	i := NewI18n(LangChinese)
	start := time.Now().Add(-5 * time.Minute)
	quiet := &cardTask{desc: "整理纪要", start: start, startKnown: true, movedAt: start}
	if s := taskShownLocked(i, quiet); joinTaskLine(s.text, s.time) != "仍在处理 · 模型响应较慢，已等待 5 分钟" {
		t.Fatalf("quiet task: %+v", s)
	}
	cmd := &cardTask{desc: "构建前端", start: start, startKnown: true, movedAt: start, step: "构建前端", command: true, steps: 1}
	if s := taskShownLocked(i, cmd); joinTaskLine(s.text, s.time) != "构建前端 · 5 分钟" {
		t.Fatalf("command: %+v", s)
	}
	unknown := &cardTask{desc: "查资料", step: "检索", steps: 1, movedAt: time.Now().Add(-time.Minute - time.Second)}
	if s := taskShownLocked(i, unknown); joinTaskLine(s.text, s.time) != "检索 · 已等待 1 分钟" {
		t.Fatalf("start unknown: %+v", s)
	}
}
