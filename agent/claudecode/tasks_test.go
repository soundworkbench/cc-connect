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
	if started.Kind != core.TaskStarted || started.TaskID != "b1" || started.Description != "整理 200 个文件" || started.Background == nil || !*started.Background || started.Step != "运行命令" || !started.StepGeneric {
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
		generic           bool
	}{
		{"command description", `{"type":"system","subtype":"task_progress","task_id":"a1","description":"Running 删除临时目录","last_tool_name":"Bash","usage":{"duration_ms":1000}}`, "删除临时目录", false},
		{"own wording", `{"type":"system","subtype":"task_progress","task_id":"a1","description":"读取八份纪要正文","last_tool_name":"Read"}`, "读取八份纪要正文", false},
		{"command without description", `{"type":"system","subtype":"task_progress","task_id":"a1","description":"Running grep -rn '会议' notes/","last_tool_name":"Bash"}`, "运行命令", true},
		{"English activity", `{"type":"system","subtype":"task_progress","task_id":"a1","description":"Reading main.go","last_tool_name":"Read"}`, "读取文件", true},
		{"summary only", `{"type":"system","subtype":"task_progress","task_id":"a1","description":"正在比对纪要","summary":"正在比对纪要"}`, "", false},
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
			if ev.Description != "" || ev.Step != tc.step || ev.StepGeneric != tc.generic {
				t.Fatalf("description=%q step=%q generic=%v, want no description, step %q, generic %v", ev.Description, ev.Step, ev.StepGeneric, tc.step, tc.generic)
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

// A background command stopped on purpose ends as "failed" with a signal's
// exit code (11-11 test: task b0qp7qehs, "failed with exit code 144"). Ends
// after a TaskStop call or with exit code 143/144 count as stops.
func TestStoppedTasksEndAsStopped(t *testing.T) {
	cs := progressSession()
	assistantBlock(cs, map[string]any{"type": "tool_use", "id": "tu1", "name": "TaskStop", "input": map[string]any{"task_id": "b1"}})
	drainProgressEvents(cs)
	for _, line := range []string{
		`{"type":"system","subtype":"task_notification","task_id":"b1","status":"failed","summary":"Background command \"整理\" failed with exit code 1"}`,
		`{"type":"system","subtype":"task_notification","task_id":"b2","status":"failed","summary":"Background command \"搜索会议\" failed with exit code 144"}`,
		`{"type":"system","subtype":"task_notification","task_id":"b3","status":"failed","summary":"Background command \"x\" failed with exit code 143"}`,
		`{"type":"system","subtype":"task_notification","task_id":"b4","status":"failed","summary":"Background command \"x\" failed with exit code 1"}`,
		`{"type":"system","subtype":"task_notification","task_id":"b5","status":"failed","summary":"Background command \"x\" failed with exit code 1440"}`,
	} {
		cs.handleReadLoopLine(line)
	}
	want := map[string]string{"b1": "stopped", "b2": "stopped", "b3": "stopped", "b4": "failed", "b5": "failed"}
	for _, ev := range drainProgressEvents(cs) {
		if ev.Task == nil {
			continue
		}
		if ev.Task.Status != want[ev.Task.TaskID] {
			t.Errorf("%s: status=%s, want %s", ev.Task.TaskID, ev.Task.Status, want[ev.Task.TaskID])
		}
		delete(want, ev.Task.TaskID)
	}
	if len(want) != 0 {
		t.Fatalf("missing events: %v", want)
	}
	// The StopTask control request counts the same way.
	cs.stdin = &bufferCloser{}
	cs.alive.Store(true)
	if err := cs.StopTask("b6"); err != nil {
		t.Fatal(err)
	}
	cs.handleReadLoopLine(`{"type":"system","subtype":"task_notification","task_id":"b6","status":"failed","summary":"Agent failed"}`)
	if evs := drainProgressEvents(cs); len(evs) != 1 || evs[0].Task.Status != "stopped" {
		t.Fatalf("after StopTask: %+v", evs)
	}
}

// A subagent's task_notification carries its whole run time in
// usage.duration_ms (frame of Claude Code 2.1.3xx, eval run B_t1_r1).
func TestTaskNotificationCarriesTheRunTime(t *testing.T) {
	cs := progressSession()
	cs.handleReadLoopLine(`{"type":"system","subtype":"task_notification","task_id":"ac4d8716cd30fe52d","status":"completed","output_file":"/tmp/o","summary":"检查六页 PPT 视觉版式","usage":{"total_tokens":51234,"tool_uses":9,"duration_ms":1740000}}`)
	cs.handleReadLoopLine(`{"type":"system","subtype":"task_notification","task_id":"b1","status":"completed","summary":"Background command \"统计文档目录\" completed (exit code 0)"}`)
	events := drainProgressEvents(cs)
	if len(events) != 2 || events[0].Task == nil || events[1].Task == nil {
		t.Fatalf("events=%+v", events)
	}
	if ev := events[0].Task; ev.Kind != core.TaskEnded || ev.Elapsed != 29*time.Minute {
		t.Errorf("agent notification=%+v", ev)
	}
	if ev := events[1].Task; ev.Kind != core.TaskEnded || ev.Elapsed != 0 {
		t.Errorf("command notification=%+v", ev)
	}
}
