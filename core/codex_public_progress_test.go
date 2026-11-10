package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Agent boundaries supply public events; exercise the real quiet compact card
// path with private thinking enabled and tool messages disabled (coordinator settings).
func TestQuietCompactPublicProgressAndSingleFinal(t *testing.T) {
	p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "progress"}}
	e := NewEngine("progress", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	defer e.Stop()
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", ThinkingMessages: true, ToolMessages: false, HideAgentFooter: true})
	cfg := DefaultStreamPreviewCfg()
	cfg.IntervalMs = 10
	e.SetStreamPreviewCfg(cfg)
	key := "progress:one"
	session := e.sessions.GetOrCreateActive(key)
	as := newControllableSession("one")
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
	e.interactiveStates[key] = state
	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "m1", time.Now(), nil, nil, "ctx", 0)
		close(done)
	}()
	as.events <- Event{Type: EventThinking, Content: "PRIVATE_THINKING"}
	as.events <- Event{Type: EventProgress, Content: "我先审回执。"}
	as.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "PRIVATE_ARGUMENT", Metadata: map[string]any{"public_tool_progress": "运行命令 · git log"}}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.previewMu.Lock()
		edits := strings.Join(p.edits, "\n")
		p.previewMu.Unlock()
		if strings.Contains(edits, "我先审回执。") && strings.Contains(edits, "运行命令 · git log") {
			if strings.Contains(edits, "PRIVATE_") {
				t.Fatal(edits)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no public progress card: %s", edits)
		}
		time.Sleep(time.Millisecond)
	}
	as.events <- Event{Type: EventText, Content: "最终回复"}
	as.events <- Event{Type: EventResult, Done: true}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("final timed out")
	}
	if sent := p.getSent(); len(sent) != 1 || sent[0] != "最终回复" {
		t.Fatalf("final=%v", sent)
	}
}
