package core

import (
	"strings"
	"time"
	"unicode"
)

func (d DisplayCfg) toolProgressLimit() int {
	if d.Mode == "full" {
		return 0
	}
	if d.ToolProgressLines != nil {
		n := *d.ToolProgressLines
		if n < 0 {
			return 0
		}
		if n > 3 {
			return 3
		}
		return n
	}
	if d.Mode == "quiet" {
		return 3
	}
	return 0
}

// Only approved public descriptions can supply text. ToolInput is never read:
// it may contain commands, paths, arguments or credentials from any agent.
func publicToolProgress(event Event) string {
	if text, ok := event.Metadata["public_tool_progress"].(string); ok && strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
		// A progress entry occupies one row even for a multiline description.
		return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
	}
	key := MsgProgressOperation
	switch event.ToolName {
	case "Bash", "exec_command", "shell":
		key = MsgProgressCommand
	case "Read", "read_file":
		key = MsgProgressRead
	case "Edit", "Write", "NotebookEdit", "Patch", "apply_patch":
		key = MsgProgressEdit
	case "Grep", "Glob":
		key = MsgProgressSearchFiles
	case "WebFetch":
		key = MsgProgressFetch
	case "WebSearch":
		key = MsgProgressWebSearch
	case "Agent", "Task":
		key = MsgProgressAgent
	case "Skill":
		key = MsgProgressSkill
	case "TodoWrite":
		key = MsgProgressTodo
	case "MCP":
		key = MsgProgressMCP
	default:
		if strings.HasPrefix(event.ToolName, "mcp__") {
			key = MsgProgressMCP
		}
	}
	return NewI18n(LangChinese).T(key)
}

// replaceToolProgress coalesces bursts at the stream_preview interval. It only
// edits an existing placeholder; final delivery cancels the owned timer.
func (sp *streamPreview) replaceToolProgress(text string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.degraded || sp.previewMsgID == nil {
		return
	}
	sp.pendingToolProgress = text
	if sp.timer != nil {
		return
	}
	delay := time.Duration(sp.cfg.IntervalMs)*time.Millisecond - time.Since(sp.lastSentAt)
	if delay <= 0 {
		sp.replacePreviewLocked(text)
		return
	}
	sp.timer = time.AfterFunc(delay, func() {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		sp.timer = nil
		if sp.degraded || sp.previewMsgID == nil {
			return
		}
		sp.replacePreviewLocked(sp.pendingToolProgress)
	})
}
