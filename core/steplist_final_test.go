package core

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// stepListPlatform is a segmented platform that recalls first and does not keep
// the card, like Feishu with keep_preview_on_finish = false. It records the rows
// the engine hands to the card and the messages that reach the chat.
type stepListPlatform struct {
	cujSegmentedPlatform
	mu2        sync.Mutex
	tools      []string
	progress   []string
	recallErr  error
	recallSeen int
}

func (p *stepListPlatform) FinalReplyOrder() FinalReplyOrder { return FinalReplyRecallFirst }
func (p *stepListPlatform) KeepPreviewOnFinish() bool        { return false }
func (p *stepListPlatform) AddPreviewTool(_ context.Context, _ any, text string) error {
	p.mu2.Lock()
	defer p.mu2.Unlock()
	p.tools = append(p.tools, text)
	return nil
}
func (p *stepListPlatform) AddPreviewProgress(_ context.Context, _ any, text string) error {
	p.mu2.Lock()
	defer p.mu2.Unlock()
	p.progress = append(p.progress, text)
	return nil
}
func (p *stepListPlatform) DeletePreviewMessage(ctx context.Context, id any) error {
	p.mu2.Lock()
	p.recallSeen++
	err := p.recallErr
	p.mu2.Unlock()
	if err != nil {
		return err
	}
	return p.cujSegmentedPlatform.DeletePreviewMessage(ctx, id)
}

func runStepListEngineTurn(t *testing.T, p *stepListPlatform, events ...Event) {
	t.Helper()
	s := &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 1)}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	e.SetStreamPreviewCfg(cfg)
	t.Cleanup(func() { _ = e.Stop() })
	key := "segmented:user"
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "1", UserID: "user", Content: "请求", ReplyCtx: "ctx"})
	select {
	case <-s.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt missing")
	}
	for _, ev := range events {
		s.events <- ev
	}
	deadline := time.Now().Add(3 * time.Second)
	for e.sessions.GetOrCreateActive(key).Busy() {
		if time.Now().After(deadline) {
			t.Fatal("turn hung")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStepListToolRowUsesStepLabel(t *testing.T) {
	p := &stepListPlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine{n: "segmented"}}}
	runStepListEngineTurn(t, p,
		Event{Type: EventToolUse, ToolName: "Bash", ProgressID: "a", Metadata: map[string]any{"public_tool_progress": "运行命令", "step_label": "  清理测试对话 "}},
		Event{Type: EventToolUse, ToolName: "Bash", ProgressID: "b", Metadata: map[string]any{"public_tool_progress": "运行命令"}},
		Event{Type: EventResult, Content: "answer", Done: true},
	)
	p.mu2.Lock()
	defer p.mu2.Unlock()
	if len(p.tools) != 2 || p.tools[0] != "清理测试对话" || p.tools[1] != "运行命令" {
		t.Fatalf("tool rows %q: want the description first, the tool wording when there is none", p.tools)
	}
}

func TestStepListToolTurnRecallsCardAndRepliesOnce(t *testing.T) {
	p := &stepListPlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine{n: "segmented"}}}
	runStepListEngineTurn(t, p,
		Event{Type: EventToolUse, ToolName: "Bash", ProgressID: "a", Metadata: map[string]any{"public_tool_progress": "运行命令"}},
		Event{Type: EventToolResult, ToolName: "Bash"},
		Event{Type: EventTextDelta, Content: "answer"},
		Event{Type: EventResult, Content: "answer", Done: true},
	)
	all := p.stubPlatformEngine.getSent()
	if len(all) != 2 || all[0] != "" || all[1] != "answer" || p.recallSeen != 1 {
		t.Fatalf("want one recall and one new reply, got %q (recalls %d)", all, p.recallSeen)
	}
}

// When the platform cannot take the card down, the card becomes the reply.
func TestStepListFailedRecallSettlesCardInPlace(t *testing.T) {
	p := &stepListPlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine{n: "segmented"}}, recallErr: errors.New("still present")}
	runStepListEngineTurn(t, p,
		Event{Type: EventToolUse, ToolName: "Bash", ProgressID: "a", Metadata: map[string]any{"public_tool_progress": "运行命令"}},
		Event{Type: EventResult, Content: "answer", Done: true},
	)
	all := p.stubPlatformEngine.getSent()
	if len(all) != 1 || all[0] != "answer" {
		t.Fatalf("want the card settled as the only message, got %q", all)
	}
}
