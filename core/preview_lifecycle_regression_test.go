package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type lifecyclePlatform struct {
	stubPlatformEngine
	previewMu              sync.Mutex
	starts, edits, deletes []string
	failUpdates            int
	textPreview            bool
}

func (p *lifecyclePlatform) StartPreviewImmediately() bool   { return true }
func (p *lifecyclePlatform) UseThinkingProcessPreview() bool { return !p.textPreview }
func (p *lifecyclePlatform) KeepPreviewOnFinish() bool       { return false }
func (p *lifecyclePlatform) SendPreviewStart(_ context.Context, _ any, text string) (any, error) {
	p.previewMu.Lock()
	defer p.previewMu.Unlock()
	p.starts = append(p.starts, text)
	return fmt.Sprintf("card-%d", len(p.starts)), nil
}
func (p *lifecyclePlatform) UpdateMessage(_ context.Context, id any, text string) error {
	p.previewMu.Lock()
	defer p.previewMu.Unlock()
	p.edits = append(p.edits, fmt.Sprintf("%v:%s", id, text))
	if p.failUpdates > 0 {
		p.failUpdates--
		return fmt.Errorf("transient preview update failure")
	}
	return nil
}
func (p *lifecyclePlatform) DeletePreviewMessage(_ context.Context, id any) error {
	p.previewMu.Lock()
	defer p.previewMu.Unlock()
	p.deletes = append(p.deletes, fmt.Sprint(id))
	return nil
}

func TestProcessInteractiveEvents_PreviewLifecycleOneCard(t *testing.T) {
	for _, tools := range []bool{true, false} {
		t.Run(fmt.Sprintf("tools_%t", tools), func(t *testing.T) {
			p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "lifecycle"}}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
			e.SetDisplayConfig(DisplayCfg{Mode: "quiet", ThinkingMessages: true, ToolMessages: tools, HideAgentFooter: true})
			e.SetStreamPreviewCfg(DefaultStreamPreviewCfg())
			key := "lifecycle:one"
			session := e.sessions.GetOrCreateActive(key)
			as := newControllableSession("one")
			state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
			e.interactiveStates[key] = state
			done := make(chan struct{})
			go func() {
				e.processInteractiveEvents(state, session, e.sessions, key, "m1", time.Now(), nil, nil, "ctx")
				close(done)
			}()
			deadline := time.Now().Add(2 * time.Second)
			for {
				p.previewMu.Lock()
				n := len(p.starts)
				p.previewMu.Unlock()
				if n > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no immediate placeholder before first model event")
				}
				time.Sleep(time.Millisecond)
			}
			as.events <- Event{Type: EventThinking, Content: "progress one"}
			as.events <- Event{Type: EventText, Content: "intermediate commentary"}
			as.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "pwd"}
			as.events <- Event{Type: EventToolResult, ToolName: "Bash", ToolResult: "hidden output"}
			as.events <- Event{Type: EventThinking, Content: "progress two"}
			as.events <- Event{Type: EventText, Content: "answer delta"}
			as.events <- Event{Type: EventResult, Content: "final answer", Done: true}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("turn did not finish")
			}
			if len(p.starts) != 1 || p.starts[0] != "思考中…" {
				t.Fatalf("starts=%v", p.starts)
			}
			if len(p.edits) != 2 {
				t.Fatalf("edits=%v", p.edits)
			}
			for _, edit := range p.edits {
				if !strings.HasPrefix(edit, "card-1:") {
					t.Fatalf("different card: %s", edit)
				}
			}
			if !strings.Contains(p.edits[1], "progress one") || !strings.Contains(p.edits[1], "progress two") {
				t.Fatalf("lost progress: %v", p.edits)
			}
			if len(p.deletes) != 1 || p.deletes[0] != "card-1" {
				t.Fatalf("recalls=%v", p.deletes)
			}
			if sent := p.getSent(); len(sent) != 1 || sent[0] != "final answer" {
				t.Fatalf("sent=%v", sent)
			}
		})
	}
}

