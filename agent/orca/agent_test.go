package orca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type recordedCall struct {
	args  []string
	stdin string
}

type mockRunner struct {
	mu      sync.Mutex
	calls   []recordedCall
	handler func(args []string, stdin []byte) ([]byte, error)
}

func (m *mockRunner) Run(_ context.Context, args []string, stdin []byte) ([]byte, error) {
	m.mu.Lock()
	cp := append([]string(nil), args...)
	m.calls = append(m.calls, recordedCall{args: cp, stdin: string(stdin)})
	h := m.handler
	m.mu.Unlock()
	if h == nil {
		return []byte(`{"ok":true,"result":{}}`), nil
	}
	return h(cp, stdin)
}

func (m *mockRunner) snapshotCalls() []recordedCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]recordedCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func mustEnvelope(t *testing.T, result any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func collectTurnEvents(t *testing.T, ch <-chan core.Event, timeout time.Duration) []core.Event {
	t.Helper()
	deadline := time.After(timeout)
	var out []core.Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
			if (ev.Type == core.EventResult && ev.Done) || ev.Type == core.EventError {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out waiting for turn completion; collected %d events: %+v", len(out), out)
		}
	}
}

func newTestAgent(t *testing.T, runner cliRunner, extra map[string]any) *Agent {
	t.Helper()
	opts := map[string]any{
		"bot_id":                "bot-test-0001",
		"sender_id":             "feishu-bridge-test",
		"poll_interval_ms":      15,
		"work_poll_interval_ms": 20,
		"item_poll_interval_ms": 25,
		"turn_timeout_sec":      5,
		"cc_data_dir":           t.TempDir(),
	}
	for k, v := range extra {
		opts[k] = v
	}
	raw, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	a := raw.(*Agent)
	a.runnerOverride = runner
	t.Cleanup(func() { _ = a.Stop() })
	return a
}

func TestNewValidationAndRegistration(t *testing.T) {
	if _, err := core.CreateAgent("orca", map[string]any{}); err == nil {
		t.Fatal("expected error when bot_id is missing")
	}

	// Verify resolveDefaultOrcaBin prefers <orcaHome>/.config/orca/cli/bin/orca.
	fakeHome := t.TempDir()
	binDir := filepath.Join(fakeHome, ".config", "orca", "cli", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(binDir, "orca")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveDefaultOrcaBin(fakeHome); got != fakeBin {
		t.Fatalf("resolveDefaultOrcaBin(fakeHome) = %q, want %q", got, fakeBin)
	}

	// When orca_home is empty and no binary exists under HOME or PATH, falls back to "orca".
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("PATH", t.TempDir())
	ag, err := core.CreateAgent("orca", map[string]any{"bot_id": "bot-123"})
	if err != nil {
		t.Fatalf("CreateAgent(orca) failed: %v", err)
	}
	if ag.Name() != "orca" {
		t.Fatalf("Name() = %q, want orca", ag.Name())
	}
	oa := ag.(*Agent)
	if oa.orcaBin != "orca" || oa.orcaHome != "" || oa.senderID != defaultSenderID {
		t.Fatalf("unexpected defaults: bin=%q home=%q sender=%q", oa.orcaBin, oa.orcaHome, oa.senderID)
	}
	if oa.pollInterval != defaultPollIntervalMs*time.Millisecond || oa.turnTimeout != defaultTurnTimeoutSec*time.Second {
		t.Fatalf("unexpected timing defaults: poll=%v timeout=%v", oa.pollInterval, oa.turnTimeout)
	}
	if oa.CLIDisplayName() != "Orca Bot" {
		t.Fatalf("CLIDisplayName = %q", oa.CLIDisplayName())
	}

	// Verify buildCleanOrcaEnv strips ORCA_* and preserves HOME when orcaHome is empty.
	envKept := buildCleanOrcaEnv([]string{"HOME=/tmp/user-home", "ORCA_TOKEN=secret", "PATH=/usr/bin"}, "")
	var gotHome, gotPath string
	for _, kv := range envKept {
		if strings.HasPrefix(kv, "ORCA_") {
			t.Fatalf("ORCA_* variable leaked: %q", kv)
		}
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			gotHome = v
		}
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			gotPath = v
		}
	}
	if gotHome != "/tmp/user-home" || gotPath != "/usr/bin" {
		t.Fatalf("buildCleanOrcaEnv empty home: got HOME=%q PATH=%q", gotHome, gotPath)
	}

	// Verify non-empty orcaHome overrides HOME.
	envOverride := buildCleanOrcaEnv([]string{"HOME=/tmp/user-home", "ORCA_SESSION=abc"}, "/tmp/orca-home")
	gotHome = ""
	for _, kv := range envOverride {
		if strings.HasPrefix(kv, "ORCA_") {
			t.Fatalf("ORCA_* variable leaked: %q", kv)
		}
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			gotHome = v
		}
	}
	if gotHome != "/tmp/orca-home" {
		t.Fatalf("buildCleanOrcaEnv override home: got HOME=%q, want /tmp/orca-home", gotHome)
	}
}

