package claudecode

import (
	"errors"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// Frames below follow the stream-json output of Claude Code 2.1.322
// (SDKAPIRetryMessage, SDKAssistantMessage.error, SDKResultSuccess with
// api_error_status).

func TestAPIRetryBecomesRetryEvent(t *testing.T) {
	cs := progressSession()
	cs.handleReadLoopLine(`{"type":"system","subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1200,"error_status":529,"error":"overloaded","uuid":"u1","session_id":"s1"}`)
	events := drainProgressEvents(cs)
	if len(events) != 1 || events[0].Type != core.EventRetry {
		t.Fatalf("events=%+v", events)
	}
	r := events[0].Retry
	if r == nil || r.Attempt != 2 || r.MaxRetries != 10 || r.HTTPStatus != 529 || r.Kind != "overloaded" || r.UsageLimited() {
		t.Fatalf("retry=%+v", r)
	}
}

func TestAPIRetryWithoutResponseAndUsageLimit(t *testing.T) {
	cs := progressSession()
	cs.handleReadLoopLine(`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":1,"retry_delay_ms":0,"error_status":null,"error":"unknown","no_response":{"waited_ms":30000,"retry_wait_ms":60000}}`)
	cs.handleReadLoopLine(`{"type":"system","subtype":"api_retry","attempt":3,"max_retries":10,"retry_delay_ms":60000,"error_status":429,"error":"rate_limit"}`)
	events := drainProgressEvents(cs)
	if len(events) != 2 {
		t.Fatalf("events=%+v", events)
	}
	if r := events[0].Retry; r.HTTPStatus != 0 || r.UsageLimited() {
		t.Fatalf("no-response retry=%+v", r)
	}
	if r := events[1].Retry; !r.UsageLimited() {
		t.Fatalf("429 retry=%+v", r)
	}
}

func TestExhaustedAPIRetriesEndTurnAsModelFailure(t *testing.T) {
	cs := progressSession()
	cs.handleReadLoopLine(`{"type":"assistant","error":"server_error","message":{"id":"m1","content":[{"type":"text","text":"API Error: 529 overloaded"}]},"parent_tool_use_id":null}`)
	cs.handleReadLoopLine(`{"type":"result","subtype":"success","is_error":true,"result":"API Error: 529 overloaded","api_error_status":529,"num_turns":1,"session_id":"s1"}`)
	events := drainProgressEvents(cs)
	if len(events) != 1 || events[0].Type != core.EventError {
		t.Fatalf("events=%+v", events)
	}
	var mf *core.ModelFailure
	if !errors.As(events[0].Error, &mf) || mf.HTTPStatus != 529 || mf.Kind != "server_error" || !mf.Unreachable() {
		t.Fatalf("error=%v", events[0].Error)
	}
	// The flag does not leak into the next turn.
	cs.handleReadLoopLine(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s1"}`)
	if ev := <-cs.events; ev.Type != core.EventResult || ev.Content != "ok" {
		t.Fatalf("next turn=%+v", ev)
	}
}

func TestUsageLimitFailureIsRecognized(t *testing.T) {
	cs := progressSession()
	cs.handleReadLoopLine(`{"type":"assistant","error":"rate_limit","message":{"content":[{"type":"text","text":"You've hit your limit"}]},"parent_tool_use_id":null}`)
	cs.handleReadLoopLine(`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your limit","session_id":"s1"}`)
	ev := <-cs.events
	var mf *core.ModelFailure
	if ev.Type != core.EventError || !errors.As(ev.Error, &mf) || !mf.UsageLimited() {
		t.Fatalf("event=%+v", ev)
	}
}

func TestOtherErrorResultsKeepTheOrdinaryPath(t *testing.T) {
	cs := progressSession()
	cs.handleReadLoopLine(`{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s1"}`)
	cs.handleReadLoopLine(`{"type":"result","subtype":"success","is_error":true,"result":"hook blocked","session_id":"s1"}`)
	for _, ev := range drainProgressEvents(cs) {
		if ev.Type != core.EventResult {
			t.Fatalf("non-API error result became %+v", ev)
		}
	}
}
