package core

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	sp.acknowledgeVisible(text)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.queueProgressLocked(text)
}

func (sp *streamPreview) queueProgressLocked(text string) {
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

// streamBlock shows the block the agent is still writing below the progress
// rows. It shares queueProgressLocked's throttle and pending text, so the next
// progress row (or the final reply) simply supersedes the partial block.
func (sp *streamPreview) streamBlock(block string) {
	block, _ = stripTrailingSilent(block)
	block = strings.TrimSpace(block)
	if block == "" || isEllipsisOnly(block) {
		return
	}
	sp.acknowledgeVisible(block)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.degraded || sp.previewMsgID == nil {
		return
	}
	text := block
	if rows := sp.progressTextLocked(); strings.TrimSpace(rows) != "" {
		text = rows + "\n\n" + block
	}
	if !progressFits(text) {
		return
	}
	sp.queueProgressLocked(text)
}

func (d DisplayCfg) appendProgress() bool {
	return d.Mode != "full" && (d.ProgressHistory == "append" || (d.ProgressHistory == "" && d.Mode == "quiet"))
}

// Budget leaves room for the card envelope, status/footer and escaping inside
// the API's JSON string. Source: Feishu im-v1/message/create (30 KB card body).
const progressMaxRunes = 8000
const progressMaxJSONBytes = 20000

func progressFits(text string) bool {
	encoded, _ := json.Marshal(text)
	encoded, _ = json.Marshal(string(encoded)) // content is itself a JSON string in the request
	return len([]rune(text)) <= progressMaxRunes && len(encoded) <= progressMaxJSONBytes
}

func (sp *streamPreview) progressTextLocked() string {
	text := strings.Join(sp.progressRows, "\n")
	if sp.progressFolded > 0 {
		text = NewI18n(LangChinese).Tf(MsgProgressFolded, sp.progressFolded) + "\n" + text
	}
	return text
}

// A source ID distinguishes separate operations with identical public wording.
// Older adapters without IDs deduplicate identical public events within the turn;
// only public data participates in that fallback (never raw input or thinking).
func (sp *streamPreview) appendProgressEvent(event Event, text string) {
	if event.Type != EventProgress && event.Type != EventToolUse {
		return
	}
	text, _ = stripTrailingSilent(text)
	text = strings.TrimSpace(text)
	if text == "" || isEllipsisOnly(text) {
		return
	}
	sp.acknowledgeVisible(text)
	key := string(event.Type) + ":" + event.ProgressID
	if event.ProgressID == "" {
		key = fmt.Sprintf("%s:%x", event.Type, sha256.Sum256([]byte(event.ToolName+"\x00"+text)))
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	// A hidden preview still records rows so substantive progress is merged
	// into the final reply; queueProgressLocked never sends without a handle.
	if sp.degraded || (sp.previewMsgID == nil && !sp.hidden) {
		return
	}
	if sp.progressSeen == nil {
		sp.progressSeen = make(map[string]struct{})
	}
	if _, seen := sp.progressSeen[key]; seen {
		return
	}
	sp.progressSeen[key] = struct{}{}
	// Retain the original public segment before card size limits split/fold it.
	if sp.mergeCfg.Enabled && substantiveProgress(text, sp.mergeCfg) {
		sp.substantiveProgress = append(sp.substantiveProgress, text)
	}
	if sp.segmented != nil {
		if event.Type == EventToolUse {
			sp.addToolProgressLocked(text)
		} else {
			sp.replacePreviewLocked(text)
		}
		return
	}
	// Split unusually large entries before displaying them. Existing visible rows
	// are never rewritten: only complete oldest rows can be folded later.
	runes := []rune(text)
	for len(runes) > 0 {
		n := min(len(runes), 2000)
		sp.progressRows = append(sp.progressRows, string(runes[:n]))
		runes = runes[n:]
	}
	for !progressFits(sp.progressTextLocked()) && len(sp.progressRows) > 1 {
		sp.progressRows[0] = "" // release folded text rather than retaining it in the backing slice
		sp.progressRows = sp.progressRows[1:]
		sp.progressFolded++
	}
	sp.queueProgressLocked(sp.progressTextLocked())
}
