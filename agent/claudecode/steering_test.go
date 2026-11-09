package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"github.com/chenhg5/cc-connect/core"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
func TestClaudeSteer_Compaction(t *testing.T) {
	cs, ch := steerFixture(t)
	cs.Send("first", "", nil, nil)
	a := takeInput(t, ch)
	cs.handleResult(map[string]any{"subtype": "compact"})
	if (<-cs.events).Done {
		t.Fatal("compaction terminal")
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
			expected := core.ErrSteerWriteUncertain
			if tc.pipe.deadlineFail {
				expected = core.ErrSteerNotWritten
			}
			if ok || !errors.Is(err, expected) {
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
func TestClaudeSteer_PartialWriteInvalidatesStream(t *testing.T) {
	cs, ch := steerFixture(t)
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	takeInput(t, ch)
	cs.stdin = &shortOncePipe{File: cs.stdin.(*os.File)}
	receipt, err := cs.SteerAsync("supplement", "m2", nil, nil)
	if receipt != nil || !errors.Is(err, core.ErrSteerWriteUncertain) {
		t.Fatalf("receipt=%v error=%v", receipt, err)
	}
	if cs.Alive() {
		t.Fatal("partial frame must invalidate the process")
	}
	if err := cs.Send("supplement", "m2", nil, nil); err == nil {
		t.Fatal("partial frame must never be continued on the same stream")
	}
	for frame := range ch {
		t.Fatalf("unexpected complete frame: %v", frame["type"])
	}
}

func TestClaudeSteer_InlineImagesAndTextOnce(t *testing.T) {
	cs, ch := steerFixture(t)
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	first := takeInput(t, ch)
	receipt, err := cs.SteerAsync("look at this", "m2", []core.ImageAttachment{{Data: []byte("png-data")}, {Data: []byte("jpeg-data"), MimeType: "image/jpeg"}}, nil)
	if err != nil || receipt == nil {
		t.Fatalf("image steer: %v %v", receipt, err)
	}
	frame := takeInput(t, ch)
	parts := frame["message"].(map[string]any)["content"].([]any)
	for i, want := range []string{"cG5nLWRhdGE=", "anBlZy1kYXRh"} {
		part := parts[i].(map[string]any)
		source := part["source"].(map[string]any)
		mime := []string{"image/png", "image/jpeg"}[i]
		if part["type"] != "image" || source["type"] != "base64" || source["media_type"] != mime || source["data"] != want {
			t.Fatalf("image[%d]=%v", i, part)
		}
	}
	if parts[2].(map[string]any)["text"] != "look at this" {
		t.Fatal(parts)
	}
	if frame["uuid"] == first["uuid"] {
		t.Fatal("reused UUID")
	}
	finish(cs, first["uuid"])
	if ev := <-cs.events; ev.Done {
		t.Fatal("image lost at native result boundary")
	}
	ack(cs, frame)
	select {
	case <-receipt.Ack:
	default:
		t.Fatal("image replay not acknowledged")
	}
	finish(cs, frame["uuid"])
	if ev := <-cs.events; !ev.Done {
		t.Fatal("image did not finish")
	}
	if len(ch) != 0 {
		t.Fatal("duplicate image frame")
	}
}

func TestClaudeSteer_PartialImageWriteInvalidatesStream(t *testing.T) {
	cs, ch := steerFixture(t)
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	takeInput(t, ch)
	cs.stdin = &shortOncePipe{File: cs.stdin.(*os.File)}
	images := []core.ImageAttachment{{Data: []byte("png-data"), MimeType: "image/png"}}
	receipt, err := cs.SteerAsync("picture", "m2", images, nil)
	if receipt != nil || !errors.Is(err, core.ErrSteerWriteUncertain) {
		t.Fatalf("receipt=%v error=%v", receipt, err)
	}
	if cs.Alive() {
		t.Fatal("partial image frame must invalidate the process")
	}
	if err := cs.Send("picture", "m2", images, nil); err == nil {
		t.Fatal("partial image must not reuse stdin")
	}
	for frame := range ch {
		t.Fatalf("unexpected complete frame: %v", frame["type"])
	}
}

func TestClaudeSteer_FilesAndLargeImagesInjectPaths(t *testing.T) {
	cs, ch := steerFixture(t)
	cs.workDir = t.TempDir()
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	takeInput(t, ch)
	big := make([]byte, claudeInlineImageLimit)
	receipt, err := cs.SteerAsync("see these", "m2", []core.ImageAttachment{{Data: big, MimeType: "image/png"}}, []core.FileAttachment{{FileName: "a.txt", Data: []byte("hello")}})
	if err != nil || receipt == nil {
		t.Fatalf("steer: %v %v", receipt, err)
	}
	frame := takeInput(t, ch)
	text, ok := frame["message"].(map[string]any)["content"].(string)
	if !ok || len(text) > 4096 {
		t.Fatalf("expected small text frame, got %T len=%d", frame["message"].(map[string]any)["content"], len(text))
	}
	if !strings.Contains(text, "see these") || !strings.Contains(text, filepath.Join(cs.workDir, ".cc-connect", "attachments")) || !strings.Contains(text, "a.txt") {
		t.Fatalf("frame=%q", text)
	}
	for _, f := range regexp.MustCompile(regexp.QuoteMeta(cs.workDir) + `[^ ,)]+`).FindAllString(text, -1) {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("path not on disk: %v", err)
		}
	}
}
