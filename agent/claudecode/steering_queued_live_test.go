package claudecode

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// A steer written while the CLI is inside a model call is queued by the CLI and
// run as the next turn. That turn can stay silent for longer than steerGrace (a
// slow tool or model call). The message must still be delivered exactly once and
// no error may be reported.
func TestClaudeSteerLiveQueuedNextTurnSilent(t *testing.T) {
	bin := os.Getenv("CLAUDE_STEER_LIVE_CMD")
	if bin == "" {
		t.Skip("explicit live acceptance only")
	}
	setSteerGrace(t, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 170*time.Second)
	defer cancel()
	cs, err := newClaudeSession(ctx, t.TempDir(), bin, []string{"--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--tools", "Bash"}, "", "haiku", "medium", "", "bypassPermissions", "", "", []string{"Bash(sleep *)"}, nil, nil, nil, "", false, core.SpawnOptions{}, 0, 0, t.TempDir(), core.LangEnglish)
	if err != nil {
		t.Fatal("session start failed")
	}
	defer cs.Close()
	if err = cs.Send("Write the numbers 1 to 300, one per line, each followed by a short English word. Do not use tools.", "first", nil, nil); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(4 * time.Second)
		cs.SteerAsync("Run Bash exactly once with sleep 10, not in background. Then reply exactly QUEUED_STEER_OK.", "second", nil, nil)
	}()
	toolCalls, terminal, answers := 0, 0, 0
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout tools=%d terminal=%d answers=%d", toolCalls, terminal, answers)
		case ev, ok := <-cs.Events():
			if !ok {
				t.Fatal("session ended early")
			}
			switch ev.Type {
			case core.EventError:
				t.Fatalf("error reported: %v (tools=%d)", ev.Error, toolCalls)
			case core.EventToolUse:
				toolCalls++
			case core.EventResult:
				if strings.Contains(ev.Content, "QUEUED_STEER_OK") {
					answers++
				}
				if ev.Done {
					terminal++
				}
			}
			if ev.Type == core.EventResult && ev.Done && answers > 0 {
				// Allow a late duplicate to show up before judging.
				deadline := time.After(15 * time.Second)
			drain:
				for {
					select {
					case ev2 := <-cs.Events():
						if ev2.Type == core.EventError {
							t.Fatalf("error after delivery: %v", ev2.Error)
						}
						if ev2.Type == core.EventToolUse {
							toolCalls++
						}
					case <-deadline:
						break drain
					}
				}
				if toolCalls != 1 || answers != 1 {
					t.Fatalf("delivered more or less than once: tools=%d answers=%d", toolCalls, answers)
				}
				t.Logf("tools=%d answers=%d terminal=%d errors=0", toolCalls, answers, terminal)
				return
			}
		}
	}
}
