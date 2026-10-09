package claudecode

import "strings"

// Model IDs are explicit. Floating aliases follow the installed native CLI.
// ultracode is workflow orchestration, not an API reasoning effort.
func modelReasoningEfforts(model string) []string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimSuffix(m, "[1m]")
	if m == "haiku" || strings.HasPrefix(m, "claude-haiku-") {
		return nil
	}
	modern := m == "fable" || m == "best" || m == "opus" || m == "sonnet" ||
		strings.HasPrefix(m, "claude-fable-5") || strings.HasPrefix(m, "claude-opus-5") ||
		strings.HasPrefix(m, "claude-opus-4-7") || strings.HasPrefix(m, "claude-opus-4-8") ||
		strings.HasPrefix(m, "claude-sonnet-5")
	if modern {
		return []string{"low", "medium", "high", "xhigh", "max"}
	}
	return []string{"low", "medium", "high", "max"}
}
