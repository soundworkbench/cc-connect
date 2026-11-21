package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// switchCompactPercent is the share of a model's effective window that a
// conversation may carry onto it without being compacted first. Claude Code
// compacts on its own at the window minus ~33k (reserved output plus buffer);
// a turn adds tens of thousands of tokens (measured: 316k -> 380k in one
// turn), so starting below three quarters keeps the first turns on the new
// model clear of a mid-turn compaction on a cold cache.
const switchCompactPercent = 75

var (
	_ core.SwitchContextBudget    = (*Agent)(nil)
	_ core.CarriedContextReporter = (*claudeSession)(nil)
)

// SwitchCompactLine implements core.SwitchContextBudget.
//
// The window is, in order: the configured context_window_tokens, the window
// Claude Code reported for model in an earlier result (only for models Claude
// Code recognizes; for others it reports a guess), and the model-name
// heuristic (200k, 1M for "[1m]"). The auto-compact window and the maximum
// context size the session runs with cap it, since Claude Code never lets a
// conversation grow past them.
func (a *Agent) SwitchCompactLine(model string) int {
	a.mu.Lock()
	window := a.contextWindowTokens
	maxTokens := a.maxContextTokens
	env := a.runtimeEnvLocked()
	a.mu.Unlock()

	if window <= 0 {
		window = a.learnedContextWindow(model)
	}
	if window <= 0 {
		window = claudeContextWindow(model, 0)
	}
	full := core.MergeEnv(os.Environ(), env)
	for _, limit := range []int{
		envTokenCount(full, "CLAUDE_CODE_AUTO_COMPACT_WINDOW"),
		envTokenCount(full, "CLAUDE_CODE_MAX_CONTEXT_TOKENS"),
		maxTokens,
	} {
		if limit > 0 && limit < window {
			window = limit
		}
	}
	return window * switchCompactPercent / 100
}

// rememberContextWindow records the window Claude Code reported for model.
func (a *Agent) rememberContextWindow(model string, window int) {
	if a.contextWindows == nil || window <= 0 || !claudeKnowsModel(model) {
		return
	}
	a.contextWindows.Store(contextWindowKey(model), window)
}

func (a *Agent) learnedContextWindow(model string) int {
	if a.contextWindows == nil {
		return 0
	}
	if v, ok := a.contextWindows.Load(contextWindowKey(model)); ok {
		return v.(int)
	}
	return 0
}

func contextWindowKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// claudeKnowsModel reports whether Claude Code's model catalog describes
// model, so the contextWindow it reports is the real one. For any other id it
// reports the window it assumes.
func claudeKnowsModel(model string) bool {
	base := strings.ToLower(strings.TrimSpace(model))
	base = strings.TrimSuffix(base, "[1m]")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "opus", "sonnet", "haiku", "fable", "opusplan":
		return true
	}
	return strings.HasPrefix(base, "claude-")
}

// envTokenCount parses a token count such as "400000", "400k" or "1m" from
// the last KEY= entry in env; 0 when absent or unusable.
func envTokenCount(env []string, key string) int {
	prefix := key + "="
	val := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			val = kv[len(prefix):]
		}
	}
	val = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(val, "_", "")))
	if val == "" {
		return 0
	}
	mult := 1
	switch {
	case strings.HasSuffix(val, "k"):
		mult, val = 1_000, strings.TrimSuffix(val, "k")
	case strings.HasSuffix(val, "m"):
		mult, val = 1_000_000, strings.TrimSuffix(val, "m")
	}
	n, err := strconv.ParseFloat(val, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return int(n * float64(mult))
}

// CarriedContext implements core.CarriedContextReporter: the live per-call
// prompt size while this process has one, otherwise the newest record in the
// transcript — the last call's prompt size and model, or the size a
// compaction left.
func (cs *claudeSession) CarriedContext() (tokens int, model string) {
	cs.usageMu.Lock()
	live := cs.lastUsage != nil && (cs.usageSource == "event" || cs.usageSource == "transcript")
	if live {
		tokens = cs.lastUsage.UsedTokens
	}
	cs.usageMu.Unlock()
	if live && !cs.carriedStale.Load() {
		return tokens, cs.GetModel()
	}
	return carriedFromTranscript(cs.transcriptPath())
}

// carriedFromTranscript returns the newest carried-context record of a
// transcript: an assistant call's prompt size and model, or a compaction
// boundary's post-compaction size (model "", since the summary was written
// for whichever model runs next).
func carriedFromTranscript(path string) (tokens int, model string) {
	scanTranscriptTail(path, transcriptRecoveryWindow, func(line []byte) bool {
		if u := claudeUsageFromTranscriptLine(line, 0); u != nil {
			tokens, model = u.UsedTokens, transcriptLineModel(line)
			return true
		}
		if post, ok := compactBoundaryTokens(line); ok {
			tokens, model = post, ""
			return true
		}
		return false
	})
	return tokens, model
}

func transcriptLineModel(line []byte) string {
	var rec struct {
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return ""
	}
	return rec.Message.Model
}

// compactBoundaryTokens reports whether line is a compaction boundary and the
// prompt size it left (0 when the record does not say).
func compactBoundaryTokens(line []byte) (int, bool) {
	if !bytes.Contains(line, []byte("compact_boundary")) {
		return 0, false
	}
	var rec struct {
		Type     string `json:"type"`
		Subtype  string `json:"subtype"`
		Metadata struct {
			PostTokens int `json:"postTokens"`
		} `json:"compactMetadata"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "system" || rec.Subtype != "compact_boundary" {
		return 0, false
	}
	return rec.Metadata.PostTokens, true
}
