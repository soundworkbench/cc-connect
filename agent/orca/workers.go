package orca

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// orcaWorkerSnapshot is a normalized view of one Orca background worker
// reported by `orca bot work list --bot <id> --json`.
type orcaWorkerSnapshot struct {
	ID          string
	Description string
	Status      string // raw normalized lowercase status
	Step        string
	Summary     string
	Elapsed     time.Duration
	Active      bool
	EndStatus   string // "completed", "failed", or "stopped" when !Active
}

func parseWorkerRow(row map[string]any) (orcaWorkerSnapshot, bool) {
	id := firstNonEmptyStr(row, "dispatchId", "dispatch_id", "workerId", "worker_id", "id", "taskId", "task_id", "uid")
	if id == "" {
		return orcaWorkerSnapshot{}, false
	}

	desc := firstNonEmptyStr(row, "title", "subject", "taskTitle", "description", "spec", "name", "repo")
	if desc == "" {
		short := id
		if len(short) > 8 {
			short = short[:8]
		}
		desc = "后台任务 " + short
	}

	rawStatus := strings.ToLower(firstNonEmptyStr(row, "status", "state", "taskStatus", "task_status", "outcome", "terminalState"))
	step := firstNonEmptyStr(row, "phase", "step", "currentStep", "current_step", "progress", "lastActivity")
	summary := firstNonEmptyStr(row, "outcomeSummary", "summary", "result", "closeNote", "error", "prUrl", "pr_url")

	elapsed := parseWorkerElapsed(row)
	active, endStatus := classifyWorkerStatus(rawStatus)

	// If PR link is present and summary doesn't already include it, keep it visible.
	if prURL := firstNonEmptyStr(row, "prUrl", "pr_url"); prURL != "" && summary != prURL && !strings.Contains(summary, prURL) {
		if summary == "" {
			summary = prURL
		} else {
			summary = summary + " (" + prURL + ")"
		}
	}

	return orcaWorkerSnapshot{
		ID:          id,
		Description: oneLineClip(desc, 60),
		Status:      rawStatus,
		Step:        oneLineClip(step, 80),
		Summary:     oneLineClip(summary, 300),
		Elapsed:     elapsed,
		Active:      active,
		EndStatus:   endStatus,
	}, true
}

func classifyWorkerStatus(status string) (active bool, endStatus string) {
	switch status {
	case "", "running", "working", "in_progress", "active", "pending", "queued", "starting", "open", "waiting":
		return true, ""
	case "failed", "error", "errored", "timed_out", "timeout":
		return false, "failed"
	case "stopped", "cancelled", "canceled", "killed", "aborted", "closed":
		return false, "stopped"
	default:
		// completed, done, succeeded, finished, merged, etc.
		return false, "completed"
	}
}

func parseWorkerElapsed(row map[string]any) time.Duration {
	if ms, ok := asInt64(row["ageMs"]); ok && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if ms, ok := asInt64(row["durationMs"]); ok && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	for _, key := range []string{"startedAt", "createdAt", "started_at", "created_at"} {
		v, exists := row[key]
		if !exists || v == nil {
			continue
		}
		if ts, ok := asInt64(v); ok && ts > 0 {
			var t time.Time
			if ts > 1e12 {
				t = time.UnixMilli(ts)
			} else {
				t = time.Unix(ts, 0)
			}
			if d := time.Since(t); d > 0 {
				return d
			}
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
				if d := time.Since(t); d > 0 {
					return d
				}
			}
		}
	}
	return 0
}

type trackedWorker struct {
	snap    orcaWorkerSnapshot
	started bool
	ended   bool
}

// workerTracker diffs `orca bot work list` snapshots and emits cc-connect
// EventTask lifecycle events (TaskStarted / TaskProgress / TaskEnded) to drive
// native Feishu background task cards.
type workerTracker struct {
	mu        sync.Mutex
	baselined bool
	workers   map[string]*trackedWorker
	liveCount atomic.Int32
}

func newWorkerTracker() *workerTracker {
	return &workerTracker{
		workers: make(map[string]*trackedWorker),
	}
}

func (wt *workerTracker) RunningCount() int {
	return int(wt.liveCount.Load())
}

// baseline records already-ended workers so session startup never replays
// historical tasks as fresh cards.
func (wt *workerTracker) baseline(ctx context.Context, client *orcaClient, botID string) {
	snaps, err := client.listWork(ctx, botID)
	if err != nil {
		slog.Debug("orca: worker baseline skipped", "bot", botID, "error", err)
		return
	}
	wt.mu.Lock()
	defer wt.mu.Unlock()
	wt.baselined = true
	for _, w := range snaps {
		if !w.Active {
			wt.workers[w.ID] = &trackedWorker{snap: w, ended: true}
		}
	}
}

