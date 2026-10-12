package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestFirstWriteHelperProcess(t *testing.T) {
	if os.Getenv("CC_FIRST_WRITE_HELPER") != "1" {
		return
	}
	mode := ""
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	switch mode {
	case "slow":
		time.Sleep(3500 * time.Millisecond)
	case "blocked":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "half-close":
		_, _ = io.CopyN(io.Discard, os.Stdin, 4096)
		_ = os.Stdin.Close()
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	resumedID := ""
	for i, arg := range os.Args {
		if arg == "--resume" && i+1 < len(os.Args) {
			resumedID = os.Args[i+1]
		}
	}
	for scanner.Scan() {
		var input map[string]any
		if json.Unmarshal(scanner.Bytes(), &input) != nil {
			os.Exit(2)
		}
		result := "received"
		if mode == "recover" && resumedID != "" {
			result = "recovered native resume=" + resumedID + " pid=" + strconv.Itoa(os.Getpid())
		}
		output, _ := json.Marshal(map[string]any{"type": "result", "result": result, "session_id": "resume-first-write", "user_message_uuid": input["uuid"]})
		_, _ = os.Stdout.Write(append(output, '\n'))
		if mode == "recover" && resumedID == "" {
			_, _ = io.CopyN(io.Discard, os.Stdin, 4096)
			_ = os.Stdin.Close()
			time.Sleep(30 * time.Second)
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func firstWriteAgent(t *testing.T, mode, timeout string) core.Agent {
	t.Helper()
	opts := map[string]any{
		"work_dir": t.TempDir(), "cc_data_dir": t.TempDir(),
		"cmd": []string{os.Args[0], "-test.run=TestFirstWriteHelperProcess", "--", mode},
		"env": map[string]string{"CC_FIRST_WRITE_HELPER": "1"},
	}
	if timeout != "" {
		opts["first_write_timeout"] = timeout
	}
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func firstWriteSession(t *testing.T, a core.Agent) *claudeSession {
	t.Helper()
	as, err := a.StartSession(context.Background(), "resume-first-write")
	if err != nil {
		t.Fatal(err)
	}
	cs := as.(*claudeSession)
	cs.gracefulStopTimeout = 20 * time.Millisecond
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestSend_FirstWriteSlowReaderUsesStartupTimeout(t *testing.T) {
	cs := firstWriteSession(t, firstWriteAgent(t, "slow", ""))
	if err := cs.Send(strings.Repeat("x", 256<<10), "m1", nil, nil); err != nil {
		t.Fatalf("slow startup must deliver complete first frame: %v", err)
	}
	select {
	case ev := <-cs.Events():
		if !ev.Done || ev.Content != "received" {
			t.Fatalf("expected valid complete JSON result, got %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("process did not parse complete frame")
	}
}

func TestSend_FirstWriteFailureInvalidatesProcess(t *testing.T) {
	for _, mode := range []string{"blocked", "half-close"} {
		t.Run(mode, func(t *testing.T) {
			cs := firstWriteSession(t, firstWriteAgent(t, mode, "80ms"))
			start := time.Now()
			err := cs.Send(strings.Repeat("x", 256<<10), "m1", nil, nil)
			if err == nil {
				t.Fatal("expected interrupted write")
			}
			if mode == "blocked" && time.Since(start) > time.Second {
				t.Errorf("configured 80ms timeout ignored: %v", time.Since(start))
			}
			if cs.Alive() {
				t.Error("failed stdin must be invalid immediately, before the next message")
			}
			if err := cs.Send("next", "m2", nil, nil); err == nil {
				t.Error("failed process reused stdin")
			}
			select {
			case <-cs.done:
			case <-time.After(time.Second):
				t.Error("failed process not killed")
			}
		})
	}
}

func TestSend_FirstWriteFastProcessUnchanged(t *testing.T) {
	cs := firstWriteSession(t, firstWriteAgent(t, "fast", "1s"))
	for _, prompt := range []string{"first", "second"} {
		if err := cs.Send(prompt, prompt, nil, nil); err != nil {
			t.Fatal(err)
		}
		select {
		case ev := <-cs.Events():
			if !ev.Done || ev.Content != "received" {
				t.Fatalf("got %+v", ev)
			}
		case <-time.After(time.Second):
			t.Fatal("fast process result missing")
		}
	}
	if !cs.Alive() {
		t.Fatal("successful writes invalidated process")
	}
}

type firstWriteRecordingPipe struct {
	writes    int
	deadlines []time.Duration
	n         int
	err       error
	fail      bool
	closed    bool
}

func (p *firstWriteRecordingPipe) Close() error { p.closed = true; return nil }
func (p *firstWriteRecordingPipe) Write(b []byte) (int, error) {
	p.writes++
	if p.fail {
		return p.n, p.err
	}
	return len(b), nil
}
func (p *firstWriteRecordingPipe) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		p.deadlines = append(p.deadlines, time.Until(deadline))
	}
	return nil
}

func TestSend_FirstWriteDefaultAndLaterDeadline(t *testing.T) {
	cs, _ := steerFixture(t)
	p := &firstWriteRecordingPipe{}
	cs.stdin = p
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := cs.Send("second", "m2", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.deadlines) != 2 || p.deadlines[0] < 59*time.Second || p.deadlines[0] > 60*time.Second || p.deadlines[1] < 2*time.Second || p.deadlines[1] > 3*time.Second {
		t.Fatalf("want startup 60s and later 3s, got %v", p.deadlines)
	}
}

func TestSend_ControlResponseDoesNotConsumeFirstUserDeadline(t *testing.T) {
	cs, _ := steerFixture(t)
	p := &firstWriteRecordingPipe{}
	cs.stdin = p
	if err := cs.RespondPermission("request", core.PermissionResult{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.deadlines) != 2 || p.deadlines[0] > 3*time.Second || p.deadlines[1] < 59*time.Second {
		t.Fatalf("control response consumed startup deadline: %v", p.deadlines)
	}
}

func TestInputWriteFailure_ZeroShortAndControlInvalidateStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		n       int
		err     error
		control bool
	}{
		{"zero", 0, io.ErrClosedPipe, false}, {"short_nil_error", 7, nil, false}, {"control_partial", 7, io.ErrClosedPipe, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, _ := steerFixture(t)
			p := &firstWriteRecordingPipe{fail: true, n: tc.n, err: tc.err}
			cs.stdin = p
			var err error
			if tc.control {
				err = cs.RespondPermission("request", core.PermissionResult{Behavior: "allow"})
			} else {
				err = cs.Send("first", "m1", nil, nil)
			}
			var failure *core.InputWriteError
			if !errors.As(err, &failure) || failure.WrittenBytes != tc.n || failure.TotalBytes <= tc.n {
				t.Fatalf("missing transport counts: %v", err)
			}
			if cs.InputFailure() != failure {
				t.Fatal("write failure must remain observable after events close")
			}
			if cs.Alive() || !p.closed {
				t.Fatal("write failure did not invalidate stream")
			}
			if err = cs.Send("next", "m2", nil, nil); err == nil || p.writes != 1 {
				t.Fatal("failed stream reused")
			}
		})
	}
}

func TestNew_FirstWriteTimeoutRejectsInvalidConfiguration(t *testing.T) {
	for _, raw := range []any{"", "0s", "-1s", "invalid", 60} {
		if _, err := New(map[string]any{"work_dir": t.TempDir(), "cmd": os.Args[0], "first_write_timeout": raw}); err == nil {
			t.Fatalf("accepted invalid timeout %v", raw)
		}
	}
}

func TestBuildUserInput_ResumeDoesNotInjectHistoryIntoShortText(t *testing.T) {
	cs := &claudeSession{workDir: t.TempDir()}
	cs.alive.Store(true)
	cs.sessionID.Store("resume-first-write")
	prompt := strings.Repeat("x", 39)
	input, err := cs.buildUserInput(prompt, "m1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	input["uuid"] = "00000000-0000-4000-8000-000000000000"
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= 1024 || input["message"].(map[string]any)["content"] != prompt {
		t.Fatal("short text unexpectedly expanded")
	}
	t.Logf("plain content bytes=%d stream-json bytes=%d", len(prompt), len(data)+1)
	imageInput, err := cs.buildUserInput(prompt, "m2", []core.ImageAttachment{{Data: make([]byte, 49152), MimeType: "image/png"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	imageData, err := json.Marshal(imageInput)
	if err != nil {
		t.Fatal(err)
	}
	if len(imageData) >= 65536 || strings.Contains(string(imageData), "base64") {
		t.Fatal("large image should use the existing path fallback")
	}
	t.Logf("large image bytes=49152 path-only stream-json bytes=%d", len(imageData)+1)
}

type firstWritePlatform struct {
	mu      sync.Mutex
	replies []string
}

func (*firstWritePlatform) Name() string                    { return "test" }
func (*firstWritePlatform) Start(core.MessageHandler) error { return nil }
func (*firstWritePlatform) Stop() error                     { return nil }
func (p *firstWritePlatform) Reply(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.replies = append(p.replies, content)
	return nil
}
func (p *firstWritePlatform) Send(ctx context.Context, replyCtx any, content string) error {
	return p.Reply(ctx, replyCtx, content)
}
func (p *firstWritePlatform) received(text string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Contains(strings.Join(p.replies, "\n"), text)
}

func TestEngine_InputWriteFailureRespawnsNativeProcessWithResume(t *testing.T) {
	a := &firstWriteProcessAgent{firstWriteAgent(t, "recover", "1s")}
	p := &firstWritePlatform{}
	e := core.NewEngine("firstwrite", a, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	defer e.Stop()
	send := func(id, content string) {
		e.ReceiveMessage(p, &core.Message{Platform: "test", SessionKey: "test:retry", UserID: "retry", MessageID: id, Content: content, ReplyCtx: "reply"})
	}
	wait := func(text string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if p.received(text) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("platform did not receive %q", text)
	}
	send("m1", "hello")
	wait("received")
	send("m2", strings.Repeat("x", 256<<10))
	wait("Please resend")
	send("m3", "retry")
	wait("recovered native resume=resume-first-write pid=")
}

// The fake CLI has no external transcript store. Keep that filesystem boundary
// synthetic while StartSession, Send and process teardown use the real adapter.
type firstWriteProcessAgent struct{ core.Agent }

func (*firstWriteProcessAgent) ValidateSessionID(_ context.Context, id string) bool {
	return id == "resume-first-write"
}
