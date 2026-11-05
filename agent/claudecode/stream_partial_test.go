package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// Guards stream_partial_text: deltas only drive the preview (whole block so far),
// nested-agent deltas stay private, and the complete message is still the single
// source of final text, so the reply is never doubled.
func streamDelta(cs *claudeSession, parent, text string) {
	raw := map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}}
	if parent != "" {
		raw["parent_tool_use_id"] = parent
	}
	cs.handleReadLoopLine(mustJSON(raw))
}

func mustJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func streamMarker(cs *claudeSession, kind string) {
	cs.handleReadLoopLine(mustJSON(map[string]any{"type": "stream_event", "event": map[string]any{"type": kind}}))
}

func TestStreamPartialTextDeltasAreWholeBlockPreviewOnly(t *testing.T) {
	cs := progressSession()
	streamMarker(cs, "message_start")
	streamDelta(cs, "", "先")
	streamDelta(cs, "", "查一下")
	streamDelta(cs, "tool-9", "子代理的私有输出")
	streamMarker(cs, "content_block_stop")
	streamMarker(cs, "content_block_start")
	streamDelta(cs, "", "结论")
	events := drainProgressEvents(cs)
	want := []string{"先", "先查一下", "结论"}
	if len(events) != len(want) {
		t.Fatal(events)
	}
	for i, e := range events {
		if e.Type != core.EventTextDelta || e.Content != want[i] {
			t.Fatal(i, e)
		}
	}

	// The complete message and the result still yield exactly one final text.
	cs.handleAssistant(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "结论"}}}})
	cs.handleResult(map[string]any{"type": "result", "result": "结论"})
	var texts int
	for _, e := range drainProgressEvents(cs) {
		if e.Type == core.EventText {
			texts++
			if e.Content != "结论" {
				t.Fatal(e)
			}
		}
		if e.Type == core.EventTextDelta {
			t.Fatal("no delta after the block ended", e)
		}
	}
	if texts != 1 {
		t.Fatal("final text count", texts)
	}
}
