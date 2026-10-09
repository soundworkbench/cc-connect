package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuietToolProgressPlaceholderRegression(t *testing.T) {
	for _, mode := range []string{"quiet", "full", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "progress"}}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
			defer e.Stop()
			display := DisplayCfg{Mode: mode, ProgressHistory: "recent", ToolMessages: mode == "full", HideAgentFooter: true}
			if mode == "disabled" {
				zero := 0
				display.Mode = "quiet"
				display.ToolProgressLines = &zero
			}
			e.SetDisplayConfig(display)
			cfg := DefaultStreamPreviewCfg()
			cfg.IntervalMs = 20
			e.SetStreamPreviewCfg(cfg)
			key := "progress:one"
			session := e.sessions.GetOrCreateActive(key)
			as := newControllableSession("one")
			state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
			e.interactiveStates[key] = state
			done := make(chan struct{})
			go func() {
				e.processInteractiveEvents(state, session, e.sessions, key, "m1", time.Now(), nil, nil, "ctx")
				close(done)
			}()
			for i := 0; i < 5; i++ {
				as.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "PRIVATE_COMMAND /PRIVATE_PATH", Metadata: map[string]any{"public_tool_progress": fmt.Sprintf("正在检查%d", i)}}
			}
			// All events are processed before the preview timer can fire.
			time.Sleep(100 * time.Millisecond)
			p.previewMu.Lock()
			edits := append([]string(nil), p.edits...)
			starts := append([]string(nil), p.starts...)
			p.previewMu.Unlock()
			if len(starts) != 1 {
				t.Fatalf("starts=%v", starts)
			}
			if mode == "quiet" {
				if len(edits) != 1 || edits[0] != "card-1:正在检查2\n正在检查3\n正在检查4" {
					t.Fatalf("edits=%v", edits)
				}
			} else if mode == "disabled" {
				if len(edits) != 0 {
					t.Fatalf("disabled edits=%v", edits)
				}
			} else {
				if len(edits) != 5 || !strings.Contains(edits[4], "正在检查0") {
					t.Fatalf("full behavior changed: %v", edits)
				}
			}
			// Queue one more update then finish: no stale timer may edit the recalled card.
			if mode == "quiet" {
				as.events <- Event{Type: EventToolUse, ToolName: "Read", ToolInput: "/PRIVATE_PATH"}
				deadline := time.Now().Add(time.Second)
				for {
					p.previewMu.Lock()
					latest := p.edits[len(p.edits)-1]
					p.previewMu.Unlock()
					if strings.Contains(latest, "读取文件") {
						if latest != "card-1:正在检查3\n正在检查4\n读取文件" {
							t.Fatal(latest)
						}
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("tool without metadata never updated placeholder")
					}
					time.Sleep(time.Millisecond)
				}
				as.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "PRIVATE_COMMAND"}
			}
			as.events <- Event{Type: EventResult, Content: "最终回复", Done: true}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("final timed out")
			}
			p.previewMu.Lock()
			count := len(p.edits)
			p.previewMu.Unlock()
			time.Sleep(40 * time.Millisecond)
			p.previewMu.Lock()
			defer p.previewMu.Unlock()
			if len(p.edits) != count || len(p.deletes) != 1 || p.deletes[0] != "card-1" {
				t.Fatalf("late edits/recall: %v %v", p.edits, p.deletes)
			}
			if sent := p.getSent(); len(sent) != 1 || sent[0] != "最终回复" {
				t.Fatalf("sent=%v", sent)
			}
		})
	}
}

func TestToolProgressIntervalCoalescesLatestAndStops(t *testing.T) {
	p := &lifecyclePlatform{}
	cfg := DefaultStreamPreviewCfg()
	cfg.IntervalMs = 70
	sp := newStreamPreview(cfg, p, nil, context.Background(), nil)
	sp.startPlaceholder("思考中…")
	sp.replaceToolProgress("运行命令")
	sp.replaceToolProgress("读取文件")
	p.previewMu.Lock()
	n := len(p.edits)
	p.previewMu.Unlock()
	if n != 0 {
		t.Fatal("updated before interval")
	}
	deadline := time.Now().Add(time.Second)
	for {
		p.previewMu.Lock()
		edits := append([]string(nil), p.edits...)
		p.previewMu.Unlock()
		if len(edits) > 0 {
			if len(edits) != 1 || edits[0] != "card-1:读取文件" {
				t.Fatal(edits)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("latest pending progress never flushed")
		}
		time.Sleep(time.Millisecond)
	}
	sp.replaceToolProgress("修改文件")
	sp.discard()
	time.Sleep(100 * time.Millisecond)
	p.previewMu.Lock()
	defer p.previewMu.Unlock()
	if len(p.edits) != 1 || len(p.starts) != 1 {
		t.Fatalf("late/new message: %v %v", p.edits, p.starts)
	}
}

func TestPublicToolProgressNeverUsesRawInput(t *testing.T) {
	for tool, want := range map[string]string{"Bash": "运行命令", "Read": "读取文件", "Patch": "修改文件", "MCP": "调用工具", "mcp__fixture__search": "调用工具", "WebSearch": "联网搜索", "unknown": "执行操作"} {
		e := Event{ToolName: tool, ToolInput: "PRIVATE_COMMAND /PRIVATE_PATH", Metadata: map[string]any{"public_tool_progress": "English command /PRIVATE_PATH"}}
		if got := publicToolProgress(e); got != want {
			t.Fatalf("%s: %q", tool, got)
		}
	}
	if got := publicToolProgress(Event{Metadata: map[string]any{"public_tool_progress": "正在检查\n文件"}}); got != "正在检查 文件" {
		t.Fatal(got)
	}
}
