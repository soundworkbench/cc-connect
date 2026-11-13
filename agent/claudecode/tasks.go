package claudecode

import (
	"fmt"
	"regexp"
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
			ev.Step, ev.StepGeneric, ev.StepCommand = chineseToolProgress("Bash", nil, ""), true, true
		}
	case "task_progress":
		ev.Kind = core.TaskProgress
		ev.Summary, _ = raw["summary"].(string)
		if step := taskProgressStep(description, ev.Summary); step != "" {
			ev.Step, ev.StepCommand = step, strings.HasPrefix(strings.TrimSpace(description), "Running ")
		} else if tool, _ := raw["last_tool_name"].(string); tool != "" {
			ev.Step, ev.StepGeneric, ev.StepCommand = chineseToolProgress(tool, nil, ""), true, tool == "Bash"
		}
		ev.Elapsed = taskDuration(raw)
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
		// A subagent's notification carries its whole run time.
		ev.Elapsed = taskDuration(raw)
	default:
		return nil
	}
	ev.Description = strings.TrimSpace(ev.Description)
	ev.Summary = strings.TrimSpace(ev.Summary)
	return ev
}

// taskDuration is a frame's usage.duration_ms: how long the task has run.
func taskDuration(raw map[string]any) time.Duration {
	usage, _ := raw["usage"].(map[string]any)
	return time.Duration(asInt(usage["duration_ms"])) * time.Millisecond
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

// signalExit matches the exit code of a command killed by a signal and so
// stopped, not failed: 143 is SIGTERM, and a background command stopped
// through TaskStop in the 11-11 test ended with 144.
var signalExit = regexp.MustCompile(`exit code 14[34]\b`)

// noteTaskStop remembers the task a TaskStop call (KillShell and KillBash
// are its older names) asks to stop.
func (cs *claudeSession) noteTaskStop(tool string, input any) {
	if tool != "TaskStop" && tool != "KillShell" && tool != "KillBash" {
		return
	}
	m, _ := input.(map[string]any)
	for _, key := range []string{"task_id", "shell_id"} {
		if id := strVal(m, key); id != "" {
			cs.stoppingTasks.Store(id, true)
		}
	}
}

// markStoppedTask reports a task that ended because it was stopped as
// "stopped": the CLI reports a stopped background command as "failed" with
// the signal's exit code.
func (cs *claudeSession) markStoppedTask(ev *core.TaskEvent) {
	if ev.Kind != core.TaskEnded {
		return
	}
	_, asked := cs.stoppingTasks.LoadAndDelete(ev.TaskID)
	if ev.Status == "failed" && (asked || signalExit.MatchString(ev.Summary)) {
		ev.Status = "stopped"
	}
}

// trackTask keeps liveTasks in step with the CLI's task lifecycle. Hidden
// tasks (ambient, skip_transcript) are the CLI's own bookkeeping and do not
// count as work the person is waiting for.
func (cs *claudeSession) trackTask(ev *core.TaskEvent) {
	switch ev.Kind {
	case core.TaskStarted:
		if ev.Hidden {
			return
		}
		if _, loaded := cs.liveTasks.LoadOrStore(ev.TaskID, struct{}{}); !loaded {
			cs.liveTaskCount.Add(1)
		}
	case core.TaskEnded:
		if _, loaded := cs.liveTasks.LoadAndDelete(ev.TaskID); loaded {
			cs.liveTaskCount.Add(-1)
			cs.lastTaskEnd.Store(time.Now().UnixNano())
		}
	}
}

// RunningTasks implements core.RunningTasksReporter.
func (cs *claudeSession) RunningTasks() int { return int(cs.liveTaskCount.Load()) }

// taskSettleGrace is how long after its last task ended the CLI may still
// start the turn that reports the result. It does so within milliseconds;
// silence this long means no such turn is coming.
var taskSettleGrace = 5 * time.Second

// HasBackgroundWork implements core.BackgroundWorkReporter. Between turns the
// CLI still works while a task it started runs, while it writes the turn it
// started itself on a task's notification, and briefly after the last task
// ended, before that turn begins. The process must stay up, with its stdin
// open, through all of it.
func (cs *claudeSession) HasBackgroundWork() bool {
	if !cs.alive.Load() {
		return false
	}
	if cs.liveTaskCount.Load() > 0 || cs.cliWorking.Load() {
		return true
	}
	end := cs.lastTaskEnd.Load()
	return end != 0 && time.Since(time.Unix(0, end)) < taskSettleGrace
}

// busy reports whether the CLI is working: a turn is in flight (ours or one
// the CLI started itself) or a task is still running. Send holds inputMu
// while it writes a turn, so a held lock counts as busy.
func (cs *claudeSession) busy() bool {
	if cs.liveTaskCount.Load() > 0 || cs.cliWorking.Load() {
		return true
	}
	if !cs.inputMu.TryLock() {
		return true
	}
	defer cs.inputMu.Unlock()
	return cs.inputActive
}

var stopTaskSeq atomic.Int64

// StopTask asks the CLI to stop one task (control request stop_task). The
// task's own task_notification (status "stopped") confirms it.
func (cs *claudeSession) StopTask(taskID string) error {
	if !cs.alive.Load() {
		return fmt.Errorf("session process is not running")
	}
	cs.stoppingTasks.Store(taskID, true)
	return cs.writeJSON(map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("cc-connect-stop-task-%d", stopTaskSeq.Add(1)),
		"request":    map[string]any{"subtype": "stop_task", "task_id": taskID},
	})
}
