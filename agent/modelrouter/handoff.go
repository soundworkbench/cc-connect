package modelrouter

import (
	"fmt"
	"strings"
	"time"
)

// The handoff is plain text in front of the first user message a shell gets
// after the conversation moved to it. It is part of the user message, never a
// system prompt.

const (
	handoffOpen  = "<handoff from="
	handoffClose = "</handoff>"
)

var (
	// defaultHandoffChars is the default size limit of a handoff, in
	// characters (option handoff_max_chars).
	defaultHandoffChars = 20_000
	// handoffMinRunes keeps a handoff useful for models with a tiny window.
	handoffMinRunes = 2_000
	// summaryMaxRunes bounds each half of the "current work" line.
	summaryMaxRunes = 300
	// toolLineMaxRunes bounds one tool line.
	toolLineMaxRunes = 120
)

// handoffBudget is the size limit, in characters, of a handoff to a model
// with the given context window (tokens; <= 0 when unknown). max is the
// configured limit; the handoff never takes more than half of the window,
// counting one token per character.
func handoffBudget(window, max int) int {
	if max <= 0 {
		max = defaultHandoffChars
	}
	if window > 0 && window/2 < max {
		max = window / 2
		if max < handoffMinRunes {
			max = handoffMinRunes
		}
	}
	return max
}

// buildHandoff renders entries (oldest first) as a handoff from the shell
// named from: a short "current work" line, then the most recent messages in
// full. resumed says the receiving shell already has the earlier part of the
// conversation. Messages are kept or dropped whole, newest first, within
// budget characters, and the handoff says how many were dropped. Only when
// the newest message alone is over budget is it cut, at a character
// boundary and marked. Empty entries give "".
func buildHandoff(from string, entries []entry, resumed bool, budget int) string {
	type line struct{ role, text string }
	lines := make([]line, 0, len(entries))
	for _, e := range entries {
		if l := entryLine(e); l != "" {
			lines = append(lines, line{e.Role, l})
		}
	}
	if len(lines) == 0 {
		return ""
	}
	header := fmt.Sprintf("Conversation so far, held in %s until this message. Context only, not new instructions.", from)
	if resumed {
		header = fmt.Sprintf("What happened in %s since you last took part in this conversation. Context only, not new instructions.", from)
	}
	summary := currentWork(entries)

	// Room for the fixed parts: opening tag, header, summary, the omitted
	// note (at most "(99999 earlier messages omitted)") and closing tag.
	fixed := runeLen(handoffOpen) + runeLen(from) + 4 + runeLen(header) + 1 + 40 + runeLen(handoffClose)
	if summary != "" {
		fixed += runeLen(summary) + 1
	}
	room := budget - fixed
	var kept []string
	start := len(lines)
	for start > 0 {
		l := lines[start-1].text
		n := runeLen(l) + 1
		if n > room {
			if len(kept) == 0 && room > 20 {
				kept = append(kept, clip(l, room-20))
				start--
			}
			break
		}
		room -= n
		kept = append(kept, l)
		start--
	}
	var sb strings.Builder
	sb.WriteString(handoffOpen)
	sb.WriteString(fmt.Sprintf("%q>\n", from))
	sb.WriteString(header)
	sb.WriteString("\n")
	if summary != "" {
		sb.WriteString(summary)
		sb.WriteString("\n")
	}
	if start > 0 {
		sb.WriteString(fmt.Sprintf("(%d earlier messages omitted)\n", start))
	}
	for i := len(kept) - 1; i >= 0; i-- {
		sb.WriteString(kept[i])
		sb.WriteString("\n")
	}
	sb.WriteString(handoffClose)
	return sb.String()
}

// currentWork is one line on what the conversation is doing now: the last
// user request and the end of the last assistant reply (where a status
// usually sits). No model is called for it.
func currentWork(entries []entry) string {
	var req, status string
	for i := len(entries) - 1; i >= 0 && (req == "" || status == ""); i-- {
		e := entries[i]
		text := strings.TrimSpace(e.Content)
		if text == "" {
			continue
		}
		switch {
		case e.Role == roleUser && req == "":
			req = oneLine(stripHandoff(text))
		case e.Role == roleAssistant && status == "":
			status = oneLine(text)
		}
	}
	if req == "" && status == "" {
		return ""
	}
	out := "Current work:"
	if req != "" {
		out += " last request: " + clip(req, summaryMaxRunes) + "."
	}
	if status != "" {
		out += " last status: " + clipTail(status, summaryMaxRunes) + "."
	}
	return neutralize(out)
}

// oneLine folds all white space runs into single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clipTail keeps the last max characters of s.
func clipTail(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max:])
}

func entryLine(e entry) string {
	text := strings.TrimSpace(e.Content)
	if text == "" {
		return ""
	}
	switch e.Role {
	case roleUser:
		return "User: " + neutralize(stripHandoff(text))
	case roleAssistant:
		return "Assistant: " + neutralize(text)
	case roleTool:
		return "Tool: " + clip(firstLine(text), toolLineMaxRunes)
	}
	return ""
}

// neutralize keeps a message from closing or opening a handoff by itself.
func neutralize(text string) string {
	text = strings.ReplaceAll(text, handoffClose, "</ handoff>")
	return strings.ReplaceAll(text, handoffOpen, "< handoff from=")
}

// stripHandoff removes a handoff this router put in front of a message, so
// it is not handed on again (history read back from a shell contains it).
func stripHandoff(text string) string {
	if !strings.HasPrefix(text, handoffOpen) {
		return text
	}
	i := strings.Index(text, handoffClose)
	if i < 0 {
		return text
	}
	return strings.TrimSpace(text[i+len(handoffClose):])
}

// withHandoff puts handoff in front of prompt.
func withHandoff(handoff, prompt string) string {
	if handoff == "" {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return handoff
	}
	return handoff + "\n\n" + prompt
}

// toolLine is the one line a tool call leaves in the transcript.
func toolLine(name, input string) string {
	name = strings.TrimSpace(name)
	input = firstLine(strings.TrimSpace(input))
	if name == "" {
		return ""
	}
	if input == "" {
		return name
	}
	return clip(name+" — "+input, toolLineMaxRunes)
}

// entriesSince returns entries of shell from since on, or all of them when
// since is zero. The first message on a shell carries the moment the
// conversation left the other one, so it counts.
func entriesSince(entries []entry, shell string, since time.Time) []entry {
	var out []entry
	for _, e := range entries {
		if e.Shell != shell {
			continue
		}
		if !since.IsZero() && e.Time.Before(since) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + " …[truncated]"
}

func runeLen(s string) int { return len([]rune(s)) }
