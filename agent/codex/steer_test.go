package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestAppServerSteer_ActiveTurnAcknowledgementAndRejection(t *testing.T) {
	for _, result := range []string{"accepted", "rejected", "wrong-turn", "disconnect"} {
		t.Run(result, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r, w := io.Pipe()
			defer r.Close()
			defer w.Close()
			s := &appServerSession{ctx: ctx, cancel: cancel, stdin: w, events: make(chan core.Event, 8), currentTurn: "turn-1"}
			s.alive.Store(true)
			s.threadID.Store("thread-1")
			requests := make(chan map[string]any, 1)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				scanner := bufio.NewScanner(r)
				if !scanner.Scan() {
					return
				}
				var request map[string]any
				if json.Unmarshal(scanner.Bytes(), &request) != nil {
					return
				}
				requests <- request
				response := rpcResponseEnvelope{ID: request["id"], Result: json.RawMessage(`{"turnId":"turn-1"}`)}
				switch result {
				case "rejected":
					response.Error = &rpcError{Code: -32602, Message: "no active turn"}
				case "wrong-turn":
					response.Result = json.RawMessage(`{"turnId":"turn-other"}`)
				case "disconnect":
					cancel()
					return
				}
				s.handleResponse(response)
			}()
			accepted, err := s.Steer("Use the updated requirements", "message-2", nil, nil)
			wg.Wait()
			request := <-requests
			if request["method"] != "turn/steer" {
				t.Fatalf("method: %v", request)
			}
			params := request["params"].(map[string]any)
			if params["expectedTurnId"] != "turn-1" || params["threadId"] != "thread-1" {
				t.Fatalf("target: %v", params)
			}
			if _, exists := params["model"]; exists {
				t.Fatal("steer must not override model")
			}
			if result == "accepted" && (!accepted || err != nil) {
				t.Fatalf("accepted=%v err=%v", accepted, err)
			}
			if result == "rejected" && (accepted || err != nil) {
				t.Fatalf("rejection must be queueable: %v %v", accepted, err)
			}
			if (result == "wrong-turn" || result == "disconnect") && (accepted || err == nil) {
				t.Fatalf("uncertain delivery must not queue: %v %v", accepted, err)
			}
			if len(s.events) != 0 {
				t.Fatal("steer must not emit another turn result")
			}
		})
	}
}

func TestAppServerSteer_IdleTurnDoesNotWrite(t *testing.T) {
	w := &lockedWriteCloser{}
	s := &appServerSession{stdin: w}
	s.alive.Store(true)
	accepted, err := s.Steer("next message", "next", nil, nil)
	if accepted || err != nil {
		t.Fatalf("idle result: %v %v", accepted, err)
	}
}

func TestAppServerSteer_CompletedTurnCannotBeSteered(t *testing.T) {
	s := &appServerSession{currentTurn: "finished", events: make(chan core.Event, 2)}
	s.alive.Store(true)
	s.completeTurn()
	accepted, err := s.Steer("late correction", "late", nil, nil)
	if accepted || err != nil {
		t.Fatalf("completed turn result: %v %v", accepted, err)
	}
	if s.completedTurn != "finished" {
		t.Fatal("completion identity not retained")
	}
}

func TestAppServerSteer_DefaultTransportMatchesStdioClient(t *testing.T) {
	if got := normalizeAppServerURL(""); got != "stdio://" {
		t.Fatalf("default transport = %q", got)
	}
}

func TestAppServerSteer_FastCompletionBeforeStartAcknowledgement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	s := &appServerSession{ctx: ctx, cancel: cancel, stdin: w, events: make(chan core.Event, 4), preambleSent: true}
	s.alive.Store(true)
	s.threadID.Store("thread-1")
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
		s.stateMu.Lock()
		s.currentTurn = "fast-turn"
		s.stateMu.Unlock()
		s.completeTurn()
		s.handleResponse(rpcResponseEnvelope{ID: request["id"], Result: json.RawMessage(`{"turn":{"id":"fast-turn"}}`)})
	}()
	if err := s.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	<-done
	s.stateMu.Lock()
	active := s.currentTurn
	s.stateMu.Unlock()
	if active != "" {
		t.Fatalf("completed turn resurrected by delayed start acknowledgement: %q", active)
	}
}
