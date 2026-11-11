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

func runStepListEngineTurn(t *testing.T, p Platform, events ...Event) {
	t.Helper()
	runStepListEngineTurnCfg(t, p, nil, events...)
}

func runStepListEngineTurnCfg(t *testing.T, p Platform, mod func(*StreamPreviewCfg), events ...Event) {
	t.Helper()
	s := &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 1)}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	if mod != nil {
		mod(&cfg)
	}
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

// catchUpPlatform records the order of catch-up and recall.
type catchUpPlatform struct {
	stepListPlatform
	order []string
	final string
}

func (p *catchUpPlatform) CatchUpPreview(_ context.Context, _ any, final string) {
	p.mu2.Lock()
	defer p.mu2.Unlock()
	p.order = append(p.order, "catchup")
	p.final = final
}

func (p *catchUpPlatform) DeletePreviewMessage(ctx context.Context, id any) error {
	p.mu2.Lock()
	p.order = append(p.order, "recall")
	p.mu2.Unlock()
	return p.stepListPlatform.DeletePreviewMessage(ctx, id)
}

// With keep_preview_on_finish off, the card catches up to the whole reply, then is
// recalled, then the reply is sent.
func TestStepListCatchesUpBeforeRecall(t *testing.T) {
	p := &catchUpPlatform{stepListPlatform: stepListPlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine{n: "segmented"}}}}
	runStepListEngineTurn(t, p,
		Event{Type: EventToolUse, ToolName: "Bash", ProgressID: "a", Metadata: map[string]any{"public_tool_progress": "运行命令"}},
		Event{Type: EventTextDelta, Content: "ans"},
		Event{Type: EventResult, Content: "answer", Done: true},
	)
	p.mu2.Lock()
	defer p.mu2.Unlock()
	if len(p.order) != 2 || p.order[0] != "catchup" || p.order[1] != "recall" || p.final != "answer" {
		t.Fatalf("order %v final %q, want catchup then recall with the full reply", p.order, p.final)
	}
	if all := p.stubPlatformEngine.getSent(); len(all) != 2 || all[1] != "answer" {
		t.Fatalf("sent %q", all)
	}
}

// settlePlatform records how a turn ends in settle mode.
type settlePlatform struct {
	stepListPlatform
	finished  []string
	collapsed int
	delay     time.Duration
	// sentAtCollapse is how many messages had been sent when the card collapsed.
	sentAtCollapse int
}

func (p *settlePlatform) AddPreviewTool(ctx context.Context, id any, text string) error {
	time.Sleep(p.delay)
	return p.stepListPlatform.AddPreviewTool(ctx, id, text)
}

func (p *settlePlatform) FinishSegmentedPreview(ctx context.Context, id any, text string) error {
	p.mu2.Lock()
	p.finished = append(p.finished, text)
	p.mu2.Unlock()
	return p.stepListPlatform.FinishSegmentedPreview(ctx, id, text)
}

func (p *settlePlatform) CollapsePreview(context.Context, any) error {
	sent := len(p.stubPlatformEngine.getSent())
	p.mu2.Lock()
	p.sentAtCollapse = sent
	defer p.mu2.Unlock()
	p.collapsed++
	return nil
}

func settleTurn(t *testing.T, delay time.Duration, notifyAfterMs int) *settlePlatform {
	p := &settlePlatform{stepListPlatform: stepListPlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine{n: "segmented"}}}, delay: delay}
	runStepListEngineTurnCfg(t, p, func(c *StreamPreviewCfg) { c.EndMode, c.NotifyAfterMs = "settle", notifyAfterMs },
		Event{Type: EventToolUse, ToolName: "Bash", ProgressID: "a", Metadata: map[string]any{"public_tool_progress": "运行命令"}},
		Event{Type: EventTextDelta, Content: "ans"},
		Event{Type: EventResult, Content: "answer", Done: true},
	)
	return p
}

// A short turn finishes the reply inside the card: no recall, no second message.
func TestSettleShortTurnFinishesInCard(t *testing.T) {
	p := settleTurn(t, 0, 60000)
	p.mu2.Lock()
	defer p.mu2.Unlock()
	if p.recallSeen != 0 || p.collapsed != 0 || len(p.finished) != 1 || p.finished[0] != "answer" {
		t.Fatalf("recalls %d collapses %d finished %q", p.recallSeen, p.collapsed, p.finished)
	}
	if all := p.stubPlatformEngine.getSent(); len(all) != 1 || all[0] != "answer" {
		t.Fatalf("want the card as the only message, got %q", all)
	}
}

// A long turn sends the reply as a new message (which notifies the reader) and only
// then collapses the card to its steps; the card is never recalled.
func TestSettleLongTurnCollapsesCardAndSendsNewMessage(t *testing.T) {
	p := settleTurn(t, 10*time.Millisecond, 1)
	p.mu2.Lock()
	defer p.mu2.Unlock()
	if p.recallSeen != 0 || p.collapsed != 1 || len(p.finished) != 0 {
		t.Fatalf("recalls %d collapses %d finished %q", p.recallSeen, p.collapsed, p.finished)
	}
	all := p.stubPlatformEngine.getSent()
	if len(all) != 2 || all[1] != "answer" {
		t.Fatalf("want the card plus one new reply, got %q", all)
	}
	if p.sentAtCollapse != 2 {
		t.Fatalf("card collapsed after %d messages, want it after the reply", p.sentAtCollapse)
	}
}
