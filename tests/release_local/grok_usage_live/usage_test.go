package grok_usage_live

import (
	"context"
	"github.com/chenhg5/cc-connect/agent/opencode"
	"github.com/chenhg5/cc-connect/core"
	"os"
	"strings"
	"testing"
)

type capture struct{ text string }

func (p *capture) Name() string                                   { return "capture" }
func (p *capture) Start(core.MessageHandler) error                { return nil }
func (p *capture) Stop() error                                    { return nil }
func (p *capture) Reply(_ context.Context, _ any, s string) error { p.text = s; return nil }
func (p *capture) Send(_ context.Context, _ any, s string) error  { p.text = s; return nil }
func (p *capture) ReplyCard(_ context.Context, _ any, c *core.Card) error {
	p.text = c.RenderText()
	return nil
}
func (p *capture) SendCard(_ context.Context, _ any, c *core.Card) error {
	p.text = c.RenderText()
	return nil
}

func TestUsageActualCommandToBillingToCard(t *testing.T) {
	usage := os.Getenv("CC_TEST_GROK_USAGE_CMD")
	cmd := os.Getenv("CC_TEST_OPENCODE_CMD")
	if usage == "" || cmd == "" {
		t.Skip("explicit live acceptance only")
	}
	agent, err := opencode.New(map[string]any{"cmd": cmd, "usage_cmd": usage, "model": "xai/grok-4.6", "work_dir": "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	p := &capture{}
	e := core.NewEngine("usage-verification", agent, []core.Platform{p}, t.TempDir()+"/sessions.json", core.LangChinese)
	defer e.Stop()
	e.ReceiveMessage(p, &core.Message{SessionKey: "capture:test", Platform: "capture", UserID: "test", Content: "/usage", ReplyCtx: "test"})
	if !strings.Contains(p.text, "%") || !strings.Contains(p.text, "重置") || strings.Contains(p.text, "未能读取") || strings.Contains(p.text, "累计 token") {
		t.Fatalf("invalid quota card: %s", p.text)
	}
	t.Log(p.text)
}
