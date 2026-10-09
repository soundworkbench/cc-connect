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
func TestClaudeSteer_LateAckAfterThreeSecondsNoReplay(t *testing.T) {
	cs, ch := steerFixture(t)
	cs.Send("first", "", nil, nil)
	a := takeInput(t, ch)
	done := make(chan error, 1)
	go func() { _, e := cs.Steer("second", "", nil, nil); done <- e }()
	b := takeInput(t, ch)
	select {
	case err := <-done:
		t.Fatalf("ack returned before consumption: %v", err)
	case <-time.After(3200 * time.Millisecond):
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
	if err := <-done; err != nil {
		t.Fatal(err)
	}
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
		name string
		pipe failingSteerPipe
	}{
		{"zero", failingSteerPipe{}}, {"partial", failingSteerPipe{partial: true}}, {"deadline", failingSteerPipe{deadlineFail: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, ch := steerFixture(t)
			if err := cs.Send("first", "", nil, nil); err != nil {
				t.Fatal(err)
			}
			a := takeInput(t, ch)
			cs.stdin = tc.pipe
			ok, err := cs.Steer("second", "", nil, nil)
			if ok || !errors.Is(err, core.ErrSteerNotWritten) {
				t.Fatal("incorrect delivery certainty")
			}
			finish(cs, a["uuid"])
			if !(<-cs.events).Done {
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

// Unlike a mock that only reports n=1, this pipe actually commits a prefix.
// Retrying a whole JSON frame would make the scanner reject the next input.
type shortOncePipe struct {
	*os.File
	failed bool
}

func (p *shortOncePipe) Write(b []byte) (int, error) {
	if !p.failed {
		p.failed = true
		n, err := p.File.Write(b[:7])
		if err != nil {
			return n, err
		}
		return n, errors.New("transient short write")
	}
	return p.File.Write(b)
}
func TestClaudeSteer_PartialWriteQueuesSameFrameExactlyOnce(t *testing.T) {
	cs, ch := steerFixture(t)
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	first := takeInput(t, ch)
	cs.stdin = &shortOncePipe{File: cs.stdin.(*os.File)}
	receipt, err := cs.SteerAsync("supplement", "m2", nil, nil)
	if receipt != nil || !errors.Is(err, core.ErrSteerNotWritten) {
		t.Fatalf("receipt=%v error=%v", receipt, err)
	}
	finish(cs, first["uuid"])
	if !(<-cs.events).Done {
		t.Fatal("incomplete frame held old turn open")
	}
	if err = cs.Send("supplement", "m2", nil, nil); err != nil {
		t.Fatal(err)
	}
	second := takeInput(t, ch)
	if second["message"].(map[string]any)["content"] != "supplement" {
		t.Fatal("frame corrupted")
	}
	finish(cs, second["uuid"])
	if !(<-cs.events).Done {
		t.Fatal("queued turn not finished")
	}
	select {
	case <-ch:
		t.Fatal("duplicate input")
	default:
	}
}
