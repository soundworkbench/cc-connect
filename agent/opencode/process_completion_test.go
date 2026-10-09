package opencode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestProcessCompletion_DrainsFinalTextAndChecksExit(t *testing.T) {
	for _, exit := range []string{"0", "7"} {
		t.Run(exit, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "opencode")
			body := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"step_start\",\"sessionID\":\"s\"}' '{\"type\":\"step_finish\",\"part\":{\"reason\":\"stop\"}}'\nsleep 0.05\nprintf '%s\\n' '{\"type\":\"text\",\"part\":{\"text\":\"late-final\"}}'\necho 'diagnostic' >&2\nexit " + exit + "\n"
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			s, err := newOpencodeSession(context.Background(), script, nil, dir, "", "default", "", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.Send("hi", "m", nil, nil); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { s.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("process never completed")
			}
			var got []core.EventType
			for len(s.events) > 0 {
				got = append(got, (<-s.events).Type)
			}
			if len(got) != 2 || got[0] != core.EventText {
				t.Fatalf("late text missing or early completion: %v", got)
			}
			want := core.EventResult
			if exit != "0" {
				want = core.EventError
			}
			if got[1] != want {
				t.Fatalf("exit %s: %v", exit, got)
			}
		})
	}
}
