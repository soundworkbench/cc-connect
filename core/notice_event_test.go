package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTurnWatch_AgentNoticeStaysForTheTurn(t *testing.T) {
	i := NewI18n(LangChinese)
	w := newTurnWatch(false, false)
	notice := Event{Type: EventNotice, Notice: &Notice{Key: MsgShellSwitchedCarried, Args: []any{"Antigravity"}}}
	const want = "换到 Antigravity，已带上最近对话"
	if got := w.observe(i, notice); got != want {
		t.Fatalf("observe = %q", got)
	}
	if got := w.observe(i, Event{Type: EventToolUse, ToolName: "Bash"}); got != want {
		t.Fatalf("during a tool = %q", got)
	}
	if got := w.observe(i, Event{Type: EventText, Content: "x"}); got != want {
		t.Fatalf("after text = %q", got)
	}
	if got := w.observe(i, Event{Type: EventRetry, Retry: &RetryStatus{Attempt: 2}}); got == want {
		t.Fatal("a retry notice should take the line")
	}
	if got := w.observe(i, Event{Type: EventTextDelta, Content: "y"}); got != want {
		t.Fatalf("after the retry = %q", got)
	}
	if got := w.line(i, time.Now().Add(2*waitUnit)); !strings.HasPrefix(got, i.T(MsgStillWorking)) {
		t.Fatalf("the waiting line should win once the turn waits, got %q", got)
	}
}

func TestNoticeEventShowsOnCard(t *testing.T) {
	f := newFailureEnv(t)
	f.send("1", "请求")
	f.s.events <- Event{Type: EventNotice, Notice: &Notice{Key: MsgShellSwitchedCarried, Args: []any{"Antigravity"}}}
	f.s.events <- Event{Type: EventTextDelta, Content: "好的"}
	f.s.events <- Event{Type: EventResult, Content: "好的", Done: true}
	f.waitIdle()
	notices, _ := f.p.snapshot()
	if len(notices) == 0 || notices[0] != "换到 Antigravity，已带上最近对话" {
		t.Fatalf("notices=%q", notices)
	}
	if sent := strings.Join(f.p.getSent(), "\n"); strings.Contains(sent, "Antigravity") {
		t.Fatalf("notice also sent as a message: %s", sent)
	}
}

func TestNoticeEventSentAsMessageWithoutCardLine(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	s := &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 4)}
	e := NewEngine("plain", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	const key = "plain:user"
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "1", UserID: "user", Content: "请求", ReplyCtx: "ctx"})
	select {
	case <-s.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt missing")
	}
	s.events <- Event{Type: EventNotice, Notice: &Notice{Key: MsgShellSwitchFailed, Args: []any{"Antigravity", "Claude Code", "claude-opus-5-5"}}}
	s.events <- Event{Type: EventResult, Content: "好的", Done: true}
	waitUntil(t, "idle", func() bool { return !e.sessions.GetOrCreateActive(key).Busy() })
	if sent := strings.Join(p.getSent(), "\n"); !strings.Contains(sent, "Antigravity 没能启动，已回到 Claude Code（claude-opus-5-5）") {
		t.Fatalf("notice not sent: %q", sent)
	}
}
