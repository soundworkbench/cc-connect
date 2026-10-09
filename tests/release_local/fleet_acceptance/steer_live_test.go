package fleet_acceptance

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

// Opt-in real Codex protocol test. No chat messages are published.
func TestFleetLiveSteering(t *testing.T) {
	path := os.Getenv("STEER_LIVE_CONFIG")
	if path == "" {
		t.Skip("explicit live steering acceptance only")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal("cannot load selected configuration")
	}
	project := cfg.Projects[0]
	opts := project.Agent.Options
	opts["backend"] = "app_server"
	opts["work_dir"] = t.TempDir()
	opts["cc_data_dir"] = t.TempDir()
	opts["system_prompt"] = ""
	opts["append_system_prompt"] = ""
	a, err := core.CreateAgent(project.Agent.Type, opts)
	if err != nil {
		t.Fatalf("agent initialization: %s", safe(err.Error()))
	}
	defer a.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	s, err := a.StartSession(ctx, "")
	if err != nil {
		t.Fatalf("start: %s", safe(err.Error()))
	}
	defer s.Close()
	steerer, ok := s.(core.AgentSessionSteerer)
	if !ok {
		t.Fatal("live session has no steering capability")
	}
	threadID := s.CurrentSessionID()
	if threadID == "" {
		t.Fatal("empty thread")
	}
	if err = s.Send("This is an isolated message steering test. Run the shell command sleep 12 exactly once, then reply only STEER_ORIGINAL. While waiting, apply any additional user instructions. Do not read or write files, use network tools, or delegate.", "live-first", nil, nil); err != nil {
		t.Fatalf("send: %s", safe(err.Error()))
	}
	steered := false
	var answer strings.Builder
	for {
		select {
		case <-ctx.Done():
			t.Fatal("live steering turn timed out")
		case event := <-s.Events():
			switch event.Type {
			case core.EventToolUse:
				if !steered {
					for i, prompt := range []string{"Change the final marker to STEER_FIRST.", "Final correction: reply only STEER_SECOND."} {
						accepted, err := steerer.Steer(prompt, "live-addition", nil, nil)
						if err != nil || !accepted {
							t.Fatalf("addition %d unconfirmed: accepted=%v err=%s", i, accepted, safeError(err))
						}
					}
					steered = true
				}
			case core.EventText:
				answer.WriteString(event.Content)
			case core.EventError:
				t.Fatalf("live event error: %s", safeError(event.Error))
			case core.EventResult:
				if !steered {
					t.Fatal("turn completed before the two additions")
				}
				text := answer.String() + event.Content
				if !strings.Contains(text, "STEER_SECOND") || strings.Contains(text, "STEER_ORIGINAL") {
					t.Fatalf("final marker mismatch: %s", safe(text))
				}
				if s.CurrentSessionID() != threadID {
					t.Fatal("steering changed thread")
				}
				reporter, ok := s.(core.UsageReporter)
				if !ok {
					t.Fatal("usage reporter missing")
				}
				usage, err := reporter.GetUsage(ctx)
				if err != nil || usage == nil {
					t.Fatalf("usage unavailable: %s", safeError(err))
				}
				if err = s.Close(); err != nil {
					t.Fatal("close failed")
				}
				resumed, err := a.StartSession(ctx, threadID)
				if err != nil {
					t.Fatalf("resume failed: %s", safe(err.Error()))
				}
				if resumed.CurrentSessionID() != threadID {
					t.Fatal("resume lost conversation")
				}
				_ = resumed.Close()
				t.Log("STEER_ACCEPTANCE two_additions_acknowledged=true same_turn_final=STEER_SECOND usage=true resume=true")
				return
			}
		}
	}
}

func safeError(err error) string {
	if err == nil {
		return "none"
	}
	return safe(err.Error())
}