func TestMultiStepToolProgressAndFinalReply(t *testing.T) {
	var readStep int
	var mu sync.Mutex

	runner := &mockRunner{
		handler: func(args []string, stdin []byte) ([]byte, error) {
			cmd := strings.Join(args[:min(2, len(args))], " ")
			switch cmd {
			case "bot chats":
				return mustEnvelope(t, map[string]any{
					"bot": "demo-bot",
					"chats": []map[string]any{
						{"id": "home-chat-1", "title": "Home", "home": true, "closed": false},
					},
				}), nil
			case "bot work":
				return mustEnvelope(t, map[string]any{"items": []any{}}), nil
			case "bot item":
				return mustEnvelope(t, map[string]any{"items": []any{}}), nil
			case "orchestration send":
				return []byte(`{"ok":true,"result":{"id":"mail-1"}}`), nil
			case "agent read":
				mu.Lock()
				readStep++
				step := readStep
				mu.Unlock()
				switch step {
				case 1: // baseline before Send
					return mustEnvelope(t, map[string]any{
						"state": "done",
						"entries": []map[string]any{
							{"kind": "message", "role": "assistant", "text": "历史旧回复"},
						},
					}), nil
				case 2: // working: first progress sentence + read_file
					return mustEnvelope(t, map[string]any{
						"state": "working",
						"entries": []map[string]any{
							{"kind": "message", "role": "assistant", "text": "历史旧回复"},
							{"kind": "message", "role": "user", "text": "帮我查构建状态"},
							{"kind": "message", "role": "assistant", "text": "我先读取构建配置。"},
							{"kind": "tool", "name": "read_file", "state": "running"},
						},
					}), nil
				case 3: // read_file completed + execute + search_web + invoke_subagent
					return mustEnvelope(t, map[string]any{
						"state": "working",
						"entries": []map[string]any{
							{"kind": "message", "role": "assistant", "text": "历史旧回复"},
							{"kind": "message", "role": "user", "text": "帮我查构建状态"},
							{"kind": "message", "role": "assistant", "text": "我先读取构建配置。"},
							{"kind": "tool", "name": "read_file", "state": "completed"},
							{"kind": "message", "role": "assistant", "text": "接下来跑一次构建检查。"},
							{"kind": "tool", "name": "execute", "state": "completed"},
							{"kind": "tool", "name": "search_web", "state": "completed"},
							{"kind": "tool", "name": "invoke_subagent", "state": "completed"},
						},
					}), nil
				default: // done with final long markdown reply
					return mustEnvelope(t, map[string]any{
						"state": "done",
						"entries": []map[string]any{
							{"kind": "message", "role": "assistant", "text": "历史旧回复"},
							{"kind": "message", "role": "user", "text": "帮我查构建状态"},
							{"kind": "message", "role": "assistant", "text": "我先读取构建配置。"},
							{"kind": "tool", "name": "read_file", "state": "completed"},
							{"kind": "message", "role": "assistant", "text": "接下来跑一次构建检查。"},
							{"kind": "tool", "name": "execute", "state": "completed"},
							{"kind": "tool", "name": "search_web", "state": "completed"},
							{"kind": "tool", "name": "invoke_subagent", "state": "completed"},
							{"kind": "message", "role": "assistant", "text": "构建检查全部通过：\n1. 编译无报错\n2. 单测全部绿灯"},
						},
					}), nil
				}
			default:
				return nil, fmt.Errorf("unexpected cmd: %v", args)
			}
		},
	}

	ag := newTestAgent(t, runner, map[string]any{"sync_needs_you": false})
	sess, err := ag.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	if sess.CurrentSessionID() != "home-chat-1" {
		t.Fatalf("CurrentSessionID = %q, want home-chat-1", sess.CurrentSessionID())
	}

	if err := sess.Send("帮我查构建状态", "om_001", nil, nil); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	events := collectTurnEvents(t, sess.Events(), 3*time.Second)

	// Verify exact sequence: text -> read_file -> text -> execute -> search_web -> invoke_subagent -> final text -> result
	wantTypes := []core.EventType{
		core.EventText,
		core.EventToolUse,
		core.EventText,
		core.EventToolUse,
		core.EventToolUse,
		core.EventToolUse,
		core.EventText,
		core.EventResult,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d:\n%+v", len(events), len(wantTypes), events)
	}
	for i, wt := range wantTypes {
		if events[i].Type != wt {
			t.Fatalf("event[%d].Type = %q, want %q (%+v)", i, events[i].Type, wt, events[i])
		}
	}

	// Verify tool Chinese labels and metadata
	wantLabels := map[int]string{
		1: "读取文件",
		3: "执行命令",
		4: "联网搜索",
		5: "启动子代理",
	}
	for idx, want := range wantLabels {
		ev := events[idx]
		if ev.Metadata["public_tool_progress"] != want || ev.Metadata["step_label"] != want {
			t.Fatalf("event[%d] metadata = %+v, want %q", idx, ev.Metadata, want)
		}
		if ev.ProgressID == "" {
			t.Fatalf("event[%d] missing ProgressID", idx)
		}
	}

	// Final EventResult must carry only the final reply (not old history or intermediate sentences).
	final := events[len(events)-1]
	if !final.Done || final.Content != "构建检查全部通过：\n1. 编译无报错\n2. 单测全部绿灯" {
		t.Fatalf("final EventResult = %+v", final)
	}

	// Ensure no feishu-reply instructions were injected into orchestration send body.
	var foundSend bool
	for _, c := range runner.snapshotCalls() {
		if len(c.args) >= 2 && c.args[0] == "orchestration" && c.args[1] == "send" {
			foundSend = true
			if strings.Contains(c.stdin, "feishu-reply") || strings.Contains(c.stdin, "placeholder_card_id") {
				t.Fatalf("unexpected legacy feishu-reply hint in stdin: %q", c.stdin)
			}
			if c.stdin != "帮我查构建状态" {
				t.Fatalf("send stdin = %q, want clean user prompt", c.stdin)
			}
		}
	}
	if !foundSend {
		t.Fatal("orchestration send was not called")
	}
}

