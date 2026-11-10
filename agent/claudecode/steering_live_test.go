package claudecode

import (
	"context"
	"github.com/chenhg5/cc-connect/core"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClaudeSteerLive(t *testing.T) {
	cmd := os.Getenv("CLAUDE_STEER_LIVE_CMD")
	if cmd == "" {
		t.Skip("explicit live acceptance only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	cs, err := newClaudeSession(ctx, t.TempDir(), cmd, []string{"--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--tools", "Bash"}, "", "haiku", "medium", "", "default", "", "", []string{"Bash(sleep *)"}, nil, nil, nil, "", false, core.SpawnOptions{}, 0, 0, t.TempDir(), core.LangEnglish)
	if err != nil {
		t.Fatal("session start failed")
	}
	defer cs.Close()
	if err = cs.Send("Maintenance test: run Bash exactly once with sleep 20, not in background. Do not read or write files. After it completes, reply ORIGINAL unless a newer user instruction arrives.", "live-first", nil, nil); err != nil {
		t.Fatal(err)
	}
	steered := false
	var ackElapsed time.Duration
	toolCalls := 0
	ack := make(chan error, 1)
	terminal := 0
	for {
		select {
		case <-ctx.Done():
			t.Fatal("live steering timeout")
		case event, ok := <-cs.Events():
			if !ok {
				t.Fatal("session ended early")
			}
			if event.Type == core.EventToolUse {
				toolCalls++
			}
			if event.Type == core.EventToolUse && !steered {
				steered = true
				go func() {
					started := time.Now()
					receipt, e := cs.SteerAsync("Change the final answer to exactly CLAUDE_STEER_VERIFIED_926. Do not call more tools.", "live-followup", nil, nil)
					if receipt == nil && e == nil {
						e = context.Canceled
					}
					if e == nil {
						select {
						case <-receipt.Ack:
							ackElapsed = time.Since(started)
						case <-ctx.Done():
							e = ctx.Err()
						}
					}
					ack <- e
				}()
			}
			if event.Type == core.EventPermissionRequest {
				t.Fatal("unexpected permission request")
			}
			if event.Type == core.EventResult && event.Done {
				terminal++
				if !steered || strings.TrimSpace(event.Content) != "CLAUDE_STEER_VERIFIED_926" {
					t.Fatal("active turn did not use supplemental instruction")
				}
				if err := <-ack; err != nil {
					t.Fatalf("steer acknowledgement: %v", err)
				}
				if ackElapsed <= 3*time.Second {
					t.Fatalf("ack too early to exercise regression: %s", ackElapsed)
				}
				if toolCalls != 1 {
					t.Fatalf("tool calls=%d", toolCalls)
				}
				select {
				case extra := <-cs.Events():
					if extra.Type == core.EventResult && extra.Done {
						t.Fatal("duplicate terminal result")
					}
				case <-time.After(2 * time.Second):
				}
				t.Logf("ack_delay=%s tool_calls=%d", ackElapsed, toolCalls)
				t.Logf("steer_ack=true supplement_used=true terminal_results=%d session_preserved=%t", terminal, cs.CurrentSessionID() != "")
				return
			}
		}
	}
}
