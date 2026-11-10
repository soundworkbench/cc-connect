package claudecode

import (
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// chineseToolProgress changes only public tool-use summaries. Permission input,
// private thinking, and the assistant's public text/final response are untouched.
func chineseToolProgress(tool string, input any, title string) string {
	m, _ := input.(map[string]any)
	for _, text := range []string{strVal(m, "description"), title, strVal(m, "title")} {
		if strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
			return text
		}
	}
	key, label := core.MsgProgressOperation, ""
	switch tool {
	case "Bash":
		key = core.MsgProgressCommand // Never inspect or display command arguments.
	case "Read":
		key, label = core.MsgProgressRead, progressBasename(strVal(m, "file_path"))
	case "Edit", "Write", "NotebookEdit":
		key = core.MsgProgressEdit
		fp := strVal(m, "file_path")
		if tool == "NotebookEdit" {
			fp = strVal(m, "notebook_path")
		}
		label = progressBasename(fp)
	case "Grep", "Glob":
		key = core.MsgProgressSearchFiles
	case "WebFetch":
		key = core.MsgProgressFetch
		if u, err := url.Parse(strVal(m, "url")); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
			label = safeProgressLabel(u.Hostname()) // No userinfo, port, path, query or fragment.
		}
	case "WebSearch":
		key = core.MsgProgressWebSearch
	case "Agent", "Task":
		key = core.MsgProgressAgent
	case "Skill":
		key, label = core.MsgProgressSkill, safeProgressLabel(strVal(m, "skill"))
	case "TodoWrite":
		key = core.MsgProgressTodo
	default:
		if strings.HasPrefix(tool, "mcp__") {
			key = core.MsgProgressMCP
			parts := strings.SplitN(strings.TrimPrefix(tool, "mcp__"), "__", 2)
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				label = safeProgressLabel(parts[0])
			}
		}
	}
	// This deployment explicitly requires Chinese regardless of conversation locale.
	text := core.NewI18n(core.LangChinese).T(key)
	if label != "" {
		text += " " + label
	}
	return text
}

func progressBasename(value string) string {
	return safeProgressLabel(path.Base(strings.ReplaceAll(value, `\`, "/")))
}

// Labels are optional. Reject suspicious names rather than echoing arbitrary
// input, shell syntax, credentials or opaque token-like strings into cards.
func safeProgressLabel(value string) string {
	if value == "" || value == "." || value == ".." || utf8.RuneCountInString(value) > 64 {
		return ""
	}
	lower := strings.ToLower(value)
	for _, sensitive := range []string{".env", "secret", "token", "password", "credential", "api_key", "apikey", "sk-", "ghp_", "bearer", "id_rsa", "id_ed25519"} {
		if strings.Contains(lower, sensitive) {
			return ""
		}
	}
	run := 0
	for _, r := range value {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-", r)) {
			return ""
		}
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			run++
		} else {
			run = 0
		}
		if run >= 24 {
			return ""
		}
	}
	return value
}

const stepLabelMaxRunes = 80

// stepToolLabel describes a tool call for the step-list card in the user's own
// terms: the model's description of a command or subagent, the base name of a
// file, the host of a page, the search query or pattern. It returns "" when the
// input offers nothing, and the caller falls back to the generic tool wording.
// The label stays on one line and is clipped.
func stepToolLabel(tool string, input any) string {
	m, _ := input.(map[string]any)
	zh := core.NewI18n(core.LangChinese)
	with := func(key core.MsgKey, detail string) string {
		if detail = oneLineLabel(detail); detail != "" {
			return zh.T(key) + " " + detail
		}
		return ""
	}
	switch tool {
	case "Bash", "Agent", "Task":
		return oneLineLabel(strVal(m, "description"))
	case "Read":
		return with(core.MsgProgressRead, path.Base(strings.ReplaceAll(strVal(m, "file_path"), `\`, "/")))
	case "Edit", "Write":
		return with(core.MsgProgressEdit, path.Base(strings.ReplaceAll(strVal(m, "file_path"), `\`, "/")))
	case "NotebookEdit":
		return with(core.MsgProgressEdit, path.Base(strings.ReplaceAll(strVal(m, "notebook_path"), `\`, "/")))
	case "Grep", "Glob":
		return with(core.MsgProgressSearchFiles, strVal(m, "pattern"))
	case "WebFetch":
		if u, err := url.Parse(strVal(m, "url")); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
			return with(core.MsgProgressFetch, u.Hostname())
		}
	case "WebSearch":
		return with(core.MsgProgressWebSearch, strVal(m, "query"))
	}
	return ""
}

func oneLineLabel(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "." || text == "/" {
		return ""
	}
	if r := []rune(text); len(r) > stepLabelMaxRunes {
		return string(r[:stepLabelMaxRunes]) + "…"
	}
	return text
}