func (wt *workerTracker) sync(ctx context.Context, client *orcaClient, botID string, emit func(core.Event)) {
	snaps, err := client.listWork(ctx, botID)
	if err != nil {
		slog.Debug("orca: poll workers failed", "bot", botID, "error", err)
		return
	}

	var events []core.Event

	wt.mu.Lock()
	if !wt.baselined {
		wt.baselined = true
		for _, w := range snaps {
			if !w.Active {
				wt.workers[w.ID] = &trackedWorker{snap: w, ended: true}
			}
		}
	}

	seenNow := make(map[string]struct{}, len(snaps))
	bgTrue := true

	for _, w := range snaps {
		seenNow[w.ID] = struct{}{}
		cur := wt.workers[w.ID]
		if cur == nil {
			if !w.Active {
				wt.workers[w.ID] = &trackedWorker{snap: w, ended: true}
				continue
			}
			wt.workers[w.ID] = &trackedWorker{snap: w, started: true}
			events = append(events, core.Event{
				Type: core.EventTask,
				Task: &core.TaskEvent{
					Kind:        core.TaskStarted,
					TaskID:      w.ID,
					Description: w.Description,
					Background:  &bgTrue,
					Step:        w.Step,
					Elapsed:     w.Elapsed,
				},
			})
			continue
		}

		if cur.ended {
			continue
		}

		if !w.Active {
			cur.ended = true
			cur.snap = w
			summary := w.Summary
			if summary == "" {
				summary = defaultWorkerEndSummary(w.EndStatus)
			}
			events = append(events, core.Event{
				Type: core.EventTask,
				Task: &core.TaskEvent{
					Kind:        core.TaskEnded,
					TaskID:      w.ID,
					Description: w.Description,
					Background:  &bgTrue,
					Status:      w.EndStatus,
					Summary:     summary,
					Elapsed:     w.Elapsed,
				},
			})
			continue
		}

		// Still active: emit TaskProgress if step, description or summary changed.
		if w.Step != cur.snap.Step || w.Summary != cur.snap.Summary || w.Description != cur.snap.Description {
			cur.snap = w
			events = append(events, core.Event{
				Type: core.EventTask,
				Task: &core.TaskEvent{
					Kind:        core.TaskProgress,
					TaskID:      w.ID,
					Description: w.Description,
					Background:  &bgTrue,
					Step:        w.Step,
					Summary:     w.Summary,
					Elapsed:     w.Elapsed,
				},
			})
		}
	}

	// Any previously started worker that vanished from the active listing has ended.
	for id, cur := range wt.workers {
		if cur.ended || !cur.started {
			continue
		}
		if _, ok := seenNow[id]; ok {
			continue
		}
		cur.ended = true
		events = append(events, core.Event{
			Type: core.EventTask,
			Task: &core.TaskEvent{
				Kind:        core.TaskEnded,
				TaskID:      id,
				Description: cur.snap.Description,
				Background:  &bgTrue,
				Status:      "completed",
				Summary:     cur.snap.Summary,
			},
		})
	}

	var live int32
	for _, cur := range wt.workers {
		if cur.started && !cur.ended {
			live++
		}
	}
	wt.liveCount.Store(live)
	wt.mu.Unlock()

	for _, ev := range events {
		emit(ev)
	}
}

func defaultWorkerEndSummary(status string) string {
	switch status {
	case "failed":
		return "后台任务执行失败"
	case "stopped":
		return "后台任务已停止"
	default:
		return ""
	}
}

func firstNonEmptyStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch s := v.(type) {
			case string:
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					return trimmed
				}
			case map[string]any:
				// Nested objects such as {"url": "..."} or {"text": "..."}
				if nested := firstNonEmptyStr(s, "text", "title", "url", "name", "id"); nested != "" {
					return nested
				}
			}
		}
	}
	return ""
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case jsonNumber:
		if x, err := n.Int64(); err == nil {
			return x, true
		}
	}
	return 0, false
}

type jsonNumber interface {
	Int64() (int64, error)
}

func oneLineClip(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return s
}

func makeWorkerSummaryLine(desc, status string) string {
	if status == "" {
		return desc
	}
	return fmt.Sprintf("%s (%s)", desc, status)
}
