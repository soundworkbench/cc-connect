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
			e.SetDisplayConfig(DisplayCfg{Mode: "quiet", ProgressHistory: "recent", ThinkingMessages: true, ToolMessages: tools, HideAgentFooter: true})
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
	t.Run("silent_visible_normal", testPreviewLifecycleSilentVisibleThreeTurns)
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
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", ProgressHistory: "recent", ThinkingMessages: true, HideAgentFooter: true})
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
		wantEdits := turn
		if !textPreview {
			wantEdits = turn * 2
		} // thinking + quiet tool progress on the same card
		if len(p.starts) != turn || len(p.deletes) != turn || len(p.edits) != wantEdits {
			t.Errorf("turn %d: starts=%v edits=%v recalls=%v", turn, p.starts, p.edits, p.deletes)
		}
		if !textPreview && p.edits[len(p.edits)-1] != fmt.Sprintf("card-%d:运行命令", turn) {
			t.Errorf("quiet tool progress missing or unsafe: %v", p.edits)
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
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", ProgressHistory: "recent", ThinkingMessages: true, HideAgentFooter: true})
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

// Permissions interrupt the event loop, but must not detach or replace the
// append history. Private thinking stays hidden even when explicitly enabled.
func TestAppendProgressPermissionKeepsHistoryAndHandle(t *testing.T) {
	p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "lifecycle"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	defer e.Stop()
	cfg := DefaultStreamPreviewCfg()
	cfg.IntervalMs = 5
	e.SetStreamPreviewCfg(cfg)
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", ThinkingMessages: true, HideAgentFooter: true})
	key := "lifecycle:append-permission"
	as := newControllableSession("permission")
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
	e.interactiveStates[key] = state
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.processInteractiveEvents(state, e.sessions.GetOrCreateActive(key), e.sessions, key, "m", time.Now(), nil, nil, "ctx")
	}()
	waitEdit := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			p.previewMu.Lock()
			text := ""
			if len(p.edits) > 0 {
				text = p.edits[len(p.edits)-1]
			}
			p.previewMu.Unlock()
			if text == want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("missing preview %q", want)
	}
	as.events <- Event{Type: EventThinking, Content: "PRIVATE_BEFORE"}
	as.events <- Event{Type: EventProgress, ProgressID: "p1", Content: "先检查。"}
	waitEdit("card-1:先检查。")
	as.events <- Event{Type: EventPermissionRequest, RequestID: "permission", ToolName: "Bash", ToolInput: "fixture"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		state.mu.Lock()
		pending := state.pending
		state.mu.Unlock()
		if pending != nil {
			pending.resolve()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("permission never appeared")
		}
		time.Sleep(time.Millisecond)
	}
	as.events <- Event{Type: EventThinking, Content: "PRIVATE_AFTER"}
	as.events <- Event{Type: EventProgress, ProgressID: "p2", Content: "继续验证。"}
	waitEdit("card-1:先检查。\n继续验证。")
	as.events <- Event{Type: EventResult, Content: "final answer", Done: true}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never finished")
	}
	p.previewMu.Lock()
	defer p.previewMu.Unlock()
	if len(p.starts) != 1 || len(p.edits) != 2 || len(p.deletes) != 1 {
		t.Fatalf("starts=%v edits=%v deletes=%v", p.starts, p.edits, p.deletes)
	}
}

func TestProcessInteractiveEvents_NoReplyDeferredPlaceholderZeroFootprint(t *testing.T) {
	for _, textPreview := range []bool{false, true} {
		for _, instant := range []bool{false, true} {
			for _, chunks := range [][]string{nil, {"N"}, {"NO_R"}, {"**NO_R"}, {"NO_REPLY"}, {"NO_R", "EPLY"}, {"  ", "no_reply\n"}, {"**NO_REPLY"}} {
				t.Run(fmt.Sprintf("text_%t/instant_%t/chunks_%v", textPreview, instant, chunks), func(t *testing.T) {
					p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "lifecycle"}, textPreview: textPreview}
					e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
					defer e.Stop()
					e.SetStreamPreviewCfg(DefaultStreamPreviewCfg())
					e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
					e.SetInstantReply(InstantReplyCfg{Enabled: instant, Content: "思考中…"})
					as := newControllableSession("silent")
					state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
					key := "lifecycle:silent"
					e.interactiveStates[key] = state
					for _, chunk := range chunks {
						as.events <- Event{Type: EventText, Content: chunk}
					}
					as.events <- Event{Type: EventResult, Content: "NO_REPLY", Done: true}
					e.processInteractiveEvents(state, e.sessions.GetOrCreateActive(key), e.sessions, key, "silent", time.Now(), nil, nil, "ctx")
					if len(p.getSent()) != 0 || len(p.starts) != 0 || len(p.edits) != 0 || len(p.deletes) != 0 {
						t.Fatalf("silent turn left a footprint: sent=%v starts=%v edits=%v recalls=%v", p.getSent(), p.starts, p.edits, p.deletes)
					}
				})
			}
		}
	}
}