func TestProcessInteractiveEvents_PreviewLifecycleErrorRecalls(t *testing.T) {
	p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "lifecycle"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.SetStreamPreviewCfg(DefaultStreamPreviewCfg())
	key := "lifecycle:error"
	as := newControllableSession("error")
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
	e.interactiveStates[key] = state
	errCh := make(chan error, 1)
	errCh <- fmt.Errorf("simulated send failure")
	e.processInteractiveEvents(state, e.sessions.GetOrCreateActive(key), e.sessions, key, "m2", time.Now(), nil, errCh, "ctx")
	if len(p.starts) != 1 || len(p.deletes) != 1 {
		t.Fatalf("starts=%v recalls=%v", p.starts, p.deletes)
	}
}

func TestCUJ_PreviewLifecycleThreeTurns(t *testing.T) {
	t.Run("public_progress_hidden_thinking", testPreviewLifecyclePublicProgressThreeTurns)
	for _, outcome := range []string{"success", "error", "nil_handle", "disabled"} {
		for _, instant := range []bool{false, true} {
			for _, textPreview := range []bool{false, true} {
				t.Run(fmt.Sprintf("queued/%s/instant_%t/text_%t", outcome, instant, textPreview), func(t *testing.T) {
					testPreviewLifecycleQueuedTurns(t, outcome, instant, textPreview)
				})
			}
		}
	}
	for _, failures := range []int{0, 1} {
		for _, textPreview := range []bool{false, true} {
			for _, instant := range []bool{false, true} {
				t.Run(fmt.Sprintf("text_%t_update_failures_%d_instant_%t", textPreview, failures, instant), func(t *testing.T) {
					testPreviewLifecycleThreeTurns(t, failures, textPreview, instant)
				})
			}
		}
	}
}

func testPreviewLifecycleThreeTurns(t *testing.T, failures int, textPreview, instant bool) {
	p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "lifecycle"}}
	p.failUpdates = failures
	p.textPreview = textPreview
	a := &cujAgent{}
	e := NewEngine("test", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.IntervalMs = 1
	cfg.MinDeltaChars = 1
	e.SetStreamPreviewCfg(cfg)
	e.SetInstantReply(InstantReplyCfg{Enabled: instant, Content: "思考中…"})
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", ThinkingMessages: true, HideAgentFooter: true})
	defer e.Stop()
	for turn := 1; turn <= 3; turn++ {
		events := []Event{
			{Type: EventThinking, Content: fmt.Sprintf("progress %d", turn)},
			{Type: EventToolUse, ToolName: "Bash", ToolInput: "pwd"},
			{Type: EventText, Content: "commentary"},
			{Type: EventResult, Content: fmt.Sprintf("answer %d", turn), Done: true},
		}
		if textPreview {
			events = []Event{
				{Type: EventText, Content: fmt.Sprintf("answer %d", turn)},
				{Type: EventResult, Content: fmt.Sprintf("answer %d", turn), Done: true},
			}
		}
		a.mu.Lock()
		if len(a.sessions) == 0 {
			a.nextSessionEvents = events
			a.nextSessionDelayMs = 10
		} else {
			s := a.sessions[0]
			s.mu.Lock()
			s.pendingEvents = events
			s.pendingDelayMs = 10
			s.mu.Unlock()
		}
		a.mu.Unlock()
		e.ReceiveMessage(p, &Message{SessionKey: "lifecycle:user", Platform: "lifecycle", MessageID: fmt.Sprintf("m%d", turn), UserID: "user", Content: fmt.Sprintf("request %d", turn), ReplyCtx: "ctx"})
		deadline := time.Now().Add(3 * time.Second)
		for len(p.getSent()) < turn || e.sessions.GetOrCreateActive("lifecycle:user").Busy() {
			if time.Now().After(deadline) {
				t.Fatalf("turn %d did not deliver final", turn)
			}
			time.Sleep(time.Millisecond)
		}
		p.previewMu.Lock()
		if len(p.starts) != turn || len(p.deletes) != turn || len(p.edits) != turn {
			t.Errorf("turn %d: starts=%v edits=%v recalls=%v", turn, p.starts, p.edits, p.deletes)
		}
		if !strings.Contains(p.edits[len(p.edits)-1], fmt.Sprintf("card-%d:", turn)) {
			t.Error("updated previous turn card")
		}
		p.previewMu.Unlock()
		if p.getSent()[turn-1] != fmt.Sprintf("answer %d", turn) {
			t.Fatalf("unexpected final: %v", p.getSent())
		}
	}
}

