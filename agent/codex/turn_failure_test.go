package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

const disconnectedTurnMessage = "stream disconnected before completion: stream closed before response.completed"

func TestAppServerNotification_FailedTurnIsNotEmptySuccess(t *testing.T) {
	for _, tc := range []struct{ name, terminal, want string }{
		{"stream_disconnected", `"status":"failed","error":{"message":"` + disconnectedTurnMessage + `","codexErrorInfo":"other"}`, disconnectedTurnMessage},
		{"stream_disconnected_no_pending", `"status":"failed","error":{"message":"` + disconnectedTurnMessage + `"}`, disconnectedTurnMessage},
		{"failed_without_error", `"status":"failed"`, "failed"},
		{"failed_without_message", `"status":"failed","error":{"message":"  "}`, "failed"},
		{"error_even_with_completed_status", `"status":"completed","error":{"message":"backend failure"}`, "backend failure"},
		{"interrupted", `"status":"interrupted"`, "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &appServerSession{currentTurn: "turn", events: make(chan core.Event, 16), pendingMsgs: []string{"unfinished commentary"}}
			if tc.name == "stream_disconnected_no_pending" {
				s.pendingMsgs = nil
			}
			s.threadID.Store("parent")
			s.alive.Store(true)
			body := json.RawMessage(`{"threadId":"parent","turn":{"id":"turn",` + tc.terminal + `}}`)
			s.handleNotification("turn/completed", body)
			if len(s.events) != 1 {
				t.Fatalf("want exactly one terminal error, got %d events", len(s.events))
			}
			ev := <-s.events
			if ev.Type != core.EventError || ev.Error == nil || !strings.Contains(ev.Error.Error(), tc.want) {
				t.Fatalf("failure became empty success: %+v", ev)
			}
			if s.currentTurn != "" || s.completedTurn != "turn" || len(s.pendingMsgs) != 0 || !s.Alive() {
				t.Fatal("failure did not clear only the finished turn")
			}
			s.handleNotification("turn/completed", body)
			s.handleNotification("turn/completed", json.RawMessage(`{"threadId":"parent","turn":{"id":"turn","status":"completed"}}`))
			if len(s.events) != 0 {
				t.Fatal("duplicate terminal notification emitted again")
			}
			s.handleNotification("turn/started", json.RawMessage(`{"threadId":"parent","turn":{"id":"next"}}`))
			s.handleNotification("turn/completed", body)
			s.handleNotification("turn/completed", json.RawMessage(`{"threadId":"child","turn":{"id":"next","status":"failed","error":{"message":"private child error"}}}`))
			if len(s.events) != 0 || s.currentTurn != "next" {
				t.Fatal("stale/foreign failure contaminated next turn")
			}
			s.handleNotification("item/completed", json.RawMessage(`{"threadId":"parent","turnId":"next","item":{"type":"agentMessage","text":"NEXT_OK","phase":"final_answer"}}`))
			s.handleNotification("turn/completed", json.RawMessage(`{"threadId":"parent","turn":{"id":"next","status":"completed"}}`))
			if len(s.events) != 2 {
				t.Fatalf("next turn did not recover: %d events", len(s.events))
			}
			text, result := <-s.events, <-s.events
			if text.Type != core.EventText || text.Content != "NEXT_OK" || result.Type != core.EventResult {
				t.Fatalf("next turn contains failed output: %+v %+v", text, result)
			}
		})
	}
}

func TestAppServerNotification_FailedTurnRedactsKnownSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-private-environment-key")
	s := &appServerSession{currentTurn: "turn", events: make(chan core.Event, 16), extraEnv: []string{"CUSTOM_API_TOKEN=test-private-extra-token"}}
	s.threadID.Store("parent")
	s.handleNotification("turn/completed", json.RawMessage(`{"threadId":"parent","turn":{"id":"turn","status":"failed","error":{"message":"request rejected: test-private-environment-key test-private-extra-token"}}}`))
	ev := <-s.events
	if ev.Error == nil || !strings.Contains(ev.Error.Error(), "request rejected") || strings.Contains(ev.Error.Error(), "test-private-") {
		t.Fatalf("unsafe or missing error: %+v", ev)
	}
}

func TestAppServerNotification_ConcurrentFailedCompletionEmitsOnce(t *testing.T) {
	s := &appServerSession{currentTurn: "turn", events: make(chan core.Event, 16), pendingMsgs: []string{"unfinished"}}
	s.threadID.Store("parent")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleNotification("turn/completed", json.RawMessage(`{"threadId":"parent","turn":{"id":"turn","status":"failed"}}`))
		}()
	}
	wg.Wait()
	if len(s.events) != 1 {
		t.Fatalf("duplicate terminal events: %d", len(s.events))
	}
	if ev := <-s.events; ev.Type != core.EventError {
		t.Fatalf("failure emitted success: %+v", ev)
	}
}

func TestAppServerNotification_FastFailedTurnBeforeStartAcknowledgement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	s := &appServerSession{ctx: ctx, cancel: cancel, stdin: w, events: make(chan core.Event, 4), preambleSent: true}
	s.threadID.Store("parent")
	s.alive.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(r)
		if !scanner.Scan() {
			return
		}
		var request map[string]any
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return
		}
		s.handleNotification("turn/started", json.RawMessage(`{"threadId":"parent","turn":{"id":"fast"}}`))
		s.handleNotification("turn/completed", json.RawMessage(`{"threadId":"parent","turn":{"id":"fast","status":"failed"}}`))
		s.handleResponse(rpcResponseEnvelope{ID: request["id"], Result: json.RawMessage(`{"turn":{"id":"fast"}}`)})
	}()
	if err := s.Send("read-only check", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	<-done
	s.stateMu.Lock()
	active := s.currentTurn
	s.stateMu.Unlock()
	if active != "" || len(s.events) != 1 {
		t.Fatal("failed turn resurrected by delayed start acknowledgement")
	}
	if ev := <-s.events; ev.Type != core.EventError {
		t.Fatalf("failure became empty success: %+v", ev)
	}
}
