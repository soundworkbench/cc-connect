package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Delay adapter handling of the first native result until one more stdin frame
// has been written. This deterministically exercises admission racing a result:
// Claude has ended its native turn, but Engine has not yet observed that boundary.
func TestClaudeSteerLiveResultBoundary(t *testing.T) {
	bin := os.Getenv("CLAUDE_STEER_LIVE_CMD")
	if bin == "" {
		t.Skip("explicit live acceptance only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-p", "--model", "haiku", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--replay-user-messages", "--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--tools", "Bash", "--allowedTools", "Bash(sleep *)")
	cmd.Dir = t.TempDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal("start failed")
	}
	defer func() { stdin.Close(); cancel(); cmd.Wait() }()
	cs := &claudeSession{stdin: stdin, ctx: ctx, events: make(chan core.Event, 64), done: make(chan struct{})}
	cs.alive.Store(true)
	cs.sessionID.Store("")
	if err = cs.Send("Do not use tools or access files. Reply exactly BOUNDARY_FIRST.", "first", nil, nil); err != nil {
		t.Fatal(err)
	}
	var receipt *core.SteerReceipt
	nativeResults, terminal, supplements := 0, 0, 0
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 65536), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		var raw map[string]any
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		if raw["type"] == "result" {
			nativeResults++
			if nativeResults == 1 {
				receipt, err = cs.SteerAsync("Do not use tools or access files. Reply exactly BOUNDARY_SUPPLEMENT_ONCE.", "supplement", nil, nil)
				if err != nil || receipt == nil {
					t.Fatal("boundary input not admitted")
				}
				select {
				case <-receipt.Ack:
					t.Fatal("ack before consumption")
				default:
				}
			}
		}
		cs.handleReadLoopLine(line)
		for len(cs.events) > 0 {
			event := <-cs.events
			if event.Type == core.EventPermissionRequest || event.Type == core.EventToolUse {
				t.Fatal("unexpected tool/permission")
			}
			if event.Type == core.EventResult {
				if strings.TrimSpace(event.Content) == "BOUNDARY_SUPPLEMENT_ONCE" {
					supplements++
				}
				if event.Done {
					terminal++
				}
				if nativeResults == 1 && event.Done {
					t.Fatal("premature engine completion")
				}
			}
		}
		if nativeResults == 2 {
			select {
			case <-receipt.Ack:
			default:
				t.Fatal("missing late acknowledgement")
			}
			if supplements != 1 || terminal != 1 {
				t.Fatalf("supplements=%d terminal=%d", supplements, terminal)
			}
			stdin.Close()
			// Drain to EOF: an accidental third native turn must fail this test.
		}
	}
	if scanner.Err() != nil || ctx.Err() != nil || nativeResults != 2 || terminal != 1 {
		t.Fatalf("native_results=%d terminal=%d stream_error=%v context=%v", nativeResults, terminal, scanner.Err(), ctx.Err())
	}
	t.Logf("native_results=%d engine_terminal=%d supplement_used=%d resend=0 CLI_consumes_stdin_after_result=true", nativeResults, terminal, supplements)
}
