package core

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// failurePlatform records what the live card is told while a turn waits or fails.
type failurePlatform struct {
	cujSegmentedPlatform
	fmu      sync.Mutex
	notices  []string
	failures []FailureCard
}

func (p *failurePlatform) SetPreviewNotice(_ context.Context, _ any, text string) error {
	p.fmu.Lock()
	defer p.fmu.Unlock()
	p.notices = append(p.notices, text)
	return nil
}

func (p *failurePlatform) FailPreview(_ context.Context, _ any, c FailureCard) error {
	p.fmu.Lock()
	defer p.fmu.Unlock()
	p.failures = append(p.failures, c)
	return nil
}

func (p *failurePlatform) snapshot() ([]string, []FailureCard) {
	p.fmu.Lock()
	defer p.fmu.Unlock()
	return append([]string(nil), p.notices...), append([]FailureCard(nil), p.failures...)
}

type failureEnv struct {
	t   *testing.T
	p   *failurePlatform
	s   *queuedLifecycleSession
	e   *Engine
	key string
}

func newFailureEnv(t *testing.T) *failureEnv {
	t.Helper()
	p := &failurePlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}
	s := &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 4)}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	e.SetStreamPreviewCfg(cfg)
	t.Cleanup(func() { _ = e.Stop() })
	return &failureEnv{t: t, p: p, s: s, e: e, key: "segmented:user"}
}

func (f *failureEnv) send(id, content string) {
	f.t.Helper()
	f.e.ReceiveMessage(f.p, &Message{SessionKey: f.key, Platform: f.p.Name(), MessageID: id, UserID: "user", Content: content, ReplyCtx: "ctx"})
	select {
	case got := <-f.s.sent:
		if !strings.Contains(got, content) {
			f.t.Fatalf("prompt %q does not carry %q", got, content)
		}
	case <-time.After(3 * time.Second):
		f.t.Fatal("prompt missing")
	}
}

func (f *failureEnv) waitIdle() {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.e.sessions.GetOrCreateActive(f.key).Busy() {
		if time.Now().After(deadline) {
			f.t.Fatal("turn hung")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitUntil(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal(what + " timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRetryEventShowsNoticeUntilTheModelAnswers(t *testing.T) {
	f := newFailureEnv(t)
	f.send("1", "请求")
	f.s.events <- Event{Type: EventRetry, Retry: &RetryStatus{Attempt: 2, HTTPStatus: 529, Kind: "overloaded"}}
	f.s.events <- Event{Type: EventRetry, Retry: &RetryStatus{Attempt: 3, HTTPStatus: 429, Kind: "rate_limit"}}
	f.s.events <- Event{Type: EventTextDelta, Content: "好的"}
	f.s.events <- Event{Type: EventResult, Content: "好的", Done: true}
	f.waitIdle()
	notices, failures := f.p.snapshot()
	want := []string{"模型服务连不上，正在重试（第 2 次）", "模型用量到了上限，正在等待", ""}
	if strings.Join(notices, "|") != strings.Join(want, "|") {
		t.Fatalf("notices=%q want %q", notices, want)
	}
	if len(failures) != 0 {
		t.Fatalf("a recovered turn must not fail: %+v", failures)
	}
}

func TestSilentModelGetsWaitingNoticeButRunningToolDoesNot(t *testing.T) {
	old := silenceNoticeAfter
	silenceNoticeAfter = 40 * time.Millisecond
	t.Cleanup(func() { silenceNoticeAfter = old })
	f := newFailureEnv(t)
	f.send("1", "请求")
	waitUntil(t, "waiting notice", func() bool {
		notices, _ := f.p.snapshot()
		return len(notices) > 0 && notices[0] == "还在等模型响应"
	})
	f.s.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "运行命令"}
	time.Sleep(10 * silenceNoticeAfter)
	notices, _ := f.p.snapshot()
	if len(notices) != 2 || notices[1] != "" {
		t.Fatalf("a running tool is not a silent model: %q", notices)
	}
	f.s.events <- Event{Type: EventResult, Content: "done", Done: true}
	f.waitIdle()
}

func TestModelFailureSettlesCardWithRetryThatResendsOnce(t *testing.T) {
	f := newFailureEnv(t)
	f.send("1", "整理客户名单")
	f.s.events <- Event{Type: EventError, Error: &ModelFailure{HTTPStatus: 529, Kind: "overloaded"}}
	f.waitIdle()
	_, failures := f.p.snapshot()
	if len(failures) != 1 {
		t.Fatalf("failures=%+v", failures)
	}
	c := failures[0]
	if c.Title != "这次没做成：模型服务暂时连不上" || c.ButtonLabel != "重试" || !strings.HasPrefix(c.Action, "act:/retry ") || c.SessionKey != f.key {
		t.Fatalf("failure card=%+v", c)
	}
	for _, text := range f.p.getSent() {
		if strings.Contains(text, "错误") || strings.Contains(text, "Error") {
			t.Fatalf("failure also sent as an error message: %q", f.p.getSent())
		}
	}

	if card := f.e.handleCardNav(c.Action, f.key); card != nil {
		t.Fatalf("retry must not replace the card through the callback: %+v", card)
	}
	select {
	case got := <-f.s.sent:
		if !strings.Contains(got, "整理客户名单") {
			t.Fatalf("retry prompt=%q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retry did not resend the request")
	}
	_, failures = f.p.snapshot()
	if len(failures) != 2 || failures[1].Note != "已重新发送" || failures[1].Action != "" {
		t.Fatalf("clicked card=%+v", failures)
	}
	f.s.events <- Event{Type: EventResult, Content: "完成", Done: true}
	f.waitIdle()

	if card := f.e.handleCardNav(c.Action, f.key); card == nil || card.Toast != "这个按钮已失效，请重新发送消息" || len(card.Elements) != 0 {
		t.Fatalf("a used button must answer with a notice: %+v", card)
	}
	select {
	case got := <-f.s.sent:
		t.Fatalf("a second click resent %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// After a restart the bridge has no record of the button's request.
func TestRetryButtonFromBeforeRestartAnswersWithNotice(t *testing.T) {
	f := newFailureEnv(t)
	card := f.e.handleCardNav("act:/retry 0123456789abcdef0123456789abcdef", f.key)
	if card == nil || card.Toast != "这个按钮已失效，请重新发送消息" {
		t.Fatalf("card=%+v", card)
	}
}

func TestFailureTitles(t *testing.T) {
	e := NewEngine("t", &controllableAgent{}, nil, filepath.Join(t.TempDir(), "s.json"), LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	cases := []struct {
		err         error
		title, note string
	}{
		{&ModelFailure{HTTPStatus: 429, Kind: "rate_limit"}, "这次没做成：模型用量到了上限", ""},
		{&ModelFailure{Kind: "unknown"}, "这次没做成：模型服务暂时连不上", ""},
		{&ModelFailure{HTTPStatus: 400, Kind: "invalid_request", Detail: "prompt too long"}, "这次没做成", "prompt too long"},
		{errAgentExited, "这次没做成", "agent process exited"},
	}
	for _, c := range cases {
		title, note := e.failureTitle(c.err, c.err.Error())
		if title != c.title || note != c.note {
			t.Errorf("%v: got %q/%q want %q/%q", c.err, title, note, c.title, c.note)
		}
	}
}
