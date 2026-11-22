package modelrouter

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type plainPlatform struct {
	mu   sync.Mutex
	sent []string
}

func (p *plainPlatform) Name() string                    { return "plain" }
func (p *plainPlatform) Start(core.MessageHandler) error { return nil }
func (p *plainPlatform) Stop() error                     { return nil }
func (p *plainPlatform) Reply(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	p.sent = append(p.sent, content)
	p.mu.Unlock()
	return nil
}
func (p *plainPlatform) Send(ctx context.Context, rc any, content string) error {
	return p.Reply(ctx, rc, content)
}
func (p *plainPlatform) messages() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.sent, "\n")
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal(what + " timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fakeShell) allPrompts() []string {
	f.mu.Lock()
	sessions := append([]*fakeSession(nil), f.sessions...)
	f.mu.Unlock()
	var out []string
	for _, s := range sessions {
		out = append(out, s.prompts()...)
	}
	return out
}

// TestEngineModelSwitchMovesShell drives the real engine: /model to a Gemini
// model moves the conversation to the ACP shell with the handoff, and back.
func TestEngineModelSwitchMovesShell(t *testing.T) {
	r, cc, acp := newTestRouter(t, "claude-opus-5-5")
	p := &plainPlatform{}
	e := core.NewEngine("router", r, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	const key = "plain:user"
	msg := func(id, text string) {
		e.ReceiveMessage(p, &core.Message{SessionKey: key, Platform: "plain", MessageID: id, UserID: "user", Content: text, ReplyCtx: "ctx"})
	}

	msg("1", "记住暗号是蓝鲸")
	waitFor(t, "claude answers", func() bool { return strings.Contains(p.messages(), "claudecode answers") })

	msg("2", "/model gemini-4-neon")
	waitFor(t, "model switched", func() bool { return r.GetModel() == "gemini-4-neon" })
	msg("3", "暗号是什么")
	waitFor(t, "acp answers", func() bool { return strings.Contains(p.messages(), "acp answers") })
	prompts := acp.allPrompts()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "记住暗号是蓝鲸") || !strings.Contains(prompts[0], "<handoff") {
		t.Fatalf("acp prompts = %q", prompts)
	}
	if !strings.Contains(p.messages(), "换到 Antigravity，已带上最近对话") {
		t.Fatalf("switch notice missing:\n%s", p.messages())
	}

	msg("4", "/model claude-opus-5-5")
	waitFor(t, "model back", func() bool { return r.GetModel() == "claude-opus-5-5" })
	msg("5", "再说一遍")
	waitFor(t, "claude answers again", func() bool { return len(cc.allPrompts()) == 2 })
	back := cc.allPrompts()[1]
	if !strings.Contains(back, "暗号是什么") || strings.Contains(back, "记住暗号是蓝鲸") {
		t.Fatalf("claude should get only the Antigravity part:\n%s", back)
	}
	starts := cc.startedWith()
	if len(starts) != 2 || starts[1] != "claudecode-1" {
		t.Fatalf("claude should resume its own session, starts = %q", starts)
	}
}
