package claudecode

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func progressSession() *claudeSession {
	cs := &claudeSession{ctx: context.Background(), events: make(chan core.Event, 32)}
	cs.sessionID.Store("progress-test")
	return cs
}

func assistantBlock(cs *claudeSession, block map[string]any) {
	cs.handleAssistant(map[string]any{"message": map[string]any{"content": []any{block}}})
}

func drainProgressEvents(cs *claudeSession) []core.Event {
	var events []core.Event
	for len(cs.events) > 0 {
		events = append(events, <-cs.events)
	}
	return events
}

func TestClaudePublicProgressBeforeTool(t *testing.T) {
	cs := progressSession()
	assistantBlock(cs, map[string]any{"type": "text", "text": "正在检查页面。"})
	if len(cs.events) != 0 {
		t.Fatal("text must wait for tool/final classification")
	}
	assistantBlock(cs, map[string]any{"type": "thinking", "thinking": "PRIVATE_ANALYSIS"})
	assistantBlock(cs, map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"file_path": "fixture"}})
	assistantBlock(cs, map[string]any{"type": "text", "text": "检查完成。"})
	cs.handleResult(map[string]any{"result": "检查完成。"})
	events := drainProgressEvents(cs)
	want := []core.EventType{core.EventThinking, core.EventType("progress"), core.EventToolUse, core.EventText, core.EventResult}
	if len(events) != len(want) {
		t.Fatalf("events=%v", events)
	}
	for i, typ := range want {
		if events[i].Type != typ {
			t.Errorf("event %d type=%s want=%s", i, events[i].Type, typ)
		}
	}
	if events[1].Content != "正在检查页面。" || events[3].Content != "检查完成。" {
		t.Fatal("public progress/final lost")
	}
}

func TestClaudePublicProgressCompactionAndTurnIsolation(t *testing.T) {
	cs := progressSession()
	assistantBlock(cs, map[string]any{"type": "text", "text": "第一轮进度。"})
	cs.handleResult(map[string]any{"subtype": "compact"})
	compact := <-cs.events
	if compact.Type != core.EventResult || compact.Done {
		t.Fatalf("compaction=%v", compact)
	}
	assistantBlock(cs, map[string]any{"type": "tool_use", "name": "AskUserQuestion"})
	events := drainProgressEvents(cs)
	if len(events) != 1 || events[0].Type != core.EventType("progress") {
		t.Fatalf("question progress=%v", events)
	}
	assistantBlock(cs, map[string]any{"type": "text", "text": "第一轮完成。"})
	cs.handleResult(map[string]any{"result": "第一轮完成。"})
	drainProgressEvents(cs)
	cs.handleResult(map[string]any{"result": "第二轮完成。"})
	events = drainProgressEvents(cs)
	if len(events) != 1 || events[0].Content != "第二轮完成。" {
		t.Fatalf("stale text leaked=%v", events)
	}
}

func TestClaudePublicProgressNestedAssistantIgnored(t *testing.T) {
	cs := progressSession()
	raw := map[string]any{"parent_tool_use_id": "child", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "CHILD_PRIVATE_TEXT"}}}}
	raw["type"] = "assistant"
	line, _ := json.Marshal(raw)
	cs.handleReadLoopLine(string(line))
	cs.handleResult(map[string]any{"result": "main final"})
	events := drainProgressEvents(cs)
	if len(events) != 1 || events[0].Type != core.EventResult {
		t.Fatalf("child content leaked into main=%v", events)
	}
}
