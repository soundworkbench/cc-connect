package t3

import (
	"encoding/json"
	"path"
	"strings"
	"unicode"

	"github.com/chenhg5/cc-connect/core"
)

const stepLabelMaxRunes = 60

// t3Step is what a T3 tool item says about itself. T3 drops the agent's own
// step description (Antigravity's toolSummary), so the card wording is built
// from the category plus a vetted detail: a program or file base name, a tool
// name, a search query or a delegated task's title. Full command lines and
// paths never reach the card.
type t3Step struct {
	Type     string          `json:"type"`
	Status   string          `json:"status"`
	Title    string          `json:"title"`
	ToolName string          `json:"toolName"`
	Input    json.RawMessage `json:"input"`
	FileName string          `json:"fileName"`
	Pattern  string          `json:"pattern"`
}

// stepEvent returns the progress event for a tool item, and false while the
// item is still too bare to describe (T3 sends it again as it fills in).
func stepEvent(id string, st t3Step) (core.Event, bool) {
	var input map[string]any
	var inputText string
	if json.Unmarshal(st.Input, &input) != nil {
		_ = json.Unmarshal(st.Input, &inputText)
	}
	ready := runTerminal(st.Status) || requestSettled(st.Status)

	var tool, detail string
	key := core.MsgProgressOperation
	switch st.Type {
	case "command_execution":
		tool, key = "Bash", core.MsgProgressCommand
		detail = core.SafeProgressLabel(program(inputText))
		ready = ready || inputText != ""
	case "file_change":
		tool, key = "Edit", core.MsgProgressEdit
		detail = core.ProgressBasename(st.FileName)
		ready = ready || st.FileName != ""
	case "file_search":
		tool = "Grep"
		key = core.MsgProgressSearchFiles
		if strings.ContainsAny(strings.TrimSpace(st.Pattern), " \"") {
			tool, key = "WebSearch", core.MsgProgressWebSearch
		}
		detail = query(st.Pattern)
		ready = ready || st.Pattern != ""
	case "subagent":
		tool, key = "Agent", core.MsgProgressAgent
		detail = query(st.Title)
		ready = ready || st.Title != ""
	case "dynamic_tool":
		tool = st.ToolName
		file := field(input, "path", "file_path", "filePath")
		switch strings.ToLower(st.ToolName) {
		case "read", "read_file", "view_file":
			key, detail = core.MsgProgressRead, core.ProgressBasename(file)
			ready = ready || file != ""
		case "edit", "write", "write_file", "replace_file_content":
			key, detail = core.MsgProgressEdit, core.ProgressBasename(file)
			ready = ready || file != ""
		default:
			key = core.MsgProgressMCP
			name := st.ToolName
			if i := strings.LastIndex(name, "."); i >= 0 {
				name = name[i+1:]
			}
			detail = core.SafeProgressLabel(name)
			ready = ready || st.ToolName != ""
		}
	default:
		return core.Event{}, false
	}
	if !ready {
		return core.Event{}, false
	}
	if tool == "" {
		tool = "tool"
	}
	label := core.NewI18n(core.LangChinese).T(key)
	if detail != "" {
		label += " " + detail
	}
	return core.Event{Type: core.EventToolUse, ProgressID: id, ToolName: tool, ToolInput: label,
		Metadata: map[string]any{"public_tool_progress": label, "step_label": label}}, true
}

// program is the base name of the program a command line runs, past any
// leading "cd dir &&", environment assignments and sudo.
func program(cmd string) string {
	fields := strings.Fields(cmd)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "cd" || f == "pushd":
			for i < len(fields) && fields[i] != "&&" && fields[i] != ";" {
				i++
			}
		case f == "sudo" || f == "env" || f == "&&" || f == ";" || strings.HasPrefix(f, "-") || strings.Contains(f, "="):
		default:
			return path.Base(strings.Trim(f, `"'()`))
		}
	}
	return ""
}

// query is a search query or task title shortened to one card row, or ""
// when it looks like it carries a credential.
func query(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	lower := strings.ToLower(s)
	for _, sensitive := range []string{"secret", "token", "password", "credential", "api_key", "apikey", "sk-", "ghp_", "bearer"} {
		if strings.Contains(lower, sensitive) {
			return ""
		}
	}
	if r := []rune(s); len(r) > stepLabelMaxRunes {
		s = string(r[:stepLabelMaxRunes]) + "…"
	}
	return strings.TrimFunc(s, unicode.IsSpace)
}

func field(input map[string]any, keys ...string) string {
	for _, want := range keys {
		for k, v := range input {
			if s, ok := v.(string); ok && strings.EqualFold(k, want) {
				return s
			}
		}
	}
	return ""
}
