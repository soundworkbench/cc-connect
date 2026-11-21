package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func clearWindowEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "")
	t.Setenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS", "")
}

func TestSwitchCompactLine(t *testing.T) {
	clearWindowEnv(t)
	for _, tc := range []struct {
		name     string
		agent    *Agent
		learn    map[string]int
		model    string
		wantLine int
	}{
		{"unknown model uses the 200k heuristic", &Agent{}, nil, "gemini-4-neon", 150_000},
		{"[1m] suffix", &Agent{}, nil, "gemini-4-neon[1m]", 750_000},
		{"reported window of a Claude model", &Agent{}, map[string]int{"claude-opus-5-5": 1_000_000}, "claude-opus-5-5", 750_000},
		{"reported window of an unknown model is a guess", &Agent{}, map[string]int{"gpt-6.1-sol": 1_000_000}, "gpt-6.1-sol", 150_000},
		{"auto-compact window caps", &Agent{configEnv: []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=400000"}}, map[string]int{"claude-opus-5-5": 1_000_000}, "claude-opus-5-5", 300_000},
		{"shorthand", &Agent{configEnv: []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=400k"}}, map[string]int{"claude-opus-5-5": 1_000_000}, "claude-opus-5-5", 300_000},
		{"max context tokens caps", &Agent{configEnv: []string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS=128000"}}, nil, "gpt-6.1-sol", 96_000},
		{"configured window wins", &Agent{contextWindowTokens: 400_000}, map[string]int{"claude-opus-5-5": 1_000_000}, "claude-opus-5-5", 300_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.agent.contextWindows = &sync.Map{}
			for m, w := range tc.learn {
				tc.agent.rememberContextWindow(m, w)
			}
			if got := tc.agent.SwitchCompactLine(tc.model); got != tc.wantLine {
				t.Fatalf("SwitchCompactLine(%q) = %d, want %d", tc.model, got, tc.wantLine)
			}
		})
	}
}

func TestSwitchCompactLine_ProcessEnv(t *testing.T) {
	clearWindowEnv(t)
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "400000")
	a := &Agent{contextWindows: &sync.Map{}}
	a.rememberContextWindow("claude-opus-5-5", 1_000_000)
	if got := a.SwitchCompactLine("claude-opus-5-5"); got != 300_000 {
		t.Fatalf("line = %d, want 300000", got)
	}
}

func TestEnvTokenCount(t *testing.T) {
	for in, want := range map[string]int{
		"400000": 400_000, "400k": 400_000, "400K": 400_000, "1m": 1_000_000, "1.5m": 1_500_000,
		"400_000": 400_000, "": 0, "auto": 0, "-5": 0,
	} {
		if got := envTokenCount([]string{"X=1", "K=" + in}, "K"); got != want {
			t.Errorf("envTokenCount(%q) = %d, want %d", in, got, want)
		}
	}
	if got := envTokenCount([]string{"K=1k", "K=2k"}, "K"); got != 2000 {
		t.Errorf("last entry should win, got %d", got)
	}
}

const (
	neonLine      = `{"type":"assistant","message":{"model":"gemini-4-neon","usage":{"input_tokens":2428,"cache_read_input_tokens":261654,"cache_creation_input_tokens":0,"output_tokens":10}}}`
	syntheticLine = `{"type":"assistant","message":{"model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}`
	boundaryLine  = `{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"manual","preTokens":264082,"postTokens":23776}}`
	opusLine      = `{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_read_input_tokens":80858,"cache_creation_input_tokens":0,"output_tokens":5}}}`
	userLine      = `{"type":"user","message":{"content":"hi"}}`
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCarriedFromTranscript(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lines     []string
		tokens    int
		wantModel string
	}{
		{"last call, skipping a synthetic reply", []string{userLine, neonLine, userLine, syntheticLine}, 264082, "gemini-4-neon"},
		{"compaction after the last call", []string{neonLine, boundaryLine, userLine}, 23776, ""},
		{"call after the compaction", []string{neonLine, boundaryLine, opusLine}, 80860, "claude-opus-5-5"},
		{"nothing usable", []string{userLine}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens, model := carriedFromTranscript(writeTranscript(t, tc.lines...))
			if tokens != tc.tokens || model != tc.wantModel {
				t.Fatalf("got (%d, %q), want (%d, %q)", tokens, model, tc.tokens, tc.wantModel)
			}
		})
	}
	if tokens, model := carriedFromTranscript(filepath.Join(t.TempDir(), "missing.jsonl")); tokens != 0 || model != "" {
		t.Fatalf("missing transcript: (%d, %q)", tokens, model)
	}
}

func TestCarriedContext_LiveThenCompacted(t *testing.T) {
	path := writeTranscript(t, neonLine, boundaryLine)
	cs := &claudeSession{ctx: context.Background(), events: make(chan core.Event, 8), transcriptOverride: path}
	cs.activeModel.Store("claude-opus-5-5")

	// Fresh process: nothing live yet, the transcript answers.
	if tokens, model := cs.CarriedContext(); tokens != 23776 || model != "" {
		t.Fatalf("fresh: (%d, %q)", tokens, model)
	}

	cs.usageMu.Lock()
	cs.lastUsage = &core.ContextUsage{UsedTokens: 352334}
	cs.usageSource = "event"
	cs.usageMu.Unlock()
	if tokens, model := cs.CarriedContext(); tokens != 352334 || model != "claude-opus-5-5" {
		t.Fatalf("live: (%d, %q)", tokens, model)
	}

	cs.handleSystem(map[string]any{"type": "system", "subtype": "compact_boundary"})
	if tokens, _ := cs.CarriedContext(); tokens != 23776 {
		t.Fatalf("after compact_boundary: %d, want the transcript's post-compaction size", tokens)
	}
}

func TestHandleSystem_CompactingStatusEmitsEvent(t *testing.T) {
	cs := &claudeSession{ctx: context.Background(), events: make(chan core.Event, 8)}
	cs.handleSystem(map[string]any{"type": "system", "subtype": "status", "status": "compacting"})
	cs.handleSystem(map[string]any{"type": "system", "subtype": "status", "status": nil})
	if len(cs.events) != 1 {
		t.Fatalf("events = %d, want 1", len(cs.events))
	}
	if ev := <-cs.events; ev.Type != core.EventCompacting {
		t.Fatalf("event = %v", ev.Type)
	}
}

func TestClaudeKnowsModel(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-opus-5-5": true, "claude-opus-5-5[1m]": true, "claude/claude-opus-5-5": true,
		"opus": true, "sonnet[1m]": true, "gemini-4-neon": false, "codex/gpt-6.1-sol": false, "": false,
	} {
		if got := claudeKnowsModel(model); got != want {
			t.Errorf("claudeKnowsModel(%q) = %v, want %v", model, got, want)
		}
	}
}
