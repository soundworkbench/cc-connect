package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// File gates keep the fake CLI from reading stdin, then hold its first result
// until the test has checked that the busy message is still queued.
func TestSteerFirstWriteHelperProcess(t *testing.T) {
	if os.Getenv("CC_STEER_FIRST_WRITE_HELPER") != "1" {
		return
	}
	var paths []string
	for i, arg := range os.Args {
		if arg == "--" {
			paths = os.Args[i+1 : i+4]
			break
		}
	}
	wait := func(path string) {
		for {
			if _, err := os.Stat(path); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait(paths[0])
	record, err := os.Create(paths[2])
	if err != nil {
		os.Exit(2)
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for n := 0; scanner.Scan(); n++ {
		var input map[string]any
		if json.Unmarshal(scanner.Bytes(), &input) != nil {
			os.Exit(3)
		}
		if _, err := record.Write(append(scanner.Bytes(), '\n')); err != nil {
			os.Exit(4)
		}
		if n == 0 {
			wait(paths[1])
		}
		result := "first-write-complete"
		prompt := input["message"].(map[string]any)["content"].(string)
		for _, marker := range []string{"queued-follow-up-once", "drain-check"} {
			if strings.Contains(prompt, marker) {
				result = marker
			}
		}
		output, _ := json.Marshal(map[string]any{"type": "result", "result": result,
			"session_id": "resume-first-write", "user_message_uuid": input["uuid"]})
		if _, err := os.Stdout.Write(append(output, '\n')); err != nil {
			os.Exit(5)
		}
	}
	_ = record.Close()
	os.Exit(0)
}

type steerFirstWriteAgent struct {
	core.Agent
	created chan *claudeSession
}

func (a *steerFirstWriteAgent) StartSession(ctx context.Context, id string) (core.AgentSession, error) {
	as, err := a.Agent.StartSession(ctx, id)
	if err == nil {
		cs := as.(*claudeSession)
		cs.gracefulStopTimeout = 20 * time.Millisecond
		a.created <- cs
	}
	return as, err
}

func (*steerFirstWriteAgent) ValidateSessionID(context.Context, string) bool { return true }

func waitSteerFirstWrite(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestSteerAsync_FirstWriteBusyReturnsImmediately(t *testing.T) {
	cs := firstWriteSession(t, firstWriteAgent(t, "blocked", "6s"))
	sent := make(chan error, 1)
	go func() { sent <- cs.Send(strings.Repeat("x", 256<<10), "first", nil, nil) }()
	waitSteerFirstWrite(t, "first stdin write", func() bool {
		if !cs.stdinMu.TryLock() {
			return true
		}
		cs.stdinMu.Unlock()
		return false
	})
	returned := make(chan struct{})
	go func() {
		receipt, err := cs.SteerAsync("follow-up", "second", nil, nil)
		if receipt != nil || err != nil {
			t.Errorf("busy steering must remain unaccepted: receipt=%v err=%v", receipt, err)
		}
		close(returned)
	}()
	// The startup timeout bounds cleanup, including on the pre-fix code.
	t.Cleanup(func() { _ = cs.Close(); <-sent; <-returned })
	select {
	case <-returned:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SteerAsync waited for inputMu during the first stdin write")
	}
}

func TestEngine_FirstWriteBusyQueuesFollowUpExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	readGate, resultGate, transcript := filepath.Join(dir, "read"), filepath.Join(dir, "result"), filepath.Join(dir, "frames.jsonl")
	ag, err := New(map[string]any{
		"work_dir": dir, "cc_data_dir": t.TempDir(), "first_write_timeout": "6s",
		"cmd": []string{os.Args[0], "-test.run=^TestSteerFirstWriteHelperProcess$", "--", readGate, resultGate, transcript},
		"env": map[string]string{"CC_STEER_FIRST_WRITE_HELPER": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &steerFirstWriteAgent{Agent: ag, created: make(chan *claudeSession, 4)}
	p := &firstWritePlatform{}
	e := core.NewEngine("steer-first-write", a, []core.Platform{p}, filepath.Join(dir, "sessions.json"), core.LangEnglish)
	// Durable queueing is the deployment path and drains after the first result.
	e.SetDataDir(t.TempDir())
	returned := make(chan struct{})
	t.Cleanup(func() {
		_ = os.WriteFile(readGate, nil, 0600)
		_ = os.WriteFile(resultGate, nil, 0600)
		e.Stop()
	})
	send := func(id, prompt string) {
		e.ReceiveMessage(p, &core.Message{Platform: "test", SessionKey: "test:slow", UserID: "user",
			MessageID: id, Content: prompt, ReplyCtx: "reply"})
	}
	send("first", strings.Repeat("x", 256<<10))
	var cs *claudeSession
	select {
	case cs = <-a.created:
	case <-time.After(3 * time.Second):
		t.Fatal("agent session not created")
	}
	waitSteerFirstWrite(t, "first stdin write", func() bool {
		if !cs.stdinMu.TryLock() {
			return true
		}
		cs.stdinMu.Unlock()
		return false
	})
	go func() { send("second", "queued-follow-up-once"); close(returned) }()
	select {
	case <-returned:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ReceiveMessage held engine locks while waiting for the first stdin write")
	}
	if !p.received("Message received") {
		t.Fatal("busy follow-up did not take the existing queue path")
	}
	if err := os.WriteFile(readGate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitSteerFirstWrite(t, "complete first frame", func() bool {
		data, _ := os.ReadFile(transcript)
		return strings.Count(string(data), "\n") == 1
	})
	if p.received("queued-follow-up-once") {
		t.Fatal("queued input delivered before the first result")
	}
	if err := os.WriteFile(resultGate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitSteerFirstWrite(t, "queued reply on platform", func() bool { return p.received("queued-follow-up-once") })
	send("third", "drain-check")
	waitSteerFirstWrite(t, "drain reply on platform", func() bool { return p.received("drain-check") })
	e.Stop()
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	frames := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(frames) != 3 || strings.Count(string(data), "queued-follow-up-once") != 1 || strings.Count(string(data), "drain-check") != 1 {
		t.Fatalf("expected first, queued follow-up once, and drain check; got %d frames", len(frames))
	}
}
