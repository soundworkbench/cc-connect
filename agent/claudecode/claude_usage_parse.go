package claudecode

import (
	"fmt"
	"github.com/chenhg5/cc-connect/core"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	claudeUsageSessionWindowSeconds = 5 * 60 * 60
	claudeUsageWeekWindowSeconds    = 7 * 24 * 60 * 60
)

var (
	claudeUsagePercentRe    = regexp.MustCompile(`(?i)\b(\d{1,3})\s*%\s*used\b`)
	claudeUsageResetLineRe  = regexp.MustCompile(`(?i)^resets\s+(.+?)\s*$`)
	claudeUsageParenTZRe    = regexp.MustCompile(`^(.*?)\s*\(([^()]+)\)\s*$`)
	claudeUsageWhitespaceRe = regexp.MustCompile(`[ \t]+`)
	claudeUsageRuleLineRe   = regexp.MustCompile(`^[\p{Zs}\-─━_=]{4,}$`)
)

func normalizeClaudeUsageText(raw string) string {
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")

	out := make([]string, 0, len(lines))
	lastBlank := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if !lastBlank {
				out = append(out, "")
				lastBlank = true
			}
			continue
		}
		line = claudeUsageWhitespaceRe.ReplaceAllString(line, " ")
		if claudeUsageRuleLineRe.MatchString(line) {
			continue
		}
		out = append(out, line)
		lastBlank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func parseClaudeUsageReport(text string, now time.Time) (*core.UsageReport, error) {
	text = normalizeClaudeUsageText(text)
	if text == "" {
		return nil, fmt.Errorf("claudecode: Claude Code /usage produced empty output")
	}
	if err := detectClaudeUsageOutputError(text, ""); err != nil {
		return nil, err
	}

	report := &core.UsageReport{
		Provider: "claudecode",
	}

	lines := strings.Split(text, "\n")
	session, err := parseClaudeUsageWindow(lines, "Current session", claudeUsageSessionWindowSeconds, now)
	if err != nil {
		return nil, err
	}
	week, err := parseClaudeUsageWindow(lines, "Current week", claudeUsageWeekWindowSeconds, now)
	if err != nil {
		return nil, err
	}

	report.Buckets = []core.UsageBucket{{
		Name:    "Usage",
		Allowed: true,
		Windows: []core.UsageWindow{session, week},
	}}
	return report, nil
}

func parseClaudeUsageWindow(lines []string, header string, windowSeconds int, now time.Time) (core.UsageWindow, error) {
	start := -1
	headerLower := strings.ToLower(header)
	for i, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(lower, headerLower) {
			start = i
			break
		}
	}
	if start < 0 {
		return core.UsageWindow{}, fmt.Errorf("claudecode: missing %s block in Claude Code /usage output", header)
	}

	var (
		usedPercent *int
		resetRaw    string
	)
	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if i > start+1 && (strings.HasPrefix(lower, "current ") || strings.HasPrefix(lower, "extra usage")) {
			break
		}
		if usedPercent == nil {
			if m := claudeUsagePercentRe.FindStringSubmatch(line); len(m) == 2 {
				v, _ := strconv.Atoi(m[1])
				usedPercent = &v
				continue
			}
		}
		if resetRaw == "" {
			if m := claudeUsageResetLineRe.FindStringSubmatch(line); len(m) == 2 {
				resetRaw = strings.TrimSpace(m[1])
			}
		}
	}

	if usedPercent == nil {
		return core.UsageWindow{}, fmt.Errorf("claudecode: missing usage percentage in %s block", header)
	}

	window := core.UsageWindow{
		Name:          header,
		UsedPercent:   *usedPercent,
		WindowSeconds: windowSeconds,
	}

	if resetRaw != "" {
		resetAt, err := parseClaudeUsageResetTime(resetRaw, now)
		if err == nil {
			resetAfter := int(resetAt.Sub(now).Round(time.Second).Seconds())
			if resetAfter < 0 {
				resetAfter = 0
			}
			window.ResetAfterSeconds = resetAfter
			window.ResetAtUnix = resetAt.Unix()
		}
	}

	return window, nil
}

func parseClaudeUsageResetTime(raw string, now time.Time) (time.Time, error) {
	label := strings.TrimSpace(raw)
	loc := now.Location()
	if m := claudeUsageParenTZRe.FindStringSubmatch(label); len(m) == 3 {
		label = strings.TrimSpace(m[1])
		tzName := strings.TrimSpace(m[2])
		tzLoc, err := time.LoadLocation(tzName)
		if err != nil {
			return time.Time{}, fmt.Errorf("unknown timezone %q", tzName)
		}
		loc = tzLoc
	}

	nowInLoc := now.In(loc)
	for _, layout := range []string{"3:04pm", "3pm"} {
		parsed, err := time.ParseInLocation(layout, label, loc)
		if err != nil {
			continue
		}
		resetAt := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(), parsed.Hour(), parsed.Minute(), 0, 0, loc)
		if !resetAt.After(nowInLoc) {
			resetAt = resetAt.Add(24 * time.Hour)
		}
		return resetAt, nil
	}

	for _, layout := range []string{"Jan 2, 3:04pm", "Jan 2, 3pm"} {
		parsed, err := time.ParseInLocation(layout, label, loc)
		if err != nil {
			continue
		}
		resetAt := time.Date(nowInLoc.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), 0, 0, loc)
		if !resetAt.After(nowInLoc) {
			resetAt = resetAt.AddDate(1, 0, 0)
		}
		return resetAt, nil
	}

	return time.Time{}, fmt.Errorf("unsupported reset time %q", raw)
}

func detectClaudeUsageOutputError(screen, stderr string) error {
	joined := strings.ToLower(strings.TrimSpace(screen + "\n" + stderr))
	switch {
	case joined == "":
		return nil
	case strings.Contains(joined, "select login method") ||
		strings.Contains(joined, "choose the text style") ||
		strings.Contains(joined, "select a theme"):
		return &core.UserMessageError{Key: core.MsgUsageOnboardingIncomplete}
	case strings.Contains(joined, "unknown command") && strings.Contains(joined, "/usage"):
		return fmt.Errorf("claudecode: this Claude Code version does not support /usage; please upgrade Claude Code")
	case strings.Contains(joined, "auth login") || strings.Contains(joined, "not logged in") || strings.Contains(joined, "sign in"):
		return fmt.Errorf("claudecode: Claude Code is not logged in for /usage; run `claude auth login`")
	case strings.Contains(joined, "/usage") && (strings.Contains(joined, "not available") || strings.Contains(joined, "not supported") || strings.Contains(joined, "subscription")):
		return fmt.Errorf("claudecode: current Claude account does not support /usage")
	default:
		return nil
	}
}
