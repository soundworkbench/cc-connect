package orca

import (
	"fmt"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// toolStepEvent maps an Orca transcript tool entry into a cc-connect
// EventToolUse with Chinese public progress metadata so Feishu's StepList
// card renders `└ <中文工具名>`.
func toolStepEvent(progressID, toolName, _ string) core.Event {
	mappedTool, label := mapOrcaTool(toolName)
	return core.Event{
		Type:       core.EventToolUse,
		ProgressID: progressID,
		ToolName:   mappedTool,
		ToolInput:  label,
		Metadata: map[string]any{
			"public_tool_progress": label,
			"step_label":           label,
		},
	}
}

func mapOrcaTool(raw string) (toolName, label string) {
	name := strings.TrimSpace(raw)
	lower := strings.ToLower(name)
	switch lower {
	case "execute", "run_command", "bash", "shell", "exec_command", "command_execution", "run_shell_command":
		return "Bash", "执行命令"
	case "read_file", "view_file", "read", "view_file_outline", "view_code_item":
		return "Read", "读取文件"
	case "write_file", "write", "write_to_file":
		return "Write", "写入文件"
	case "edit_file", "edit", "replace_file_content", "multi_replace_file_content", "apply_patch", "patch", "notebookedit":
		return "Edit", "修改文件"
	case "grep_search", "grep", "find_by_name", "glob", "list_dir", "search_files", "file_search":
		return "Grep", "搜索文件"
	case "search_web", "web_search", "websearch":
		return "WebSearch", "联网搜索"
	case "read_url_content", "web_fetch", "webfetch", "fetch_url":
		return "WebFetch", "读取网页"
	case "invoke_subagent", "define_subagent", "manage_subagents", "agent", "task", "subagent", "run_workflow":
		return "Agent", "启动子代理"
	case "send_message":
		return "Agent", "同步子代理消息"
	case "update_steps", "todo_write", "todowrite":
		return "TodoWrite", "更新步骤"
	case "schedule", "manage_task":
		return "Task", "管理后台任务"
	case "ask_question", "askuserquestion":
		return "AskUserQuestion", "等待确认"
	case "swarm":
		return "Agent", "协调子代理"
	}

	if strings.HasPrefix(lower, "mcp__") || strings.HasPrefix(lower, "mcp.") {
		short := name
		if parts := strings.Split(name, "__"); len(parts) >= 2 && parts[1] != "" {
			short = parts[1]
		} else if i := strings.LastIndex(name, "."); i >= 0 && i+1 < len(name) {
			short = name[i+1:]
		}
		if safe := core.SafeProgressLabel(short); safe != "" {
			return "MCP", "调用工具 " + safe
		}
		return "MCP", "调用工具"
	}

	if name == "" {
		return "tool", "执行操作"
	}
	if safe := core.SafeProgressLabel(name); safe != "" {
		return name, "调用工具 " + safe
	}
	return name, "执行操作"
}

// entryKey produces a stable comparison key for an Orca transcript entry.
// Tool state transitions ("running" -> "completed") intentionally share the
// same key so one tool call emits one StepList row.
func entryKey(e orcaEntry) string {
	switch e.Kind {
	case "tool":
		return "tool:" + strings.TrimSpace(e.Name)
	case "message":
		return "msg:" + strings.TrimSpace(e.Role) + ":" + e.Text
	default:
		return fmt.Sprintf("%s:%s:%s:%s", e.Kind, e.Role, e.Name, e.Text)
	}
}

// diffEntries computes what is new in cur relative to prev (which may be a
// sliding window capped at --limit). It supports:
//   - normal appends (len(cur) > len(prev))
//   - sliding-window shifts (len(cur) == len(prev) == limit)
//   - in-place growth of the trailing assistant message (deltaText != "")
//
// Returns (trailingAssistantDelta, trailingAssistantFullText, newEntries).
func diffEntries(prev, cur []orcaEntry) (deltaText string, updatedFull string, added []orcaEntry) {
	if len(cur) == 0 {
		return "", "", nil
	}
	if len(prev) == 0 {
		return "", "", cur
	}

	prevKeys := make([]string, len(prev))
	for i, e := range prev {
		prevKeys[i] = entryKey(e)
	}
	curKeys := make([]string, len(cur))
	for i, e := range cur {
		curKeys[i] = entryKey(e)
	}

	maxOverlap := min(len(prev), len(cur))

	// First try exact suffix(prev) == prefix(cur) from largest overlap down.
	for k := maxOverlap; k >= 1; k-- {
		if equalStrings(prevKeys[len(prev)-k:], curKeys[:k]) {
			return "", "", cur[k:]
		}
	}

	// Next check if the trailing entry of prev was an assistant message that
	// grew in place (prefix match on its text) while earlier entries match.
	lastPrev := prev[len(prev)-1]
	if lastPrev.Kind == "message" && lastPrev.Role == "assistant" {
		for k := maxOverlap; k >= 1; k-- {
			// Earlier k-1 entries must match exactly.
			if k > 1 && !equalStrings(prevKeys[len(prev)-k:len(prev)-1], curKeys[:k-1]) {
				continue
			}
			cand := cur[k-1]
			if cand.Kind == "message" && cand.Role == "assistant" &&
				len(cand.Text) > len(lastPrev.Text) &&
				strings.HasPrefix(cand.Text, lastPrev.Text) {
				return cand.Text[len(lastPrev.Text):], cand.Text, cur[k:]
			}
		}
	}

	// No overlap at all (e.g. >limit entries arrived between polls).
	return "", "", cur
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
