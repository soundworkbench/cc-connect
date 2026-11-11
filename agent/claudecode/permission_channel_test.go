package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Frames below follow what Claude Code writes in stream-json mode. The tool
// result text is the CLI's own wording when a can_use_tool request cannot be
// sent because its stdin reader has ended.
const streamClosedToolResult = "Tool permission request failed: AbortError: Stream closed"

func helperLine(v map[string]any) {
	b, _ := json.Marshal(v)
	_, _ = os.Stdout.Write(append(b, '\n'))
}

func toolResultFrame(text string) map[string]any {
	return map[string]any{
		"type": "user", "session_id": "perm-channel", "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_1", "is_error": true, "content": text,
		}}},
	}
}

func taskFrame(subtype string, extra map[string]any) map[string]any {
	m := map[string]any{"type": "system", "subtype": subtype, "task_id": "task-1", "session_id": "perm-channel"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// TestPermissionChannelHelperProcess stands in for the Claude Code CLI.
//
//	background: the turn starts a background task and ends; the task keeps
//	  working. If stdin reaches EOF while it runs, the task's next tool call
//	  fails the way the real CLI fails it, and the helper leaves a marker.
//	lost: the CLI reports a tool call whose permission request failed even
//	  though cc-connect never closed stdin.
func TestPermissionChannelHelperProcess(t *testing.T) {
	if os.Getenv("CC_PERM_CHANNEL_HELPER") != "1" {
		return
	}
	mode := ""
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	lines := make(chan map[string]any)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 4096), 2<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				lines <- m
			}
		}
		close(lines)
	}()
	first, ok := <-lines
	if !ok {
		os.Exit(0)
	}
	switch mode {
	case "background":
		helperLine(taskFrame("task_started", map[string]any{"description": "后台跑测试", "task_type": "local_agent", "is_backgrounded": true}))
		helperLine(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "已在后台开始", "session_id": "perm-channel", "user_message_uuid": first["uuid"]})
		for range lines {
		}
		_ = os.WriteFile(os.Getenv("CC_PERM_CHANNEL_MARKER"), []byte("stdin closed while a task was running"), 0o644)
		helperLine(toolResultFrame(streamClosedToolResult))
		time.Sleep(30 * time.Second)
	case "lost":
		helperLine(map[string]any{"type": "assistant", "session_id": "perm-channel", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
		}}})
		helperLine(toolResultFrame(streamClosedToolResult))
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

func permChannelSession(t *testing.T, mode, marker string) *claudeSession {
	t.Helper()
	a, err := New(map[string]any{
		"work_dir": t.TempDir(), "cc_data_dir": t.TempDir(),
		"cmd": []string{os.Args[0], "-test.run=TestPermissionChannelHelperProcess", "--", mode},
		"env": map[string]string{"CC_PERM_CHANNEL_HELPER": "1", "CC_PERM_CHANNEL_MARKER": marker},
	})
	if err != nil {
		t.Fatal(err)
	}
	as, err := a.StartSession(context.Background(), "perm-channel")
	if err != nil {
		t.Fatal(err)
	}
	cs := as.(*claudeSession)
	cs.gracefulStopTimeout = 20 * time.Millisecond
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// Regression: a cron run, a relay, an idle reclaim or /new closed the CLI's
// stdin right after the turn's result while a background task was still
// running. Every later tool call of that task failed with "Stream closed"
// until the process was killed.
func TestClose_BusySessionStopsWithoutStdinEOF(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stdin-eof")
	cs := permChannelSession(t, "background", marker)
	if err := cs.Send("开始", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-cs.Events():
			done = ev.Type == core.EventResult && ev.Done
		case <-deadline:
			t.Fatal("turn result not received")
		}
	}
	if !cs.HasBackgroundWork() {
		t.Fatal("a started, unfinished task must count as background work")
	}
	start := time.Now()
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Close sent stdin EOF to a CLI with a running task; its tool calls lose the permission channel")
	}
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Fatalf("busy Close took %v; it must stop the process at once", elapsed)
	}
}

