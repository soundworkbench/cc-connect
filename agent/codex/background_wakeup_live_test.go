package codex

import (
	"context"
	"github.com/chenhg5/cc-connect/core"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in only: two tiny real tasks, no production thread or workspace.
func TestBackgroundWakeupLive_TwoBatches(t *testing.T) {
	if os.Getenv("CC_CONNECT_WAKEUP_LIVE") != "1" {
		t.Skip("requires explicit live authorization")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	s, err := newAppServerSession(ctx, "", t.TempDir(), "gpt-6-astra", "low", "suggest", "", "", "", nil, os.Getenv("CODEX_HOME"), "", "")
	if err != nil {
		t.Fatal("live session initialization failed")
	}
	defer s.Close()
	s.startBackgroundWakeup(backgroundWakeupPolicy{enabled: true, maxTurns: 3, lifetime: 3 * time.Minute})
	prompt := `Infrastructure acceptance test, no business work. Batch 1: spawn exactly one child, ask it to sleep 5 seconds using the sleep tool then reply BATCH_ONE_DONE. Immediately finish your parent turn with DISPATCH_ONE, without waiting. When a subsequent background completion event prompts you, collect that child's result, close it, spawn batch 2: one child that sleeps 5 seconds then replies BATCH_TWO_DONE; immediately finish with DISPATCH_TWO, without waiting. On the next completion event collect and close the second child and reply ALL_BATCHES_DONE. There are only two batches. No file edits, network, MCP or additional goals.`
	if err = s.Send(prompt, "", nil, nil); err != nil {
		t.Fatal("initial send failed")
	}
	seen := ""
	results := 0
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("two-batch acceptance timed out: results=%d", results)
		case e := <-s.Events():
			if e.Type == core.EventError {
				t.Fatal("live model returned an error")
			}
			if e.Type == core.EventText {
				seen += "\n" + e.Content
			}
			if e.Type == core.EventResult {
				results++
				if strings.Contains(seen, "ALL_BATCHES_DONE") {
					s.stateMu.Lock()
					used := s.wakeup.used
					s.stateMu.Unlock()
					if used < 2 || results < 3 {
						t.Fatal("not two automatic wakeups")
					}
					t.Logf("two batches verified, automatic wakeups=%d, completed turns=%d", used, results)
					return
				}
			}
		}
	}
}

func TestBackgroundWakeupLive_StopBeforeCompletion(t *testing.T) {
	if os.Getenv("CC_CONNECT_WAKEUP_LIVE") != "1" {
		t.Skip("requires explicit live authorization")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s, err := newAppServerSession(ctx, "", t.TempDir(), "gpt-6-astra", "low", "suggest", "", "", "", nil, os.Getenv("CODEX_HOME"), "", "")
	if err != nil {
		t.Fatal("live session initialization failed")
	}
	defer s.Close()
	s.startBackgroundWakeup(backgroundWakeupPolicy{enabled: true, maxTurns: 3, lifetime: 3 * time.Minute})
	if err = s.Send("Infrastructure test only: spawn one child to sleep 25 seconds then reply CHILD_DONE. Immediately finish with DISPATCHED without waiting. No file or network access.", "", nil, nil); err != nil {
		t.Fatal("initial send failed")
	}
	for {
		select {
		case <-ctx.Done():
			t.Fatal("dispatch timed out")
		case event := <-s.Events():
			if event.Type == core.EventError {
				t.Fatal("dispatch failed")
			}
			if event.Type == core.EventResult {
				goto dispatched
			}
		}
	}
dispatched:
	if err = s.Send("停止自动推进。不要再派发任何子代理，只回复 STOPPED。", "", nil, nil); err != nil {
		t.Fatal("stop send failed")
	}
	until := time.NewTimer(40 * time.Second)
	defer until.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("stop test timed out")
		case <-until.C:
			s.stateMu.Lock()
			armed, used := s.wakeup.armed, s.wakeup.used
			s.stateMu.Unlock()
			if armed || used != 0 {
				t.Fatal("stop did not prevent automatic wakeups")
			}
			t.Log("stop verified: no automatic restart during delayed child completion window")
			return
		case event := <-s.Events():
			if event.Type == core.EventError {
				t.Fatal("live stop response failed")
			}
		}
	}
}
