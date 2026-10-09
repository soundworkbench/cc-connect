package claudecode

import "testing"

func TestAppendProgressNativeSourceIDs(t *testing.T) {
	cs := progressSession()
	raw := map[string]any{"message": map[string]any{"id": "message-1", "content": []any{
		map[string]any{"type": "text", "text": "正在检查。"},
		map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read", "input": map[string]any{}},
	}}}
	cs.handleAssistant(raw)
	cs.handleAssistant(raw) // Native replay carries the same IDs.
	events := drainProgressEvents(cs)
	if len(events) != 4 {
		t.Fatal(events)
	}
	if events[0].ProgressID != "message-1:0" || events[1].ProgressID != "tool-1" || events[2].ProgressID != events[0].ProgressID || events[3].ProgressID != events[1].ProgressID {
		t.Fatal(events)
	}
}
