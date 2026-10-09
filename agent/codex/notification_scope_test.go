package codex

import (
	"encoding/json"
	"github.com/chenhg5/cc-connect/core"
	"testing"
)

func TestAppServerNotification_ChildIdleDoesNotFinishParent(t *testing.T) {
	s := &appServerSession{currentTurn: "parent-turn", events: make(chan core.Event, 16), pendingMsgs: []string{"parent progress"}}
	s.threadID.Store("parent")
	notify := func(method, body string) { s.handleNotification(method, json.RawMessage(body)) }
	// Observed failure: child thread creation sent idle while its parent still worked.
	notify("thread/status/changed", `{"threadId":"child","status":{"type":"idle"}}`)
	notify("turn/started", `{"threadId":"child","turn":{"id":"child-turn"}}`)
	notify("item/completed", `{"threadId":"child","turnId":"child-turn","item":{"type":"agentMessage","text":"child private result"}}`)
	notify("turn/completed", `{"threadId":"child","turn":{"id":"child-turn","status":"completed"}}`)
	if len(s.events) != 0 || s.currentTurn != "parent-turn" {
		t.Fatalf("child notifications ended or emitted into parent: turn=%q events=%d", s.currentTurn, len(s.events))
	}
	if len(s.pendingMsgs) != 1 || s.pendingMsgs[0] != "parent progress" {
		t.Fatal("child cleared or contaminated parent output")
	}
	// An idle update (or stale result) is not a substitute for the current turn result.
	notify("thread/status/changed", `{"threadId":"parent","status":{"type":"idle"}}`)
	notify("turn/completed", `{"threadId":"parent","turn":{"id":"previous-turn","status":"completed"}}`)
	if len(s.events) != 0 {
		t.Fatal("idle/stale event prematurely ended parent")
	}
	notify("item/completed", `{"threadId":"parent","turnId":"parent-turn","item":{"type":"agentMessage","text":"parent final"}}`)
	notify("turn/completed", `{"threadId":"parent","turn":{"id":"parent-turn","status":"completed"}}`)
	var text string
	results := 0
	for len(s.events) > 0 {
		e := <-s.events
		if e.Type == core.EventText {
			text += e.Content
		}
		if e.Type == core.EventResult {
			results++
		}
	}
	if text != "parent progressparent final" || results != 1 {
		t.Fatalf("parent result lost: %q, results=%d", text, results)
	}
	notify("turn/completed", `{"threadId":"parent","turn":{"id":"parent-turn","status":"completed"}}`)
	if len(s.events) != 0 {
		t.Fatal("duplicate completion emitted again")
	}
}

func TestAppServerNotification_ForeignThreadToolsUsageAndErrorsIgnored(t *testing.T) {
	s := &appServerSession{currentTurn: "turn", events: make(chan core.Event, 16)}
	s.threadID.Store("parent")
	cases := []struct{ method, body string }{
		{"item/started", `{"threadId":"child","turnId":"turn","item":{"type":"commandExecution","command":"private child command"}}`},
		{"error", `{"threadId":"child","message":"private child error"}`},
		{"thread/tokenUsage/updated", `{"threadId":"child","turnId":"turn","tokenUsage":{"last":{"totalTokens":99},"modelContextWindow":100}}`},
		{"item/completed", `{"threadId":"parent","turnId":"old-turn","item":{"type":"agentMessage","text":"late stale text"}}`},
		{"turn/completed", `{"turn":{"id":"turn"}}`},
	}
	for _, c := range cases {
		s.handleNotification(c.method, json.RawMessage(c.body))
	}
	if len(s.events) != 0 || len(s.pendingMsgs) != 0 || s.currentTurn != "turn" || s.GetContextUsage() != nil {
		t.Fatal("unrelated event changed parent state")
	}
}
