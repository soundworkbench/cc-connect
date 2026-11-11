package claudecode

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/chenhg5/cc-connect/core"
)

// parseTaskEvent reads the CLI's task lifecycle frames (system/task_started,
// task_progress, task_updated, task_notification; SDK types SDKTask*Message).
// It returns nil for frames without a task ID. The task's name comes only from
// task_started and task_updated: in task_progress, description is what the
// task is doing now ("Running <command description>").
func parseTaskEvent(subtype string, raw map[string]any) *core.TaskEvent {
	id, _ := raw["task_id"].(string)
	if id == "" {
		return nil
	}
	ev := &core.TaskEvent{TaskID: id}
	description, _ := raw["description"].(string)
	ev.Hidden = raw["skip_transcript"] == true || raw["ambient"] == true
	switch subtype {
	case "task_started":
		ev.Kind = core.TaskStarted
		ev.Description = description
		if b, ok := raw["is_backgrounded"].(bool); ok {
			ev.Background = &b
		}
		if taskType, _ := raw["task_type"].(string); taskType == "local_bash" {
			ev.Step = chineseToolProgress("Bash", nil, "")
		}
	case "task_progress":
		ev.Kind = core.TaskProgress
		ev.Summary, _ = raw["summary"].(string)
		if step := taskProgressStep(description, ev.Summary); step != "" {
			ev.Step = step
		} else if tool, _ := raw["last_tool_name"].(string); tool != "" {
			ev.Step = chineseToolProgress(tool, nil, "")
		}
		if usage, ok := raw["usage"].(map[string]any); ok {
			ev.Elapsed = time.Duration(asInt(usage["duration_ms"])) * time.Millisecond
		}
	case "task_updated":
		ev.Kind = core.TaskUpdated
		patch, _ := raw["patch"].(map[string]any)
		ev.Description, _ = patch["description"].(string)
		if b, ok := patch["is_backgrounded"].(bool); ok {
			ev.Background = &b
		}
	case "task_notification":
		ev.Kind = core.TaskEnded
		ev.Status, _ = raw["status"].(string)
		ev.Summary, _ = raw["summary"].(string)
	default:
		return nil
	}
	ev.Description = strings.TrimSpace(ev.Description)
	ev.Summary = strings.TrimSpace(ev.Summary)
	return ev
}

// taskProgressStep turns a task_progress description into the current step:
// the CLI writes "Running <description>" for a command, and the description is
// the model's own Chinese wording. A frame that only carries a progress summary
// repeats it as the description, and a command without a description puts the
// command itself there; neither is a step, so they return "" and the caller
// falls back to the tool's generic wording.
func taskProgressStep(description, summary string) string {
	description = strings.TrimSpace(description)
	if description == "" || description == strings.TrimSpace(summary) {
		return ""
	}
	step := strings.TrimSpace(strings.TrimPrefix(description, "Running "))
	if !strings.ContainsFunc(step, func(r rune) bool { return unicode.Is(unicode.Han, r) }) ||
		strings.ContainsAny(step, "|&;<>$`\"'=\\/") {
		return ""
	}
	return oneLineLabel(step)
}

var stopTaskSeq atomic.Int64

// StopTask asks the CLI to stop one task (control request stop_task). The
// task's own task_notification (status "stopped") confirms it.
func (cs *claudeSession) StopTask(taskID string) error {
	if !cs.alive.Load() {
		return fmt.Errorf("session process is not running")
	}
	return cs.writeJSON(map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("cc-connect-stop-task-%d", stopTaskSeq.Add(1)),
		"request":    map[string]any{"subtype": "stop_task", "task_id": taskID},
	})
}