func TestProcessInteractiveEvents_TextWithNoReplyDeliversOnlyText(t *testing.T) {
	for _, result := range []string{"NO_REPLY", "**NO_REPLY"} {
		for _, textPreview := range []bool{false, true} {
			t.Run(fmt.Sprintf("result_%s/text_%t", result, textPreview), func(t *testing.T) {
				p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "lifecycle"}, textPreview: textPreview}
				e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
				defer e.Stop()
				e.SetStreamPreviewCfg(DefaultStreamPreviewCfg())
				e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
				as := newControllableSession("visible")
				state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
				key := "lifecycle:visible"
				e.interactiveStates[key] = state
				as.events <- Event{Type: EventText, Content: "visible answer"}
				as.events <- Event{Type: EventText, Content: "\nNO_REPLY"}
				as.events <- Event{Type: EventResult, Content: result, Done: true}
				e.processInteractiveEvents(state, e.sessions.GetOrCreateActive(key), e.sessions, key, "visible", time.Now(), nil, nil, "ctx")
				if sent := p.getSent(); len(sent) != 1 || sent[0] != "visible answer" {
					t.Fatalf("visible text lost or marker leaked: %v", sent)
				}
			})
		}
	}
}

func TestAcknowledgeTurnStart_DefersPlaceholderUntilVisibleOutput(t *testing.T) {
	p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "lifecycle"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangChinese)
	defer e.Stop()
	sp := newStreamPreview(DefaultStreamPreviewCfg(), p, "ctx", e.ctx, nil)
	e.acknowledgeTurnStart(p, "ctx", sp, false)
	if len(p.starts) != 0 || len(p.getSent()) != 0 {
		t.Fatalf("acknowledged before visible output: starts=%v sent=%v", p.starts, p.getSent())
	}
	sp.acknowledgeVisible("NO_REPLY")
	if len(p.starts) != 0 {
		t.Fatalf("marker created a placeholder: %v", p.starts)
	}
	sp.acknowledgeVisible("visible progress")
	sp.replacePreview("visible progress")
	if len(p.starts) != 1 || p.starts[0] != "思考中…" || len(p.edits) != 1 || p.edits[0] != "card-1:visible progress" {
		t.Fatalf("starts=%v edits=%v", p.starts, p.edits)
	}
	sp.discard()
}

func testPreviewLifecycleSilentVisibleThreeTurns(t *testing.T) {
	p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "lifecycle"}}
	s := &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 3)}
	e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
	cfg := DefaultStreamPreviewCfg()
	cfg.IntervalMs = 1
	e.SetStreamPreviewCfg(cfg)
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	e.SetInstantReply(InstantReplyCfg{Enabled: true, Content: "思考中…"})
	t.Cleanup(func() { e.cancel(); _ = e.Stop() })
	key := "lifecycle:silent-visible"
	for turn := 1; turn <= 3; turn++ {
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), UserID: "user", MessageID: fmt.Sprintf("m%d", turn), Content: fmt.Sprintf("request %d", turn), ReplyCtx: "ctx"})
		select {
		case <-s.sent:
		case <-time.After(3 * time.Second):
			t.Fatal("request did not reach agent")
		}
		switch turn {
		case 1:
			s.events <- Event{Type: EventText, Content: "NO_R"}
			s.events <- Event{Type: EventText, Content: "EPLY"}
			s.events <- Event{Type: EventResult, Content: "NO_REPLY", Done: true}
		case 2:
			s.events <- Event{Type: EventToolUse, ToolName: "Bash"}
			s.events <- Event{Type: EventProgress, Content: "正在检查。", ProgressID: "check"}
			s.events <- Event{Type: EventToolUse, ToolName: "Bash"}
			s.events <- Event{Type: EventText, Content: "检查完成。\nNO_REPLY"}
			s.events <- Event{Type: EventResult, Content: "NO_REPLY", Done: true}
		case 3:
			s.events <- Event{Type: EventText, Content: "普通答复。"}
			s.events <- Event{Type: EventResult, Content: "普通答复。", Done: true}
		}
		deadline := time.Now().Add(3 * time.Second)
		for e.sessions.GetOrCreateActive(key).Busy() {
			if time.Now().After(deadline) {
				t.Fatalf("turn %d did not finish", turn)
			}
			time.Sleep(time.Millisecond)
		}
		p.previewMu.Lock()
		starts, recalls := len(p.starts), len(p.deletes)
		p.previewMu.Unlock()
		if starts != turn-1 || recalls != turn-1 {
			t.Fatalf("turn %d: starts=%d recalls=%d", turn, starts, recalls)
		}
		want := []string{"检查完成。", "普通答复。"}[:turn-1]
		if got := p.getSent(); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("turn %d: sent=%v want=%v", turn, got, want)
		}
	}
}
