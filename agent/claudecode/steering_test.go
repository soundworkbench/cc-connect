package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"github.com/chenhg5/cc-connect/core"
	"os"
	"testing"
	"time"
)

func steerFixture(t *testing.T) (*claudeSession, <-chan map[string]any) {
	t.Helper()
	r, w, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cs := &claudeSession{stdin: w, ctx: ctx, events: make(chan core.Event, 32)}
	cs.alive.Store(true)
	cs.sessionID.Store("test")
	lines := make(chan map[string]any, 10)
	go func() {
		defer close(lines)
		s := bufio.NewScanner(r)
		for s.Scan() {
			var m map[string]any
			if json.Unmarshal(s.Bytes(), &m) == nil {
				lines <- m
			}
		}
	}()
	t.Cleanup(func() { cancel(); w.Close(); r.Close() })
	return cs, lines
}
func takeInput(t *testing.T, ch <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(time.Second):
		t.Fatal("input not written")
		return nil
	}
}
func ack(cs *claudeSession, m map[string]any) {
	cs.handleUser(map[string]any{"isReplay": true, "uuid": m["uuid"], "message": m["message"]})
}
func finish(cs *claudeSession, ids ...any) {
	cs.handleResult(map[string]any{"type": "result", "result": "done", "user_message_uuids": ids})
}
func TestClaudeSteer_ReplayAndBoundaryRace(t *testing.T) {
	cs, ch := steerFixture(t)
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	first := takeInput(t, ch)
	ack(cs, first)
	done := make(chan error, 1)
	go func() {
		ok, err := cs.Steer("second", "m2", nil, nil)
		if !ok && err == nil {
			err = errors.New("not accepted")
		}
		done <- err
	}()
	second := takeInput(t, ch)
	// Original native result must not finish Engine before late input is consumed.
	finish(cs, first["uuid"])
	if e := <-cs.events; e.Done {
		t.Fatal("early terminal result")
	}
	ack(cs, second)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	finish(cs, second["uuid"])
	if e := <-cs.events; !e.Done {
		t.Fatal("missing final result")
	}
	if ok, err := cs.Steer("after", "m3", nil, nil); ok || err != nil {
		t.Fatal("idle steering admitted")
	}
}
func TestClaudeSteer_CoalescedResultAcknowledgesWithoutReplay(t *testing.T) {
	cs, ch := steerFixture(t)
	cs.Send("first", "", nil, nil)
	a := takeInput(t, ch)
	result := make(chan bool, 1)
	go func() { ok, e := cs.Steer("second", "", nil, nil); result <- ok && e == nil }()
	b := takeInput(t, ch)
	finish(cs, a["uuid"], b["uuid"])
	if !<-result {
		t.Fatal("result association must acknowledge")
	}
	if e := <-cs.events; !e.Done {
		t.Fatal("coalesced result not terminal")
	}
}
func TestClaudeSteer_CompactionAndAttachmentFallback(t *testing.T) {
	cs, ch := steerFixture(t)
	cs.Send("first", "", nil, nil)
	a := takeInput(t, ch)
	cs.handleResult(map[string]any{"subtype": "compact"})
	if (<-cs.events).Done {
		t.Fatal("compaction terminal")
	}
	if ok, err := cs.Steer("image", "", []core.ImageAttachment{{Data: []byte("x")}}, nil); ok || err != nil {
		t.Fatal("attachment should queue")
	}
	finish(cs, a["uuid"])
	if !(<-cs.events).Done {
		t.Fatal("final missing")
	}
}
func TestClaudeSteer_TimeoutDoesNotReplayOrFinishEarly(t *testing.T) {
	cs, ch := steerFixture(t)
	cs.Send("first", "", nil, nil)
	a := takeInput(t, ch)
	done := make(chan error, 1)
	go func() { _, e := cs.Steer("second", "", nil, nil); done <- e }()
	b := takeInput(t, ch)
	if err := <-done; err == nil {
		t.Fatal("missing acknowledgement must be uncertain")
	}
	select {
	case <-ch:
		t.Fatal("input replayed")
	default:
	}
	finish(cs, a["uuid"])
	if (<-cs.events).Done {
		t.Fatal("lost uncertain input")
	}
	ack(cs, b)
	finish(cs, b["uuid"])
	if !(<-cs.events).Done {
		t.Fatal("late result not terminal")
	}
}
func TestClaudeSteer_SubagentReplayCannotAcknowledge(t *testing.T) {
	cs, ch := steerFixture(t)
	cs.Send("first", "", nil, nil)
	a := takeInput(t, ch)
	cs.handleUser(map[string]any{"isReplay": true, "uuid": a["uuid"], "parent_tool_use_id": "child", "message": a["message"]})
	cs.inputMu.Lock()
	replayed := cs.inputs[a["uuid"].(string)].replayed
	cs.inputMu.Unlock()
	if replayed {
		t.Fatal("child ack accepted")
	}
}

type failingSteerPipe struct {
	partial      bool
	deadlineFail bool
}

func (p failingSteerPipe) Write(b []byte) (int, error) {
	if p.partial {
		return 1, errors.New("write failed")
	}
	return 0, errors.New("write failed")
}
func (p failingSteerPipe) Close() error { return nil }
func (p failingSteerPipe) SetWriteDeadline(time.Time) error {
	if p.deadlineFail {
		return errors.New("unsupported")
	}
	return nil
}
func TestClaudeSteer_WriteFailureAdmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pipe      failingSteerPipe
		uncertain bool
	}{
		{"zero", failingSteerPipe{}, false}, {"partial", failingSteerPipe{partial: true}, true}, {"deadline", failingSteerPipe{deadlineFail: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, ch := steerFixture(t)
			if err := cs.Send("first", "", nil, nil); err != nil {
				t.Fatal(err)
			}
			a := takeInput(t, ch)
			cs.stdin = tc.pipe
			ok, err := cs.Steer("second", "", nil, nil)
			if ok || (err != nil) != tc.uncertain {
				t.Fatal("incorrect delivery certainty")
			}
			finish(cs, a["uuid"])
			if (<-cs.events).Done == tc.uncertain {
				t.Fatal("incorrect completion after write failure")
			}
		})
	}
}
func TestClaudeSteer_StoppedQueues(t *testing.T) {
	cs, ch := steerFixture(t)
	cs.Send("first", "", nil, nil)
	takeInput(t, ch)
	cs.alive.Store(false)
	if ok, err := cs.Steer("second", "", nil, nil); ok || err != nil {
		t.Fatal("stopped process admitted input")
	}
	select {
	case <-ch:
		t.Fatal("unexpected write")
	default:
	}
}
