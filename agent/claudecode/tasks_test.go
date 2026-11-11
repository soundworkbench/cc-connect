package claudecode

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Frames follow the SDK types of Claude Code 2.1.322 (SDKTaskStartedMessage,
// SDKTaskProgressMessage, SDKTaskUpdatedMessage, SDKTaskNotificationMessage).
func TestTaskFramesBecomeTaskEvents(t *testing.T) {
	cs := progressSession()
	for _, line := range []string{
		`{"type":"system","subtype":"task_started","task_id":"b1","description":"整理 200 个文件","task_type":"local_bash","is_backgrounded":true,"uuid":"u","session_id":"s"}`,
		`{"type":"system","subtype":"task_progress","task_id":"a1","description":"查资料","usage":{"total_tokens":10,"tool_uses":3,"duration_ms":65000},"last_tool_name":"WebSearch","summary":"在比较三家供应商"}`,
		`{"type":"system","subtype":"task_updated","task_id":"a1","patch":{"is_backgrounded":true,"status":"running"}}`,
		`{"type":"system","subtype":"task_notification","task_id":"b1","status":"completed","output_file":"/tmp/o","summary":"整理完成"}`,
		`{"type":"system","subtype":"task_started","task_id":"w1","description":"watch","ambient":true,"skip_transcript":true}`,
		`{"type":"system","subtype":"task_progress","description":"no id"}`,
	} {
		cs.handleReadLoopLine(line)
	}
	events := drainProgressEvents(cs)
	if len(events) != 5 {
		t.Fatalf("events=%+v", events)
	}
	for _, ev := range events {
		if ev.Type != core.EventTask || ev.Task == nil {
			t.Fatalf("event=%+v", ev)
		}
	}
	started, progress, updated, ended, ambient := events[0].Task, events[1].Task, events[2].Task, events[3].Task, events[4].Task
	if started.Kind != core.TaskStarted || started.TaskID != "b1" || started.Description != "整理 200 个文件" || started.Background == nil || !*started.Background || started.Step != "运行命令" {
		t.Errorf("started=%+v", started)
	}
	if progress.Kind != core.TaskProgress || progress.Description != "" || progress.Step != "查资料" || progress.Summary != "在比较三家供应商" || progress.Elapsed != 65*time.Second {
		t.Errorf("progress=%+v", progress)
	}
	if updated.Kind != core.TaskUpdated || updated.Background == nil || !*updated.Background {
		t.Errorf("updated=%+v", updated)
	}
	if ended.Kind != core.TaskEnded || ended.Status != "completed" || ended.Summary != "整理完成" {
		t.Errorf("ended=%+v", ended)
	}
	if !ambient.Hidden {
		t.Errorf("ambient task not hidden: %+v", ambient)
	}
}

// A task_progress description says what the task is doing now; the task's
// name stays the one from task_started (the 11-11 self-test card read
// "✓ 已完成：Running 删除临时目录").
func TestTaskProgressDescriptionIsTheStep(t *testing.T) {
	cases := []struct {
		name, frame, step string
	}{
		{"command description", `{"type":"system","subtype":"task_progress","task_id":"a1","description":"Running 删除临时目录","last_tool_name":"Bash","usage":{"duration_ms":1000}}`, "删除临时目录"},
		{"own wording", `{"type":"system","subtype":"task_progress","task_id":"a1","description":"读取八份纪要正文","last_tool_name":"Read"}`, "读取八份纪要正文"},
		{"command without description", `{"type":"system","subtype":"task_progress","task_id":"a1","description":"Running grep -rn '会议' notes/","last_tool_name":"Bash"}`, "运行命令"},
		{"English activity", `{"type":"system","subtype":"task_progress","task_id":"a1","description":"Reading main.go","last_tool_name":"Read"}`, "读取文件"},
		{"summary only", `{"type":"system","subtype":"task_progress","task_id":"a1","description":"正在比对纪要","summary":"正在比对纪要"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := progressSession()
			cs.handleReadLoopLine(tc.frame)
			events := drainProgressEvents(cs)
			if len(events) != 1 || events[0].Task == nil {
				t.Fatalf("events=%+v", events)
			}
			ev := events[0].Task
			if ev.Description != "" || ev.Step != tc.step {
				t.Fatalf("description=%q step=%q, want no description and step %q", ev.Description, ev.Step, tc.step)
			}
		})
	}
}

type bufferCloser struct{ bytes.Buffer }

func (*bufferCloser) Close() error { return nil }

func TestStopTaskWritesControlRequest(t *testing.T) {
	cs := progressSession()
	var _ core.TaskStopper = cs
	buf := &bufferCloser{}
	cs.stdin = buf
	cs.alive.Store(true)
	if err := cs.StopTask("b1"); err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
			TaskID  string `json:"task_id"`
		} `json:"request"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &frame); err != nil {
		t.Fatalf("frame %q: %v", buf.String(), err)
	}
	if frame.Type != "control_request" || frame.RequestID == "" || frame.Request.Subtype != "stop_task" || frame.Request.TaskID != "b1" {
		t.Fatalf("frame=%+v", frame)
	}
}
