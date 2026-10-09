package feishu

// Opt-in smoke test. Uses only the explicitly selected bot's own existing
// allow-listed private conversation; never starts another WebSocket listener.
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type smokeAgent struct{ session *smokeSession }

func (a *smokeAgent) Name() string { return "preview-smoke" }
func (a *smokeAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	return a.session, nil
}
func (a *smokeAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) { return nil, nil }
func (a *smokeAgent) Stop() error                                                   { return nil }

type smokeSession struct{ events chan core.Event }

func (s *smokeSession) Send(string, string, []core.ImageAttachment, []core.FileAttachment) error {
	go func() {
		for _, event := range []core.Event{
			{Type: core.EventThinking, Content: "卡片修复自检：正在验证同一张卡片更新。"},
			{Type: core.EventToolUse, ToolName: "verification", ToolInput: "not executed"},
			{Type: core.EventThinking, Content: "卡片修复自检：更新完成，即将撤回。"},
			{Type: core.EventText, Content: "temporary text, must not send separately"},
			{Type: core.EventResult, Content: "卡片修复自检通过。", Done: true},
		} {
			time.Sleep(750 * time.Millisecond)
			s.events <- event
		}
	}()
	return nil
}
func (s *smokeSession) RespondPermission(string, core.PermissionResult) error { return nil }
func (s *smokeSession) Events() <-chan core.Event                             { return s.events }
func (s *smokeSession) CurrentSessionID() string                              { return "preview-smoke" }
func (s *smokeSession) Alive() bool                                           { return true }
func (s *smokeSession) Close() error                                          { return nil }

type livePreviewPlatform struct {
	*Platform
	mu             sync.Mutex
	handles        []any
	edits, recalls int
	editBodies     []string
	final          chan string
	errors         chan error
}

func (p *livePreviewPlatform) SendPreviewStart(ctx context.Context, rc any, text string) (any, error) {
	h, err := p.Platform.SendPreviewStart(ctx, rc, text)
	if err != nil {
		p.errors <- err
		return h, err
	}
	p.mu.Lock()
	p.handles = append(p.handles, h)
	p.mu.Unlock()
	return h, nil
}
func (p *livePreviewPlatform) UpdateMessage(ctx context.Context, h any, text string) error {
	p.mu.Lock()
	same := len(p.handles) == 1 && p.handles[0] == h
	p.mu.Unlock()
	if !same {
		err := fmt.Errorf("update did not reuse original handle")
		p.errors <- err
		return err
	}
	if err := p.Platform.UpdateMessage(ctx, h, text); err != nil {
		p.errors <- err
		return err
	}
	// Card 2.0 bodies are not exposed by Message.Get (it returns a fallback
	// placeholder). Verify the real message identity here, not its rendered body.
	resp, err := p.client.Im.Message.Get(ctx, larkim.NewGetMessageReqBuilder().MessageId(h.(*feishuPreviewHandle).messageID).Build())
	if err != nil || !resp.Success() {
		e := fmt.Errorf("message readback failed")
		p.errors <- e
		return e
	}
	found := false
	if resp.Data != nil {
		for _, item := range resp.Data.Items {
			if item.MessageId != nil && *item.MessageId == h.(*feishuPreviewHandle).messageID && item.Deleted != nil && !*item.Deleted {
				found = true
			}
		}
	}
	if !found {
		e := fmt.Errorf("original live message identity absent from readback")
		p.errors <- e
		return e
	}
	p.mu.Lock()
	p.edits++
	p.editBodies = append(p.editBodies, text)
	p.mu.Unlock()
	return nil
}
func (p *livePreviewPlatform) DeletePreviewMessage(ctx context.Context, h any) error {
	if err := p.Platform.DeletePreviewMessage(ctx, h); err != nil {
		p.errors <- err
		return err
	}
	resp, err := p.client.Im.Message.Get(ctx, larkim.NewGetMessageReqBuilder().MessageId(h.(*feishuPreviewHandle).messageID).Build())
	deleted := false
	if err == nil && resp.Success() && resp.Data != nil {
		for _, item := range resp.Data.Items {
			if item.MessageId != nil && *item.MessageId == h.(*feishuPreviewHandle).messageID && item.Deleted != nil && *item.Deleted {
				deleted = true
			}
		}
	}
	if !deleted {
		e := fmt.Errorf("recall not confirmed by message readback")
		p.errors <- e
		return e
	}
	p.mu.Lock()
	p.recalls++
	p.mu.Unlock()
	return nil
}

// Capture the final answer locally: this test publishes only the temporary
// card being exercised, then recalls it. No extra permanent test message.
func (p *livePreviewPlatform) Send(_ context.Context, _ any, text string) error {
	p.final <- text
	return nil
}
func (p *livePreviewPlatform) Reply(_ context.Context, _ any, text string) error {
	p.final <- text
	return nil
}

