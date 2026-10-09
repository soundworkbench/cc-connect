package claudecode

import (
	"strings"
	"time"
)

const (
	claudeUsagePollInterval = 100 * time.Millisecond
	claudeUsageStableFor    = 450 * time.Millisecond
	claudeUsageActionGap    = 250 * time.Millisecond
)

type claudeUsageProbeState struct {
	promptResponses int
	lastPrompt      string
	sentWake        bool
	sentUsage       bool
	sentEnterRetry  bool
	sentUsageRetry  bool
	lastActionAt    time.Time
	usageSentAt     time.Time
}

func nextClaudeUsageProbeAction(screen string, state *claudeUsageProbeState, now time.Time) string {
	if now.Sub(state.lastActionAt) < claudeUsageActionGap {
		return ""
	}

	if action := promptActionForScreen(screen); action != "" {
		if screen != state.lastPrompt && state.promptResponses < 6 {
			state.promptResponses++
			state.lastPrompt = screen
			state.lastActionAt = now
			return action
		}
		return "" // Do not type /usage into a still-visible confirmation dialog.
	}

	if usageReady(screen) {
		return ""
	}

	if !state.sentWake {
		if !strings.Contains(screen, "❯") && !strings.Contains(screen, "›") {
			return ""
		}
		state.sentWake = true
	}

	if !state.sentUsage {
		state.sentUsage = true
		state.usageSentAt = now
		state.lastActionAt = now
		return "/usage\r"
	}

	if !state.sentEnterRetry && now.Sub(state.usageSentAt) >= 900*time.Millisecond {
		state.sentEnterRetry = true
		state.lastActionAt = now
		return "\r"
	}

	if !state.sentUsageRetry && now.Sub(state.usageSentAt) >= 1500*time.Millisecond {
		state.sentUsageRetry = true
		state.lastActionAt = now
		return "/usage\r"
	}

	return ""
}

func promptActionForScreen(screen string) string {
	lower := strings.ToLower(screen)
	if lower == "" {
		return ""
	}
	if strings.Contains(lower, "quick safety check") || strings.Contains(lower, "yes, i trust this folder") {
		// This probe runs in a newly-created empty directory owned by this
		// process, not an arbitrary workspace. Newer CLIs default to No/exit.
		no := strings.Index(lower, "no, exit")
		yes := strings.Index(lower, "yes, i trust this folder")
		if no >= 0 && yes > no {
			return "\x1b[B\r"
		}
		return "\r"
	}
	if (strings.Contains(lower, "telemetry") || strings.Contains(lower, "help improve") || strings.Contains(lower, "usage data")) &&
		(strings.Contains(lower, "2. no") || strings.Contains(lower, "2. disable") || strings.Contains(lower, "2. don't")) {
		return "\x1b[B\r"
	}
	if strings.Contains(lower, "enter to confirm") && !usageReady(lower) {
		return "\r"
	}
	return ""
}

func usageReady(screen string) bool {
	lower := strings.ToLower(screen)
	return strings.Contains(lower, "current session") &&
		strings.Contains(lower, "current week") &&
		strings.Contains(lower, "resets") &&
		len(claudeUsagePercentRe.FindAllString(screen, -1)) >= 2
}
