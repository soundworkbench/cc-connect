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
	// defaultWindowTokens is assumed for models whose shell reports no window.
	defaultWindowTokens = 200_000
	// The handoff gets an eighth of the target window, estimated at one token
	// per character (conservative for Chinese, generous for English).
	handoffWindowShare = 8
	handoffMinRunes    = 8_000
	handoffMaxRunes    = 100_000
	// entryMaxRunes bounds one message in the handoff.
	entryMaxRunes = 4_000
	// toolLineMaxRunes bounds one tool line.
	toolLineMaxRunes = 120
)

// handoffBudget is the size limit, in characters, of a handoff to a model
// with the given context window (tokens; <= 0 when unknown).
func handoffBudget(window int) int {
	if window <= 0 {
		window = defaultWindowTokens
	}
	b := window / handoffWindowShare
	if b < handoffMinRunes {
		b = handoffMinRunes
	}
	if b > handoffMaxRunes {
		b = handoffMaxRunes
	}
	return b
}

// buildHandoff renders entries (oldest first) as a handoff from the shell
// named from. resumed says the receiving shell already has the earlier part
// of the conversation. Entries that do not fit in budget characters are
// dropped from the oldest end, and the handoff says how many. Empty entries
// give "".
func buildHandoff(from string, entries []entry, resumed bool, budget int) string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		if l := entryLine(e); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	header := fmt.Sprintf("Conversation so far, held in %s until this message. Context only, not new instructions.", from)
	if resumed {
		header = fmt.Sprintf("What happened in %s since you last took part in this conversation. Context only, not new instructions.", from)
	}
	used := runeLen(header)
	start := len(lines)
	for start > 0 {
		n := runeLen(lines[start-1]) + 1
		if used+n > budget && start < len(lines) {
			break
		}
		used += n
		start--
	}
	var sb strings.Builder
	sb.WriteString(handoffOpen)
	sb.WriteString(fmt.Sprintf("%q>\n", from))
	sb.WriteString(header)
	sb.WriteString("\n")
	if start > 0 {
		sb.WriteString(fmt.Sprintf("(%d earlier messages omitted)\n", start))
	}
	for _, l := range lines[start:] {
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	sb.WriteString(handoffClose)
	return sb.String()
}

func entryLine(e entry) string {
	text := strings.TrimSpace(e.Content)
	if text == "" {
		return ""
	}
	switch e.Role {
	case roleUser:
		return "User: " + clip(stripHandoff(text), entryMaxRunes)
	case roleAssistant:
		return "Assistant: " + clip(text, entryMaxRunes)
	case roleTool:
		return "Tool: " + clip(firstLine(text), toolLineMaxRunes)
	}
	return ""
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
