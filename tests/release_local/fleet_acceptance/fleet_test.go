package fleet_acceptance

// Opt-in real backend checks. The real engine/agent/config are used; only the
// outgoing chat boundary is captured so tests never publish model answers.
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/chenhg5/cc-connect/agent/claudecode"
	_ "github.com/chenhg5/cc-connect/agent/codex"
	_ "github.com/chenhg5/cc-connect/agent/opencode"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

type capture struct {
	mu                       sync.Mutex
	starts, updates, recalls int
	final                    chan string
}

func (p *capture) Name() string                                         { return "capture" }
func (p *capture) Start(core.MessageHandler) error                      { return nil }
func (p *capture) Stop() error                                          { return nil }
func (p *capture) Send(_ context.Context, _ any, text string) error     { p.final <- text; return nil }
func (p *capture) Reply(ctx context.Context, rc any, text string) error { return p.Send(ctx, rc, text) }
func (p *capture) ReplyCard(ctx context.Context, rc any, card *core.Card) error {
	return p.Send(ctx, rc, card.RenderText())
}
func (p *capture) SendCard(ctx context.Context, rc any, card *core.Card) error {
	return p.ReplyCard(ctx, rc, card)
}
func (p *capture) StartPreviewImmediately() bool   { return true }
func (p *capture) UseThinkingProcessPreview() bool { return true }
func (p *capture) KeepPreviewOnFinish() bool       { return false }
func (p *capture) SendPreviewStart(context.Context, any, string) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts++
	return fmt.Sprintf("probe-%d", p.starts), nil
}
func (p *capture) UpdateMessage(context.Context, any, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updates++
	return nil
}
func (p *capture) DeletePreviewMessage(context.Context, any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recalls++
	return nil
}

func safe(text string) string {
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if (strings.Contains(key, "TOKEN") || strings.Contains(key, "SECRET") || strings.Contains(key, "KEY") || strings.Contains(key, "PROXY")) && value != "" {
			text = strings.ReplaceAll(text, value, "[REDACTED]")
		}
	}
	text = regexp.MustCompile(`[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}`).ReplaceAllString(text, "[EMAIL]")
	text = regexp.MustCompile(`(?:sk-|sess-)[A-Za-z0-9_.-]+`).ReplaceAllString(text, "[REDACTED]")
	return text
}

func TestFleetLiveUsageAndTurn(t *testing.T) {
	path := os.Getenv("FLEET_CONFIG")
	if path == "" {
		t.Skip("explicit live acceptance only")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal("cannot load selected configuration")
	}
	project := cfg.Projects[0]
	opts := project.Agent.Options
	opts["cc_data_dir"] = t.TempDir()
	agent, err := core.CreateAgent(project.Agent.Type, opts)
	if err != nil {
		t.Fatalf("agent initialization: %s", safe(err.Error()))
	}
	p := &capture{final: make(chan string, 32)}
	e := core.NewEngine("fleet-live", agent, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
	e.SetStreamPreviewCfg(core.DefaultStreamPreviewCfg())
	e.SetDisplayConfig(core.DisplayCfg{Mode: "quiet", ThinkingMessages: true, HideAgentFooter: true})
	e.SetReplyFooterEnabled(false)
	instantReply := cfg.InstantReply.Enabled != nil && *cfg.InstantReply.Enabled
	e.SetInstantReply(core.InstantReplyCfg{Enabled: instantReply, Content: cfg.InstantReply.Content})
	defer e.Stop()
	usageCount := 0
	actions := []string{"/usage", "这是维护连通性检查。不要调用任何工具，不读取文件，不修改任何东西，只回复 FLEET_MODEL_OK。", "/usage"}
	if os.Getenv("FLEET_USAGE_ONLY") != "" {
		actions = []string{"/usage"}
	}
	for i, content := range actions {
		started := time.Now()
		e.ReceiveMessage(p, &core.Message{SessionKey: "capture:health", Platform: "capture", UserID: "health", MessageID: fmt.Sprint(i), Content: content, ReplyCtx: "capture"})
		select {
		case text := <-p.final:
			if content == "/usage" {
				if !strings.Contains(text, "%") || !strings.Contains(text, "重置") || strings.Contains(text, "失败") {
					t.Fatalf("usage command: %s", safe(text))
				}
				usageCount++
				if label := os.Getenv("FLEET_EXPECT_USAGE_LABEL"); label != "" && !strings.Contains(text, label) {
					t.Fatalf("real usage card lacks expected quota %q", label)
				}
				t.Logf("real /usage card elapsed=%s percentages=%v", time.Since(started).Round(time.Millisecond), regexp.MustCompile(`\d+%`).FindAllString(text, -1))
			} else if strings.TrimSpace(text) != "FLEET_MODEL_OK" {
				t.Fatalf("model final: %s", safe(text))
			}
		case <-time.After(150 * time.Second):
			t.Fatal("real request timed out")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(actions) > 1 && (p.starts != 1 || p.recalls != 1) {
		t.Fatalf("preview starts=%d recalls=%d", p.starts, p.recalls)
	}
	receipt, _ := json.Marshal(map[string]any{"agent": project.Name, "backend": project.Agent.Type, "usage_commands": usageCount, "model_request": len(actions) > 1, "preview_starts": p.starts, "preview_recalls": p.recalls, "instant_reply_enabled": instantReply, "verified_usage_label": os.Getenv("FLEET_EXPECT_USAGE_LABEL")})
	t.Log("FLEET_ACCEPTANCE", string(receipt))
}