func TestNoReplySuppression(t *testing.T) {
	var readStep int
	var mu sync.Mutex
	runner := &mockRunner{
		handler: func(args []string, _ []byte) ([]byte, error) {
			switch strings.Join(args[:min(2, len(args))], " ") {
			case "bot chats":
				return mustEnvelope(t, map[string]any{
					"chats": []map[string]any{{"id": "home-1", "home": true}},
				}), nil
			case "bot work", "bot item":
				return mustEnvelope(t, map[string]any{"items": []any{}}), nil
			case "orchestration send":
				return []byte(`{"ok":true,"result":{}}`), nil
			case "agent read":
				mu.Lock()
				readStep++
				step := readStep
				mu.Unlock()
				if step == 1 {
					return mustEnvelope(t, map[string]any{"state": "done", "entries": []any{}}), nil
				}
				return mustEnvelope(t, map[string]any{
					"state": "done",
					"entries": []map[string]any{
						{"kind": "message", "role": "user", "text": "内部心跳"},
						{"kind": "message", "role": "assistant", "text": "NO_REPLY"},
					},
				}), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}

	ag := newTestAgent(t, runner, map[string]any{"sync_needs_you": false})
	sess, err := ag.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Send("内部心跳", "om_noreply", nil, nil); err != nil {
		t.Fatal(err)
	}
	events := collectTurnEvents(t, sess.Events(), 3*time.Second)
	if len(events) != 2 || events[0].Type != core.EventText || events[0].Content != "NO_REPLY" ||
		events[1].Type != core.EventResult || !events[1].Done || events[1].Content != "NO_REPLY" {
		t.Fatalf("unexpected NO_REPLY events: %+v", events)
	}
}

func TestAttachmentsSavedInBodyForNormalAndFileFlagForItem(t *testing.T) {
	dataDir := t.TempDir()
	var sendCalls []recordedCall
	var readStep int
	var mu sync.Mutex

	runner := &mockRunner{
		handler: func(args []string, stdin []byte) ([]byte, error) {
			switch strings.Join(args[:min(2, len(args))], " ") {
			case "bot chats":
				return mustEnvelope(t, map[string]any{
					"chats": []map[string]any{{"id": "home-att", "home": true}},
				}), nil
			case "bot work", "bot item":
				return mustEnvelope(t, map[string]any{"items": []any{}}), nil
			case "orchestration send":
				mu.Lock()
				sendCalls = append(sendCalls, recordedCall{
					args:  append([]string(nil), args...),
					stdin: string(stdin),
				})
				mu.Unlock()
				return []byte(`{"ok":true,"result":{}}`), nil
			case "agent read":
				mu.Lock()
				readStep++
				step := readStep
				mu.Unlock()
				if step == 1 {
					return mustEnvelope(t, map[string]any{"state": "done", "entries": []any{}}), nil
				}
				return mustEnvelope(t, map[string]any{
					"state": "done",
					"entries": []map[string]any{
						{"kind": "message", "role": "assistant", "text": "已查看图片和 PDF。"},
					},
				}), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}

	ag := newTestAgent(t, runner, map[string]any{"cc_data_dir": dataDir, "sync_needs_you": false})
	sess, err := ag.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	pngHeader := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00}
	pdfData := []byte("%PDF-1.4 sample content")
	images := []core.ImageAttachment{
		{FileName: "../../escape/shot.png", MimeType: "image/png", Data: pngHeader},
		{FileName: "", MimeType: "image/jpeg", Data: []byte{0xff, 0xd8, 0xff, 0xe0}},
	}
	files := []core.FileAttachment{
		{FileName: "../spec.pdf", MimeType: "application/pdf", Data: pdfData},
	}

	// 1. Ordinary message (no --item): MUST NOT pass --file, MUST embed local paths in body.
	if err := sess.Send("看看这两张图和文档", "om_att_1", images, files); err != nil {
		t.Fatalf("Send with attachments failed: %v", err)
	}
	events := collectTurnEvents(t, sess.Events(), 3*time.Second)
	if len(events) == 0 || events[len(events)-1].Type != core.EventResult {
		t.Fatalf("unexpected events: %+v", events)
	}

	mu.Lock()
	normalCall := sendCalls[0]
	mu.Unlock()

	for _, a := range normalCall.args {
		if a == "--file" {
			t.Fatalf("ordinary message must not pass --file flag: %v", normalCall.args)
		}
	}

	expectedDir := filepath.Join(dataDir, "orca-attachments", "om_att_1")
	img1 := filepath.Join(expectedDir, "shot.png")
	img2 := filepath.Join(expectedDir, "image-2.jpg")
	file1 := filepath.Join(expectedDir, "spec.pdf")
	for _, p := range []string{img1, img2, file1} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("saved attachment %q missing: %v", p, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("attachment %q perm = %o, want 0600", p, info.Mode().Perm())
		}
	}
	if !strings.Contains(normalCall.stdin, "看看这两张图和文档") ||
		!strings.Contains(normalCall.stdin, "[飞书附件，已保存至本机，请直接读取查看]") ||
		!strings.Contains(normalCall.stdin, "- 图片："+img1) ||
		!strings.Contains(normalCall.stdin, "- 图片："+img2) ||
		!strings.Contains(normalCall.stdin, "- 文件："+file1) {
		t.Fatalf("normal message body missing attachment paths:\n%s", normalCall.stdin)
	}

	// 2. Decision reply with --item <N> and file attachment: MUST pass --item and --file.
	itemFiles := []core.FileAttachment{
		{FileName: "decision-note.txt", MimeType: "text/plain", Data: []byte("approved")},
	}
	if err := sess.Send("【决定】[#12] 采用哪个方案？：方案 A", "om_att_item", nil, itemFiles); err != nil {
		t.Fatalf("Send item decision with attachment failed: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	_ = sess.(*Session).CancelTurn()
	_ = collectTurnEvents(t, sess.Events(), 3*time.Second)

	mu.Lock()
	if len(sendCalls) < 2 {
		mu.Unlock()
		t.Fatalf("expected 2 orchestration send calls, got %d", len(sendCalls))
	}
	itemCall := sendCalls[1]
	mu.Unlock()

	var itemFlag, fileFlag string
	for i := 0; i < len(itemCall.args)-1; i++ {
		if itemCall.args[i] == "--item" {
			itemFlag = itemCall.args[i+1]
		}
		if itemCall.args[i] == "--file" {
			fileFlag = itemCall.args[i+1]
		}
	}
	expectedItemFile := filepath.Join(dataDir, "orca-attachments", "om_att_item", "decision-note.txt")
	if itemFlag != "12" || fileFlag != expectedItemFile {
		t.Fatalf("expected --item 12 --file %q, got args=%v", expectedItemFile, itemCall.args)
	}
	if strings.Contains(itemCall.stdin, "[飞书附件，已保存至本机，请直接读取查看]") {
		t.Fatalf("item send with --file should not duplicate body attachment block: %s", itemCall.stdin)
	}
}

func TestBaselinePreventsHistoryReplayAndHandlesSlidingWindow(t *testing.T) {
	// Simulate read_limit=3 sliding window and in-place streaming growth,
	// plus initial missing Home Chat resolved after Send.
	var chatsCalls, readCalls int
	var mu sync.Mutex

	runner := &mockRunner{
		handler: func(args []string, _ []byte) ([]byte, error) {
			switch strings.Join(args[:min(2, len(args))], " ") {
			case "bot chats":
				mu.Lock()
				chatsCalls++
				n := chatsCalls
				mu.Unlock()
				if n <= 2 { // StartSession + pre-Send: no Home Chat yet
					return mustEnvelope(t, map[string]any{"chats": []any{}}), nil
				}
				return mustEnvelope(t, map[string]any{
					"chats": []map[string]any{{"id": "home-new", "home": true}},
				}), nil
			case "bot work", "bot item":
				return mustEnvelope(t, map[string]any{"items": []any{}}), nil
			case "orchestration send":
				return []byte(`{"ok":true,"result":{}}`), nil
			case "agent read":
				mu.Lock()
				readCalls++
				n := readCalls
				mu.Unlock()
				switch n {
				case 1: // Turn 1 poll 1: partial assistant text
					return mustEnvelope(t, map[string]any{
						"state": "working",
						"entries": []map[string]any{
							{"kind": "message", "role": "user", "text": "hello"},
							{"kind": "message", "role": "assistant", "text": "正在"},
						},
					}), nil
				case 2: // Turn 1 poll 2: grown assistant text in place
					return mustEnvelope(t, map[string]any{
						"state": "done",
						"entries": []map[string]any{
							{"kind": "message", "role": "user", "text": "hello"},
							{"kind": "message", "role": "assistant", "text": "正在处理，已完成。"},
						},
					}), nil
				case 3: // Turn 1 confirm done
					return mustEnvelope(t, map[string]any{
						"state": "done",
						"entries": []map[string]any{
							{"kind": "message", "role": "user", "text": "hello"},
							{"kind": "message", "role": "assistant", "text": "正在处理，已完成。"},
						},
					}), nil
				case 4: // Turn 2 baseline (at limit=3)
					return mustEnvelope(t, map[string]any{
						"state":   "done",
						"limited": true,
						"entries": []map[string]any{
							{"kind": "message", "role": "user", "text": "hello"},
							{"kind": "tool", "name": "execute", "state": "completed"},
							{"kind": "message", "role": "assistant", "text": "正在处理，已完成。"},
						},
					}), nil
				default: // Turn 2 sliding window shifted by 2 entries (still len=3!)
					return mustEnvelope(t, map[string]any{
						"state":   "done",
						"limited": true,
						"entries": []map[string]any{
							{"kind": "message", "role": "assistant", "text": "正在处理，已完成。"},
							{"kind": "message", "role": "user", "text": "第二轮"},
							{"kind": "message", "role": "assistant", "text": "第二轮答复"},
						},
					}), nil
				}
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}

	ag := newTestAgent(t, runner, map[string]any{"read_limit": 3, "sync_needs_you": false})
	sess, err := ag.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.CurrentSessionID() != "" {
		t.Fatalf("expected empty initial chatID, got %q", sess.CurrentSessionID())
	}

	// Turn 1: discovers Home Chat after Send and streams delta properly.
	if err := sess.Send("hello", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	ev1 := collectTurnEvents(t, sess.Events(), 3*time.Second)
	if sess.CurrentSessionID() != "home-new" {
		t.Fatalf("CurrentSessionID after send = %q, want home-new", sess.CurrentSessionID())
	}
	if len(ev1) != 3 || ev1[0].Content != "正在" || ev1[1].Content != "处理，已完成。" || ev1[2].Content != "正在处理，已完成。" {
		t.Fatalf("turn 1 delta/result mismatch: %+v", ev1)
	}

	// Turn 2: sliding window stays len=3; old assistant reply must NOT replay.
	if err := sess.Send("第二轮", "m2", nil, nil); err != nil {
		t.Fatal(err)
	}
	ev2 := collectTurnEvents(t, sess.Events(), 3*time.Second)
	if len(ev2) != 2 || ev2[0].Type != core.EventText || ev2[0].Content != "第二轮答复" || ev2[1].Content != "第二轮答复" {
		t.Fatalf("turn 2 sliding window mismatch: %+v", ev2)
	}
}

func TestBackgroundWorkerTaskCardEvents(t *testing.T) {
	var workPoll int
	var mu sync.Mutex

	runner := &mockRunner{
		handler: func(args []string, _ []byte) ([]byte, error) {
			switch strings.Join(args[:min(2, len(args))], " ") {
			case "bot chats":
				return mustEnvelope(t, map[string]any{
					"chats": []map[string]any{{"id": "home-w", "home": true}},
				}), nil
			case "bot item":
				return mustEnvelope(t, map[string]any{"items": []any{}}), nil
			case "orchestration send":
				return []byte(`{"ok":true,"result":{}}`), nil
			case "agent read":
				return mustEnvelope(t, map[string]any{
					"state":   "done",
					"entries": []map[string]any{{"kind": "message", "role": "assistant", "text": "已派 worker 处理。"}},
				}), nil
			case "bot work":
				mu.Lock()
				workPoll++
				n := workPoll
				mu.Unlock()
				switch n {
				case 1: // baseline on StartSession: old completed worker must be ignored
					return mustEnvelope(t, map[string]any{
						"items": []map[string]any{
							{"dispatchId": "old-done-1", "title": "历史旧任务", "status": "completed"},
						},
					}), nil
				case 2: // worker starts
					return mustEnvelope(t, map[string]any{
						"items": []map[string]any{
							{"dispatchId": "old-done-1", "title": "历史旧任务", "status": "completed"},
							{"dispatchId": "wk-101", "title": "编译并跑单测", "status": "running", "phase": "正在拉取依赖", "ageMs": 5000},
						},
					}), nil
				case 3: // worker progresses
					return mustEnvelope(t, map[string]any{
						"items": []map[string]any{
							{"dispatchId": "wk-101", "title": "编译并跑单测", "status": "running", "phase": "运行 go test", "summary": "已通过 8/10", "ageMs": 15000},
						},
					}), nil
				default: // worker completes
					return mustEnvelope(t, map[string]any{
						"items": []map[string]any{
							{"dispatchId": "wk-101", "title": "编译并跑单测", "status": "completed", "summary": "全部通过", "prUrl": "https://github.com/org/repo/pull/42", "ageMs": 28000},
						},
					}), nil
				}
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}

	ag := newTestAgent(t, runner, map[string]any{"sync_needs_you": false})
	sess, err := ag.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Send("跑一下构建", "m-w1", nil, nil); err != nil {
		t.Fatal(err)
	}

	var taskEvents []*core.TaskEvent
	deadline := time.After(3 * time.Second)
	for len(taskEvents) < 3 {
		select {
		case ev := <-sess.Events():
			if ev.Type == core.EventTask && ev.Task != nil {
				taskEvents = append(taskEvents, ev.Task)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for 3 task events; got %d: %+v", len(taskEvents), taskEvents)
		}
	}

	if taskEvents[0].Kind != core.TaskStarted || taskEvents[0].TaskID != "wk-101" ||
		taskEvents[0].Background == nil || !*taskEvents[0].Background || taskEvents[0].Description != "编译并跑单测" {
		t.Fatalf("unexpected TaskStarted: %+v", taskEvents[0])
	}
	if taskEvents[1].Kind != core.TaskProgress || taskEvents[1].Step != "运行 go test" || taskEvents[1].Summary != "已通过 8/10" {
		t.Fatalf("unexpected TaskProgress: %+v", taskEvents[1])
	}
	if taskEvents[2].Kind != core.TaskEnded || taskEvents[2].Status != "completed" ||
		!strings.Contains(taskEvents[2].Summary, "全部通过") || !strings.Contains(taskEvents[2].Summary, "pull/42") {
		t.Fatalf("unexpected TaskEnded: %+v", taskEvents[2])
	}
	if got := sess.(*Session).RunningTasks(); got != 0 {
		t.Fatalf("RunningTasks() = %d, want 0", got)
	}
}

func TestNeedsYouItemBridgedToDecisionCardAndFallback(t *testing.T) {
	var itemListCall int
	var readCall int
	var mu sync.Mutex

	runner := &mockRunner{
		handler: func(args []string, stdin []byte) ([]byte, error) {
			cmd := strings.Join(args[:min(3, len(args))], " ")
			switch {
			case strings.HasPrefix(cmd, "bot chats"):
				return mustEnvelope(t, map[string]any{
					"chats": []map[string]any{{"id": "home-ny", "home": true}},
				}), nil
			case strings.HasPrefix(cmd, "bot work"):
				return mustEnvelope(t, map[string]any{"items": []any{}}), nil
			case strings.HasPrefix(cmd, "orchestration send"):
				return []byte(`{"ok":true,"result":{}}`), nil
			case strings.HasPrefix(cmd, "agent read"):
				mu.Lock()
				readCall++
				n := readCall
				mu.Unlock()
				if n == 1 {
					return mustEnvelope(t, map[string]any{"state": "done", "entries": []any{}}), nil
				}
				return mustEnvelope(t, map[string]any{
					"state": "done",
					"entries": []map[string]any{
						{"kind": "message", "role": "assistant", "text": "已整理两个方案，请确认。"},
					},
				}), nil
			case strings.HasPrefix(cmd, "bot item list"):
				mu.Lock()
				itemListCall++
				n := itemListCall
				mu.Unlock()
				if n == 1 { // baseline on StartSession: #5 already existed
					return mustEnvelope(t, map[string]any{
						"items": []map[string]any{
							{"number": 5, "title": "旧待确认", "status": "needs_you", "lastEventId": "e10"},
						},
					}), nil
				}
				return mustEnvelope(t, map[string]any{
					"items": []map[string]any{
						{"number": 5, "title": "旧待确认", "status": "needs_you", "lastEventId": "e10"},
						{"number": 12, "title": "是否上线网页版", "status": "needs_you", "lastEventId": "e42"},
					},
				}), nil
			case strings.HasPrefix(cmd, "bot item show"):
				return mustEnvelope(t, map[string]any{
					"item": map[string]any{
						"number":      12,
						"title":       "是否上线网页版",
						"status":      "needs_you",
						"version":     3,
						"lastEventId": "e42",
						"chat":        map[string]any{"id": "home-ny", "kind": "home"},
						"question": map[string]any{
							"eventId": "e42",
							"gist":    "需要确定部署方案",
							"decisions": []map[string]any{
								{
									"question": "采用哪个方案？",
									"replies": []map[string]any{
										{"label": "方案 A", "recommended": true},
										{"label": "方案 B", "recommended": false},
									},
								},
							},
						},
					},
				}), nil
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}

	ag := newTestAgent(t, runner, map[string]any{"sync_needs_you": true})
	sess, err := ag.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Send("评估网页版方案", "m-ny1", nil, nil); err != nil {
		t.Fatal(err)
	}

	var permEv *core.Event
	deadline := time.After(3 * time.Second)
	for permEv == nil {
		select {
		case ev := <-sess.Events():
			if ev.Type == core.EventPermissionRequest {
				c := ev
				permEv = &c
			}
		case <-deadline:
			t.Fatal("timed out waiting for needs-you EventPermissionRequest")
		}
	}

	if permEv.ToolName != "AskUserQuestion" || len(permEv.Questions) != 1 || len(permEv.Questions[0].Options) != 2 {
		t.Fatalf("unexpected permission event: %+v", permEv)
	}
	if !strings.HasPrefix(permEv.Questions[0].Question, "[#12] 采用哪个方案？") {
		t.Fatalf("unexpected question text: %q", permEv.Questions[0].Question)
	}

	// Path A: cc-connect converted it to a non-blocking decision card and calls
	// RespondPermission(deny). That must NOT send anything to Orca.
	callsBefore := len(runner.snapshotCalls())
	if err := sess.RespondPermission(permEv.RequestID, core.PermissionResult{Behavior: "deny", Message: "已发选择卡片"}); err != nil {
		t.Fatalf("RespondPermission(deny) failed: %v", err)
	}
	if len(runner.snapshotCalls()) != callsBefore {
		t.Fatal("RespondPermission(deny) should not invoke Orca CLI")
	}

	// When the user later clicks the Feishu decision card, cc-connect calls Send("【决定】[#12] ...").
	// Update readCall behavior cleanly by accepting another turn.
	if err := sess.Send("【决定】"+permEv.Questions[0].Question+"：方案 A", "dec-1", nil, nil); err != nil {
		t.Fatalf("Send decision answer failed: %v", err)
	}
	// Cancel turn after verifying send args (since mock agent read stays static).
	time.Sleep(40 * time.Millisecond)
	_ = sess.(*Session).CancelTurn()
	_ = collectTurnEvents(t, sess.Events(), 3*time.Second)

	var foundItemRoute bool
	for _, c := range runner.snapshotCalls() {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, "orchestration send") && strings.Contains(joined, "--item 12") && strings.Contains(joined, "--type handoff") {
			foundItemRoute = true
			break
		}
	}
	if !foundItemRoute {
		t.Fatalf("expected decision prompt to route with --item 12 --type handoff; calls=%+v", runner.snapshotCalls())
	}

	// Path B: Blocking AskUserQuestion fallback when RespondPermission(allow) is called.
	s := sess.(*Session)
	s.items.mu.Lock()
	s.items.pendingReq["orca-item:15:e99"] = itemRoute{Number: 15, ChatID: "home-ny"}
	s.items.mu.Unlock()
	if err := sess.RespondPermission("orca-item:15:e99", core.PermissionResult{
		Behavior: "allow",
		UpdatedInput: map[string]any{
			"answers": map[string]any{"[#15] 继续吗？": "继续"},
		},
	}); err != nil {
		t.Fatalf("RespondPermission(allow) failed: %v", err)
	}
	var foundFallback bool
	for _, c := range runner.snapshotCalls() {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, "orchestration send") && strings.Contains(joined, "--item 15") && strings.Contains(c.stdin, "继续") {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Fatal("expected blocking RespondPermission(allow) to send --item 15 handoff")
	}
}

func TestErrorHandlingCancelAndCloseCleanup(t *testing.T) {
	// Subtest 1: sustained CLI read errors emit EventError.
	t.Run("consecutive_read_errors", func(t *testing.T) {
		var reads int
		var mu sync.Mutex
		runner := &mockRunner{
			handler: func(args []string, _ []byte) ([]byte, error) {
				switch strings.Join(args[:min(2, len(args))], " ") {
				case "bot chats":
					return mustEnvelope(t, map[string]any{"chats": []map[string]any{{"id": "home-err", "home": true}}}), nil
				case "bot work", "bot item":
					return mustEnvelope(t, map[string]any{"items": []any{}}), nil
				case "orchestration send":
					return []byte(`{"ok":true,"result":{}}`), nil
				case "agent read":
					mu.Lock()
					reads++
					n := reads
					mu.Unlock()
					if n == 1 {
						return mustEnvelope(t, map[string]any{"state": "done", "entries": []any{}}), nil
					}
					return nil, errors.New("orca daemon unreachable")
				}
				return nil, fmt.Errorf("unexpected: %v", args)
			},
		}
		ag := newTestAgent(t, runner, map[string]any{"sync_needs_you": false})
		sess, err := ag.StartSession(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Send("测试报错", "m-err", nil, nil); err != nil {
			t.Fatal(err)
		}
		events := collectTurnEvents(t, sess.Events(), 3*time.Second)
		if len(events) == 0 || events[len(events)-1].Type != core.EventError {
			t.Fatalf("expected EventError, got %+v", events)
		}
	})

	// Subtest 2: CancelTurn cancels cleanly without closing the session; Close closes Events().
	t.Run("cancel_and_close", func(t *testing.T) {
		runner := &mockRunner{
			handler: func(args []string, _ []byte) ([]byte, error) {
				switch strings.Join(args[:min(2, len(args))], " ") {
				case "bot chats":
					return mustEnvelope(t, map[string]any{"chats": []map[string]any{{"id": "home-cancel", "home": true}}}), nil
				case "bot work", "bot item":
					return mustEnvelope(t, map[string]any{"items": []any{}}), nil
				case "orchestration send":
					return []byte(`{"ok":true,"result":{}}`), nil
				case "agent read":
					return mustEnvelope(t, map[string]any{"state": "working", "entries": []any{}}), nil
				}
				return nil, fmt.Errorf("unexpected: %v", args)
			},
		}
		ag := newTestAgent(t, runner, map[string]any{"sync_needs_you": false})
		sess, err := ag.StartSession(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Send("长任务", "m-cancel", nil, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
		if err := sess.(*Session).CancelTurn(); err != nil {
			t.Fatalf("CancelTurn failed: %v", err)
		}
		events := collectTurnEvents(t, sess.Events(), 3*time.Second)
		if len(events) == 0 || events[len(events)-1].Type != core.EventResult || !events[len(events)-1].Done {
			t.Fatalf("expected clean EventResult on CancelTurn, got %+v", events)
		}
		if !sess.Alive() {
			t.Fatal("session should remain alive after CancelTurn")
		}
		if err := sess.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		select {
		case _, ok := <-sess.Events():
			if ok {
				// drain until closed
				for range sess.Events() {
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Events() channel was not closed after Close()")
		}
		if sess.Alive() {
			t.Fatal("session still reports Alive() after Close()")
		}
	})

	// Subtest 3: turn timeout emits EventError.
	t.Run("turn_timeout", func(t *testing.T) {
		runner := &mockRunner{
			handler: func(args []string, _ []byte) ([]byte, error) {
				switch strings.Join(args[:min(2, len(args))], " ") {
				case "bot chats":
					return mustEnvelope(t, map[string]any{"chats": []map[string]any{{"id": "home-to", "home": true}}}), nil
				case "bot work", "bot item":
					return mustEnvelope(t, map[string]any{"items": []any{}}), nil
				case "orchestration send":
					return []byte(`{"ok":true,"result":{}}`), nil
				case "agent read":
					return mustEnvelope(t, map[string]any{"state": "working", "entries": []any{}}), nil
				}
				return nil, fmt.Errorf("unexpected: %v", args)
			},
		}
		ag := newTestAgent(t, runner, map[string]any{"sync_needs_you": false})
		ag.turnTimeout = 80 * time.Millisecond
		sess, err := ag.StartSession(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Send("会超时", "m-to", nil, nil); err != nil {
			t.Fatal(err)
		}
		events := collectTurnEvents(t, sess.Events(), 3*time.Second)
		if len(events) == 0 || events[len(events)-1].Type != core.EventError || !strings.Contains(events[len(events)-1].Error.Error(), "timed out") {
			t.Fatalf("expected timeout EventError, got %+v", events)
		}
	})
}

// engineStubPlatform exercises cc-connect core.Engine's StepList + DecisionCard
// integration end-to-end with agent/orca.
type engineStubPlatform struct {
	mu       sync.Mutex
	progress []string
	tools    []string
	finished []string
	sent     []string
	cards    []*core.Card
}

func (p *engineStubPlatform) Name() string                         { return "feishu" }
func (p *engineStubPlatform) Start(core.MessageHandler) error      { return nil }
func (p *engineStubPlatform) Stop() error                          { return nil }
func (p *engineStubPlatform) Reply(_ context.Context, _ any, s string) error {
	p.mu.Lock()
	p.sent = append(p.sent, s)
	p.mu.Unlock()
	return nil
}
func (p *engineStubPlatform) Send(_ context.Context, _ any, s string) error {
	p.mu.Lock()
	p.sent = append(p.sent, s)
	p.mu.Unlock()
	return nil
}
func (p *engineStubPlatform) UpdateMessage(context.Context, any, string) error { return nil }
func (p *engineStubPlatform) KeepPreviewOnFinish() bool                        { return true }
func (p *engineStubPlatform) UseThinkingProcessPreview() bool                  { return true }
func (p *engineStubPlatform) StartSegmentedPreview(context.Context, any, core.StreamPreviewCfg, *core.I18n) (any, error) {
	return "preview-1", nil
}
func (p *engineStubPlatform) SetPreviewBody(context.Context, any, string) error { return nil }
func (p *engineStubPlatform) AddPreviewProgress(_ context.Context, _ any, s string) error {
	p.mu.Lock()
	p.progress = append(p.progress, s)
	p.mu.Unlock()
	return nil
}
func (p *engineStubPlatform) AddPreviewTool(_ context.Context, _ any, s string) error {
	p.mu.Lock()
	p.tools = append(p.tools, s)
	p.mu.Unlock()
	return nil
}
func (p *engineStubPlatform) FinishSegmentedPreview(_ context.Context, _ any, s string) error {
	p.mu.Lock()
	p.finished = append(p.finished, s)
	p.mu.Unlock()
	return nil
}
func (p *engineStubPlatform) SendCardID(_ context.Context, _ any, c *core.Card) (string, error) {
	p.mu.Lock()
	p.cards = append(p.cards, c)
	id := fmt.Sprintf("card-%d", len(p.cards))
	p.mu.Unlock()
	return id, nil
}
func (p *engineStubPlatform) ReconstructReplyCtx(string) (any, error) { return "ctx", nil }

func TestEngineStepListAndDecisionCardEndToEnd(t *testing.T) {
	var readStep, itemPolls int
	var mu sync.Mutex
	runner := &mockRunner{
		handler: func(args []string, _ []byte) ([]byte, error) {
			cmd := strings.Join(args[:min(3, len(args))], " ")
			switch {
			case strings.HasPrefix(cmd, "bot chats"):
				return mustEnvelope(t, map[string]any{"chats": []map[string]any{{"id": "home-e2e", "home": true}}}), nil
			case strings.HasPrefix(cmd, "bot work"):
				return mustEnvelope(t, map[string]any{"items": []any{}}), nil
			case strings.HasPrefix(cmd, "orchestration send"):
				return []byte(`{"ok":true,"result":{}}`), nil
			case strings.HasPrefix(cmd, "bot item list"):
				mu.Lock()
				itemPolls++
				n := itemPolls
				mu.Unlock()
				if n == 1 {
					return mustEnvelope(t, map[string]any{"items": []any{}}), nil
				}
				return mustEnvelope(t, map[string]any{
					"items": []map[string]any{
						{"number": 21, "title": "发布确认", "status": "needs_you", "lastEventId": "e77"},
					},
				}), nil
			case strings.HasPrefix(cmd, "bot item show"):
				return mustEnvelope(t, map[string]any{
					"item": map[string]any{
						"number":      21,
						"title":       "发布确认",
						"status":      "needs_you",
						"lastEventId": "e77",
						"chat":        map[string]any{"id": "home-e2e", "kind": "home"},
						"question": map[string]any{
							"eventId": "e77",
							"decisions": []map[string]any{
								{
									"question": "是否立即发布？",
									"replies": []map[string]any{
										{"label": "立即发布", "recommended": true},
										{"label": "稍后再发", "recommended": false},
									},
								},
							},
						},
					},
				}), nil
			case strings.HasPrefix(cmd, "agent read"):
				mu.Lock()
				readStep++
				n := readStep
				mu.Unlock()
				switch n {
				case 1:
					return mustEnvelope(t, map[string]any{"state": "done", "entries": []any{}}), nil
				case 2:
					return mustEnvelope(t, map[string]any{
						"state": "working",
						"entries": []map[string]any{
							{"kind": "message", "role": "assistant", "text": "正在核对发布清单。"},
							{"kind": "tool", "name": "read_file", "state": "completed"},
						},
					}), nil
				default:
					return mustEnvelope(t, map[string]any{
						"state": "done",
						"entries": []map[string]any{
							{"kind": "message", "role": "assistant", "text": "正在核对发布清单。"},
							{"kind": "tool", "name": "read_file", "state": "completed"},
							{"kind": "message", "role": "assistant", "text": "清单核对完毕，已发确认卡请你拍板。"},
						},
					}), nil
				}
			}
			return nil, fmt.Errorf("unexpected: %v", args)
		},
	}

	ag := newTestAgent(t, runner, map[string]any{"sync_needs_you": true})
	p := &engineStubPlatform{}
	dataDir := t.TempDir()
	eng := core.NewEngine("demo-orca", ag, []core.Platform{p}, filepath.Join(dataDir, "sessions.json"), core.LangChinese)
	eng.SetDisplayConfig(core.DisplayCfg{Mode: "quiet", ProgressHistory: "append", HideAgentFooter: true})
	spCfg := core.ProductStreamPreviewCfg()
	eng.SetStreamPreviewCfg(spCfg)
	decStore, err := core.NewDecisionStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	eng.SetDecisionStore(decStore, 12)
	t.Cleanup(func() { _ = eng.Stop() })

	eng.ReceiveMessage(p, &core.Message{
		SessionKey: "feishu:chat-1:user-1",
		Platform:   "feishu",
		MessageID:  "om_e2e_1",
		UserID:     "user-1",
		Content:    "准备发布",
		ReplyCtx:   "ctx",
	})

	deadline := time.Now().Add(4 * time.Second)
	for {
		p.mu.Lock()
		done := len(p.finished) > 0 && len(p.cards) > 0
		p.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			p.mu.Lock()
			t.Fatalf("timed out waiting for e2e finish+card; progress=%v tools=%v finished=%v cards=%d", p.progress, p.tools, p.finished, len(p.cards))
		}
		time.Sleep(15 * time.Millisecond)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.progress) == 0 || p.progress[0] != "正在核对发布清单。" {
		t.Fatalf("StepList progress = %v, want [正在核对发布清单。]", p.progress)
	}
	if len(p.tools) == 0 || p.tools[0] != "读取文件" {
		t.Fatalf("StepList tools = %v, want [读取文件]", p.tools)
	}
	if p.finished[0] != "清单核对完毕，已发确认卡请你拍板。" {
		t.Fatalf("finished = %q", p.finished[0])
	}
	if len(p.cards) != 1 {
		t.Fatalf("expected 1 native decision card for Needs-You #21, got %d", len(p.cards))
	}
}

func TestExecRunnerWithMockOrcaScript(t *testing.T) {
	dir := t.TempDir()
	fakeHome := filepath.Join(dir, "orca-home")
	if err := os.MkdirAll(fakeHome, 0o700); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "mock-orca")
	script := `#!/bin/sh
set -eu
if [ "${ORCA_AGENT_SESSION_ID:-}" != "" ]; then
  echo '{"ok":false,"error":"leaked ORCA_AGENT_SESSION_ID"}'
  exit 1
fi
if [ "${HOME:-}" != "` + fakeHome + `" ]; then
  echo '{"ok":false,"error":"wrong HOME"}'
  exit 1
fi
if [ "$1" = "bot" ] && [ "$2" = "chats" ]; then
  echo '{"ok":true,"result":{"bot":"demo","chats":[{"id":"chat-script-1","title":"Home","home":true,"closed":false}]}}'
  exit 0
fi
if [ "$1" = "orchestration" ] && [ "$2" = "send" ]; then
  body=$(cat)
  printf '{"ok":true,"result":{"echo":%s}}\n' "$(printf '%s' "$body" | sed 's/"/\\"/g; s/^/"/; s/$/"/')"
  exit 0
fi
echo '{"ok":true,"result":{"items":[]}}'
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ORCA_AGENT_SESSION_ID", "should-be-stripped")

	raw, err := New(map[string]any{
		"bot_id":    "demo-bot",
		"orca_bin":  scriptPath,
		"orca_home": fakeHome,
		"sender_id": "feishu-bridge-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ag := raw.(*Agent)
	chats, err := ag.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions via script failed: %v", err)
	}
	if len(chats) != 1 || chats[0].ID != "chat-script-1" {
		t.Fatalf("unexpected ListSessions result: %+v", chats)
	}
	if err := ag.client().sendMessage(context.Background(), sendMessageReq{
		BotID:   "demo-bot",
		From:    "feishu-bridge-test",
		Type:    "question",
		Subject: "测试",
		Body:    "你好 Orca",
	}); err != nil {
		t.Fatalf("sendMessage via script failed: %v", err)
	}
}
