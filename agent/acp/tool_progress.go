package acp

import (
	"encoding/json"
	"net/url"
	"strings"
	"unicode"

	"github.com/chenhg5/cc-connect/core"
)

const stepLabelMaxRunes = 80

// acpToolProgressMeta gives an ACP tool call its public card wording, the
// counterpart of the Claude Code adapter's chineseToolProgress/stepToolLabel.
// The generic wording comes from the standard ACP kind; the step label is the
// agent's own description of the call. Command lines and full paths never
// reach the card: only a vetted base name or host may follow the category.
func acpToolProgressMeta(kind, title string, rawInput json.RawMessage) map[string]any {
	var input map[string]any
	_ = json.Unmarshal(rawInput, &input)

	key, detail := core.MsgProgressOperation, ""
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "execute":
		key = core.MsgProgressCommand
	case "read":
		key, detail = core.MsgProgressRead, core.ProgressBasename(inputString(input, "path", "file_path"))
	case "edit", "delete", "move":
		key, detail = core.MsgProgressEdit, core.ProgressBasename(inputString(input, "path", "file_path"))
	case "search":
		key = core.MsgProgressSearchFiles
	case "fetch":
		key = core.MsgProgressFetch
		if u, err := url.Parse(inputString(input, "url")); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
			detail = core.SafeProgressLabel(u.Hostname())
		}
	}
	// "think", "other" and unknown kinds keep the generic operation wording.
	generic := core.NewI18n(core.LangChinese).T(key)
	if detail != "" {
		generic += " " + detail
	}

	label := acpStepDescription(kind, title, input)
	public := generic
	if hasHan(label) {
		public = label
	}
	meta := map[string]any{"public_tool_progress": public}
	if label != "" {
		meta["step_label"] = label
	} else if detail != "" {
		meta["step_label"] = generic
	}
	return meta
}

// acpStepDescription returns the agent's human-written description of a call,
// or "". A summary field in the input (Antigravity's toolSummary, the
// description a Claude-style shell tool carries) is written for people and is
// used as is. The ACP title is often just the command line or a path, so it
// only counts when it reads as Chinese prose and is not part of the command
// and contains no path separator.
func acpStepDescription(kind, title string, input map[string]any) string {
	if s := oneLineLabel(inputString(input, "toolSummary")); s != "" {
		return s
	}
	if strings.EqualFold(kind, "execute") {
		if s := oneLineLabel(inputString(input, "description")); s != "" {
			return s
		}
	}
	title = oneLineLabel(title)
	if !hasHan(title) || strings.ContainsAny(title, `/\`) {
		return ""
	}
	if cmd := strings.Join(strings.Fields(inputString(input, "command", "commandLine")), " "); cmd != "" && (strings.Contains(title, cmd) || strings.Contains(cmd, title)) {
		return ""
	}
	return title
}

// inputString returns the first string field among keys, matching names
// without regard to case or underscores (CommandLine, command_line, command).
func inputString(input map[string]any, keys ...string) string {
	bare := func(s string) string { return strings.ReplaceAll(s, "_", "") }
	for _, want := range keys {
		for k, v := range input {
			if s, ok := v.(string); ok && strings.EqualFold(bare(k), bare(want)) {
				return s
			}
		}
	}
	return ""
}

func oneLineLabel(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > stepLabelMaxRunes {
		return string(r[:stepLabelMaxRunes]) + "…"
	}
	return text
}

func hasHan(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) })
}
