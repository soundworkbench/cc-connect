package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// Sanitized Claude Code 2.1.273 stream-json frame, reproduced by resuming an
// isolated transcript with an orphaned async task. It precedes the replay of
// the actual user request. No API call or user-message association exists.
const resumedNotificationResult = `{"type":"result","subtype":"success","is_error":false,"duration_ms":51,"duration_api_ms":0,"num_turns":0,"stop_reason":null,"session_id":"resume-test","result":"","origin":{"kind":"task-notification"},"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0},"permission_denials":[],"queued_turn_count":0,"result_index":0}`

func TestClaudeResumedNotificationDoesNotFinishUserTurn(t *testing.T) {
	cs := progressSession()
	// More than one orphan may be reported on resume. No EventResult (even
	// Done=false) may escape: the idle/unsolicited reader also consumes them.
	for range 2 {
		cs.handleReadLoopLine(resumedNotificationResult)
		if events := drainProgressEvents(cs); len(events) != 0 {
			t.Fatalf("resume bookkeeping ended or leaked into the user turn: %+v", events)
		}
	}
	cs.handleReadLoopLine(`{"type":"user","isReplay":true,"uuid":"user-request","message":{"role":"user","content":"Reply RESUME_OK"}}`)
	cs.handleReadLoopLine(`{"type":"assistant","user_message_uuid":"user-request","message":{"content":[{"type":"text","text":"RESUME_OK"}],"usage":{"input_tokens":20,"output_tokens":1}}}`)
	cs.handleReadLoopLine(`{"type":"result","subtype":"success","result":"RESUME_OK","num_turns":1,"user_message_uuid":"user-request","user_message_uuids":["user-request"],"usage":{"input_tokens":20,"output_tokens":12}}`)
	events := drainProgressEvents(cs)
	if len(events) != 2 || events[0].Type != core.EventText || events[0].Content != "RESUME_OK" || events[1].Type != core.EventResult || !events[1].Done || events[1].Content != "RESUME_OK" || events[1].OutputTokens != 12 {
		t.Fatalf("lost or prematurely completed the real answer: %+v", events)
	}
	// A following turn must still terminate normally; filtering is not a sticky
	// session flag, and must not leave the engine waiting forever.
	cs.handleReadLoopLine(`{"type":"result","result":"NEXT_OK"}`)
	events = drainProgressEvents(cs)
	if len(events) != 1 || !events[0].Done || events[0].Content != "NEXT_OK" {
		t.Fatalf("subsequent turn broken: %+v", events)
	}
}

func TestClaudeNotificationDoesNotFlushPendingTextOrUsage(t *testing.T) {
	cs := progressSession()
	assistantBlock(cs, map[string]any{"type": "text", "text": "Still checking"})
	cs.lastUsage = &core.ContextUsage{UsedTokens: 20, OutputTokens: 10, TotalTokens: 30}
	cs.handleReadLoopLine(resumedNotificationResult)
	if events := drainProgressEvents(cs); len(events) != 0 {
		t.Fatalf("notification flushed the foreground answer: %+v", events)
	}
	if len(cs.pendingText) != 1 || cs.lastUsage.TotalTokens != 30 {
		t.Fatal("notification changed foreground text or usage")
	}
	assistantBlock(cs, map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"file_path": "fixture"}})
	events := drainProgressEvents(cs)
	if len(events) != 2 || events[0].Type != core.EventProgress || events[0].Content != "Still checking" || events[1].Type != core.EventToolUse {
		t.Fatalf("notification reclassified public progress as a final answer: %+v", events)
	}
}

func TestClaudeNotificationPreservesRealResults(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"ordinary_empty_answer", func(r map[string]any) { delete(r, "origin") }},
		{"other_origin", func(r map[string]any) { r["origin"] = map[string]any{"kind": "sdk"} }},
		{"notification_error", func(r map[string]any) { r["is_error"] = true }},
		{"error_subtype", func(r map[string]any) { r["subtype"] = "error_during_execution" }},
		{"notification_answer", func(r map[string]any) { r["result"] = "Task finished" }},
		{"whitespace_answer", func(r map[string]any) { r["result"] = " " }},
		{"notification_model_turn", func(r map[string]any) { r["num_turns"] = float64(1) }},
		{"missing_turn_count", func(r map[string]any) { delete(r, "num_turns") }},
		{"missing_error_flag", func(r map[string]any) { delete(r, "is_error") }},
		{"missing_result", func(r map[string]any) { delete(r, "result") }},
		{"api_work", func(r map[string]any) { r["duration_api_ms"] = float64(1) }},
		{"input_usage", func(r map[string]any) { r["usage"].(map[string]any)["input_tokens"] = float64(1) }},
		{"output_usage", func(r map[string]any) { r["usage"].(map[string]any)["output_tokens"] = float64(1) }},
		{"cache_read_usage", func(r map[string]any) { r["usage"].(map[string]any)["cache_read_input_tokens"] = float64(1) }},
		{"cache_write_usage", func(r map[string]any) { r["usage"].(map[string]any)["cache_creation_input_tokens"] = float64(1) }},
		{"explicit_errors", func(r map[string]any) { r["errors"] = []any{"request failed"} }},
		{"missing_api_duration", func(r map[string]any) { delete(r, "duration_api_ms") }},
		{"missing_usage", func(r map[string]any) { delete(r, "usage") }},
		// Notification-triggered turns can absorb user messages. Never discard
		// their results, including valid zero-turn/error/empty command replies.
		{"associated_user", func(r map[string]any) { r["user_message_uuid"] = "foreground" }},
		{"absorbed_users", func(r map[string]any) { r["user_message_uuids"] = []any{"foreground"} }},
		{"permission_denied", func(r map[string]any) { r["permission_denials"] = []any{map[string]any{"tool_name": "Read"}} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(resumedNotificationResult), &raw); err != nil {
				t.Fatal(err)
			}
			tc.change(raw)
			cs := progressSession()
			cs.handleResult(raw)
			events := drainProgressEvents(cs)
			if len(events) != 1 || events[0].Type != core.EventResult || !events[0].Done {
				t.Fatalf("real/ambiguous result was swallowed: %+v", events)
			}
			if want, ok := raw["result"].(string); ok && events[0].Content != want {
				t.Fatalf("result changed: %q != %q", events[0].Content, want)
			}
		})
	}
}