type placeholderOutcomePlatform struct {
	lifecyclePlatform
	outcome string
}

func (p *placeholderOutcomePlatform) SendPreviewStart(ctx context.Context, rc any, text string) (any, error) {
	switch p.outcome {
	case "error":
		return nil, fmt.Errorf("preview unavailable")
	case "nil_handle":
		return nil, nil
	}
	return p.lifecyclePlatform.SendPreviewStart(ctx, rc, text)
}

func TestProcessInteractiveEvents_PreviewLifecycleInstantReplyFallback(t *testing.T) {
	for _, outcome := range []string{"success", "error", "nil_handle", "disabled"} {
		t.Run(outcome, func(t *testing.T) {
			p := &placeholderOutcomePlatform{outcome: outcome}
			p.n = "lifecycle"
			e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
			cfg := DefaultStreamPreviewCfg()
			cfg.Enabled = outcome != "disabled"
			e.SetStreamPreviewCfg(cfg)
			e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
			e.SetInstantReply(InstantReplyCfg{Enabled: true, Content: "思考中…"})
			as := newControllableSession("instant")
			state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
			key := "lifecycle:instant"
			e.interactiveStates[key] = state
			as.events <- Event{Type: EventResult, Content: "final answer", Done: true}
			e.processInteractiveEvents(state, e.sessions.GetOrCreateActive(key), e.sessions, key, "m", time.Now(), nil, nil, "ctx")
			sent := p.getSent()
			want := 2 // Failed/disabled preview must retain its ordinary acknowledgement.
			if outcome == "success" {
				want = 1
			}
			if len(sent) != want || sent[len(sent)-1] != "final answer" {
				t.Fatalf("sent=%v, want %d messages ending in final answer", sent, want)
			}
			if want == 2 && sent[0] != "思考中…" {
				t.Fatalf("missing fallback acknowledgement: %v", sent)
			}
		})
	}
}

func TestProcessInteractiveEvents_PreviewLifecyclePermissionKeepsHandle(t *testing.T) {
	p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "lifecycle"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.SetStreamPreviewCfg(DefaultStreamPreviewCfg())
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", ThinkingMessages: true, HideAgentFooter: true})
	key := "lifecycle:permission"
	as := newControllableSession("permission")
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
	e.interactiveStates[key] = state
	as.events <- Event{Type: EventThinking, Content: "before question"}
	as.events <- Event{Type: EventPermissionRequest, RequestID: "permission", ToolName: "Bash", ToolInput: "pwd"}
	as.events <- Event{Type: EventThinking, Content: "after question"}
	as.events <- Event{Type: EventResult, Content: "final answer", Done: true}
	resolved := make(chan struct{})
	go func() {
		defer close(resolved)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			state.mu.Lock()
			pending := state.pending
			state.mu.Unlock()
			if pending != nil {
				pending.resolve()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	e.processInteractiveEvents(state, e.sessions.GetOrCreateActive(key), e.sessions, key, "m", time.Now(), nil, nil, "ctx")
	<-resolved
	if len(p.starts) != 1 || len(p.edits) != 2 || len(p.deletes) != 1 {
		t.Fatalf("starts=%v edits=%v recalls=%v", p.starts, p.edits, p.deletes)
	}
	if !strings.HasPrefix(p.edits[1], "card-1:") {
		t.Fatal("question detached the original card")
	}
}