func TestLivePreviewLifecycle(t *testing.T) {
	configPath := os.Getenv("PREVIEW_SMOKE_CONFIG")
	if configPath == "" {
		t.Skip("explicit live configuration required")
	}
	envBytes, err := os.ReadFile("/proc/" + os.Getenv("PREVIEW_SMOKE_PID") + "/environ")
	if err != nil {
		t.Fatal("cannot read selected bot environment")
	}
	env := map[string]string{}
	for _, item := range strings.Split(string(envBytes), "\x00") {
		if k, v, ok := strings.Cut(item, "="); ok {
			env[k] = v
		}
	}
	var cfg struct {
		Display struct {
			Mode             string
			ThinkingMessages bool `toml:"thinking_messages"`
			ToolMessages     bool `toml:"tool_messages"`
		} `toml:"display"`
		InstantReply struct {
			Enabled bool
			Content string
		} `toml:"instant_reply"`
		Projects []struct {
			Platforms []struct{ Options map[string]any }
		}
	}
	if _, err := toml.DecodeFile(configPath, &cfg); err != nil {
		t.Fatal("cannot load selected config")
	}
	opts := cfg.Projects[0].Platforms[0].Options
	for k, v := range opts {
		if s, ok := v.(string); ok {
			opts[k] = os.Expand(s, func(k string) string { return env[k] })
		}
	}
	var sessions struct {
		Active map[string]string `json:"active_session"`
	}
	b, err := os.ReadFile(os.Getenv("PREVIEW_SMOKE_SESSIONS"))
	if err != nil {
		t.Fatal("cannot load selected sessions")
	}
	if json.Unmarshal(b, &sessions) != nil {
		t.Fatal("invalid session store")
	}
	allowed, _ := opts["allow_from"].(string)
	key := ""
	for k := range sessions.Active {
		parts := strings.Split(k, ":")
		if len(parts) == 3 && parts[0] == "feishu" && strings.HasPrefix(parts[1], "oc_") && strings.HasPrefix(parts[2], "ou_") && strings.Contains(allowed, parts[2]) {
			if key != "" {
				t.Fatal("ambiguous allow-listed conversation")
			}
			key = k
		}
	}
	if key == "" {
		t.Fatal("no explicit allow-listed conversation")
	}
	base, err := New(opts)
	if err != nil {
		t.Fatal("cannot initialize selected platform")
	}
	ip, ok := base.(*interactivePlatform)
	if !ok {
		t.Fatal("selected platform does not support interactive cards")
	}
	p := &livePreviewPlatform{Platform: ip.Platform, final: make(chan string, 4), errors: make(chan error, 8)}
	if !p.UseThinkingProcessPreview() || !p.StartPreviewImmediately() {
		t.Fatal("selected configuration does not enable compact preview")
	}
	realModel := os.Getenv("PREVIEW_SMOKE_REAL_AGENT") == "1"
	limit := 30 * time.Second
	if realModel {
		limit = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	chat, err := p.client.Im.Chat.Get(ctx, larkim.NewGetChatReqBuilder().ChatId(strings.Split(key, ":")[1]).Build())
	if err != nil || !chat.Success() || chat.Data == nil || chat.Data.ChatMode == nil || *chat.Data.ChatMode != "p2p" {
		t.Fatal("not a verified private conversation; no test message sent")
	}
	rc, err := p.ReconstructReplyCtx(key)
	if err != nil {
		t.Fatal("cannot reconstruct selected conversation")
	}
	var agent core.Agent = &smokeAgent{session: &smokeSession{events: make(chan core.Event, 8)}}
	prompt, expectedFinal := "卡片生命周期自检", "卡片修复自检通过。"
	display := core.DisplayCfg{Mode: "quiet", ThinkingMessages: true, HideAgentFooter: true}
	previewCfg := core.DefaultStreamPreviewCfg()
	if realModel {
		// Use the production loader, including expansion inside nested env lists.
		realConfig, err := config.Load(configPath)
		if err != nil {
			t.Fatal("cannot load real agent configuration")
		}
		aopts := realConfig.Projects[0].Agent.Options
		if activeModel := os.Getenv("PREVIEW_SMOKE_ACTIVE_MODEL"); activeModel != "" {
			aopts["model"] = activeModel // Preserve the selected conversation's /model override.
		}
		sp := realConfig.StreamPreview
		if sp.Enabled != nil {
			previewCfg.Enabled = *sp.Enabled
		}
		if sp.IntervalMs != nil {
			previewCfg.IntervalMs = *sp.IntervalMs
		}
		if sp.MinDeltaChars != nil {
			previewCfg.MinDeltaChars = *sp.MinDeltaChars
		}
		if sp.MaxChars != nil {
			previewCfg.MaxChars = *sp.MaxChars
		}
		previewCfg.DisabledPlatforms = sp.DisabledPlatforms
		aopts["cc_data_dir"] = t.TempDir()
		agent, err = core.CreateAgent(realConfig.Projects[0].Agent.Type, aopts)
		if err != nil {
			t.Fatal("cannot initialize real preview agent")
		}
		display = core.DisplayCfg{Mode: cfg.Display.Mode, ThinkingMessages: cfg.Display.ThinkingMessages, ToolMessages: cfg.Display.ToolMessages, HideAgentFooter: true}
		if display.ThinkingMessages {
			t.Fatal("public progress acceptance requires private thinking disabled")
		}
		prompt = "这是中文进度转发验收，只做两次只读命令，不涉及业务内容。验收要求两次工具前各发送一条用户可见的 assistant text 消息；第二条也必须是公开文本，不能在 thinking 内默念来代替。先发送：正在核对运行目录。然后用 Bash 执行 pwd。收到结果后先发送第二条公开文本：目录已核对，正在验证命令执行。然后另一次 Bash 执行 sleep 2 && printf PROGRESS_TOOL_OK，等待用于观察进度卡刷新。最后仅回复 PROGRESS_MODEL_OK。两次工具必须分开调用；不要读取文件、不要修改文件、不要访问网络、不要输出内部推理。"
		expectedFinal = "PROGRESS_MODEL_OK"
	}
	e := core.NewEngine("preview-smoke", agent, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
	e.SetStreamPreviewCfg(previewCfg)
	e.SetDisplayConfig(display)
	e.SetReplyFooterEnabled(false)
	e.SetInstantReply(core.InstantReplyCfg{Enabled: cfg.InstantReply.Enabled, Content: cfg.InstantReply.Content})
	defer func() {
		e.Stop()
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.recalls == 0 {
			for _, h := range p.handles {
				_ = p.Platform.DeletePreviewMessage(context.Background(), h)
			}
		}
	}()
	e.ReceiveMessage(p, &core.Message{SessionKey: key, Platform: "feishu", UserID: strings.Split(key, ":")[2], Content: prompt, ReplyCtx: rc})
	select {
	case err := <-p.errors:
		t.Fatalf("live card operation failed: %v", err)
	case final := <-p.final:
		if strings.TrimSpace(final) != expectedFinal {
			// Keep diagnostics useful without printing model text or credentials.
			t.Fatalf("unexpected standalone message: bytes=%d final_marker=%t error_marker=%t startup_marker=%t", len(final), strings.Contains(final, expectedFinal), strings.Contains(final, "❌"), strings.Contains(final, "启动"))
		}
	case <-ctx.Done():
		t.Fatal("live card lifecycle timed out")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if realModel {
		for i, body := range p.editBodies {
			t.Logf("progress update %d: first_marker=%t second_marker=%t", i, strings.Contains(body, "正在核对运行目录"), strings.Contains(body, "目录已核对，正在验证命令执行"))
		}
	}
	if len(p.handles) != 1 || p.edits < 2 || (!realModel && p.edits != 2) || p.recalls != 1 {
		t.Fatalf("starts=%d updates=%d recalls=%d", len(p.handles), p.edits, p.recalls)
	}
	if realModel {
		first, second := -1, -1
		for i, body := range p.editBodies {
			if first < 0 && strings.Contains(body, "正在核对运行目录") {
				first = i
			}
			if second < 0 && strings.Contains(body, "目录已核对，正在验证命令执行") {
				second = i
			}
			if strings.Contains(body, expectedFinal) {
				t.Fatal("final answer leaked into process preview")
			}
		}
		if first < 0 || second <= first {
			t.Fatal("two real Chinese progress updates were not delivered separately before final")
		}
		t.Logf("REAL_PUBLIC_PROGRESS real_model=true public_chinese_progress=2 same_card_updates=%d private_thinking=false final_captured=1 recalls=1 deleted_readback=true", p.edits)
	}
	t.Logf("real Feishu API: starts=1 same-card updates=%d identity_readbacks=%d recalls=1 deleted_readback=true clean final captured=1", p.edits, p.edits)
	if os.Getenv("PREVIEW_SMOKE_NOTIFY") == "complete" {
		if err := p.Platform.Send(ctx, rc, "✅ 卡片预览实测通过：即时提示、同卡更新、结束撤回均正常；模型请求正常。"); err != nil {
			t.Fatal("repair succeeded but completion notification failed")
		}
	}
}