func TestPermissionChannelLost_StopsProcessForRestart(t *testing.T) {
	cs := permChannelSession(t, "lost", "")
	if err := cs.Send("列一下文件", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case ev, ok := <-cs.Events():
			if !ok {
				open = false
				break
			}
			if ev.Type == core.EventToolResult && strings.Contains(ev.ToolResult, "Stream closed") {
				t.Fatalf("the CLI's English failure must not reach the chat: %+v", ev)
			}
		case <-deadline:
			t.Fatal("process was not stopped after losing its permission channel")
		}
	}
	failure := cs.InputFailure()
	if failure == nil || !errors.Is(failure, core.ErrPermissionChannelClosed) {
		t.Fatalf("InputFailure = %v, want ErrPermissionChannelClosed", failure)
	}
	if cs.Alive() {
		t.Fatal("session must report dead so the engine restarts it")
	}
}

func TestTrackTask_CountsVisibleTasksUntilTheyEnd(t *testing.T) {
	cs := &claudeSession{ctx: context.Background(), events: make(chan core.Event, 16)}
	cs.alive.Store(true)
	line := func(v map[string]any) {
		b, _ := json.Marshal(v)
		cs.handleReadLoopLine(string(b))
	}
	line(map[string]any{"type": "system", "subtype": "task_started", "task_id": "ambient-1", "ambient": true})
	if cs.HasBackgroundWork() {
		t.Fatal("an ambient task is CLI bookkeeping, not background work")
	}
	line(taskFrame("task_started", map[string]any{"is_backgrounded": true}))
	line(taskFrame("task_started", map[string]any{"is_backgrounded": true})) // repeated frame
	if !cs.HasBackgroundWork() || !cs.busy() {
		t.Fatal("a running task must count as background work")
	}
	line(taskFrame("task_notification", map[string]any{"status": "completed"}))
	if cs.liveTaskCount.Load() != 0 {
		t.Fatalf("task ended but %d still counted", cs.liveTaskCount.Load())
	}
	if !cs.HasBackgroundWork() {
		t.Fatal("right after the last task ends the CLI may still start the turn that reports it")
	}
	// The CLI reports the task's result in a turn of its own.
	line(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{}}})
	if !cs.busy() || !cs.HasBackgroundWork() {
		t.Fatal("a turn the CLI is writing must count as busy and as background work")
	}
	line(map[string]any{"type": "result", "subtype": "success", "result": "ok"})
	if cs.busy() || cs.HasBackgroundWork() {
		t.Fatal("the turn's result must end busy and background work")
	}
}

func TestHasBackgroundWork_SettleGraceExpires(t *testing.T) {
	old := taskSettleGrace
	taskSettleGrace = 20 * time.Millisecond
	defer func() { taskSettleGrace = old }()
	cs := &claudeSession{ctx: context.Background(), events: make(chan core.Event, 16)}
	cs.alive.Store(true)
	cs.trackTask(&core.TaskEvent{Kind: core.TaskStarted, TaskID: "t1"})
	cs.trackTask(&core.TaskEvent{Kind: core.TaskEnded, TaskID: "t1"})
	time.Sleep(40 * time.Millisecond)
	if cs.HasBackgroundWork() {
		t.Fatal("no turn followed the task's end; the work is over")
	}
}

func TestPermissionChannelFailure_DuringCloseIsExpected(t *testing.T) {
	cs := &claudeSession{ctx: context.Background(), events: make(chan core.Event, 4)}
	cs.alive.Store(true)
	cs.closing.Store(true)
	b, _ := json.Marshal(toolResultFrame(streamClosedToolResult))
	cs.handleReadLoopLine(string(b))
	if cs.InputFailure() != nil {
		t.Fatal("a failure caused by cc-connect's own teardown must not be reported as a lost channel")
	}
	if !isPermissionChannelFailure("Tool permission request failed: AbortError: Tool permission stream closed before response received") {
		t.Fatal("pending-request variant not recognized")
	}
	if isPermissionChannelFailure("Exit code 1\nls: cannot access") {
		t.Fatal("an ordinary tool error is not a permission channel failure")
	}
}
