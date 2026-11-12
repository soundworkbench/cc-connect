package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A decision answer and a restart resume start as soon as the session is idle,
// even when hold_seconds is set long; an ordinary timer prompt still waits.
func TestBridgeFollowUpsSkipTheAutomationHold(t *testing.T) {
	t.Run("restart resume", func(t *testing.T) {
		dir := t.TempDir()
		cut := time.Now().Add(-2 * time.Minute)
		data, _ := json.Marshal(inflightFile{AliveAt: cut, Sessions: map[string]*inflightSession{
			"scheduled:chat:user": {Turn: &inflightTurn{Request: "整理周报", Started: cut.Add(-time.Minute)}},
		}})
		if err := AtomicWriteFile(inflightPath(dir, "scheduled"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		env := newResumeEnv(t, dir)
		env.e.queueBatch.Hold = time.Hour
		start := time.Now()
		env.e.resumeInterrupted(time.Now())
		if got := cc27Take(t, env.s.started); !strings.Contains(got, resumeMarker) {
			t.Fatalf("prompt=%q", got)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("resume waited %v", took)
		}
	})

	t.Run("plain timer still held", func(t *testing.T) {
		env := newResumeEnv(t, t.TempDir())
		env.e.queueBatch.Hold = time.Hour
		go func() {
			_ = env.e.ExecuteTimerJob(&TimerJob{ID: "j1", Project: "scheduled", SessionKey: env.key, ScheduledAt: time.Now(), Prompt: "看一下邮件", SessionMode: "reuse"})
		}()
		env.noPrompt(t)
	})

	t.Run("decision answer", func(t *testing.T) {
		e, _, sess, _ := newDecisionEngine(t, t.TempDir())
		cfg := DefaultQueueBatchConfig()
		cfg.Hold = time.Hour
		e.SetQueueBatchConfig(cfg)
		id := sendTestDecision(t, e, DecisionRequest{Question: "用哪个方案？", Options: []string{"甲", "乙"}})
		start := time.Now()
		e.handleCardNav("act:/decision "+id+" 1", decKey)
		got := waitPrompts(t, sess, 1)
		if !strings.Contains(got[0], "【决定】用哪个方案？：甲") {
			t.Fatalf("prompt=%q", got[0])
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("decision answer waited %v", took)
		}
	})
}

// With the default settings an idle timer prompt starts within a few seconds:
// long enough to merge a burst of automated input, never a minute.
func TestDefaultAutomationHoldIsAFewSeconds(t *testing.T) {
	if hold := DefaultQueueBatchConfig().Hold; hold <= 0 || hold > 3*time.Second {
		t.Fatalf("default hold = %v", hold)
	}
	env := newResumeEnv(t, t.TempDir())
	env.e.SetQueueBatchConfig(DefaultQueueBatchConfig())
	start := time.Now()
	go func() {
		_ = env.e.ExecuteTimerJob(&TimerJob{ID: "j1", Project: "scheduled", SessionKey: env.key, ScheduledAt: time.Now(), Prompt: "看一下邮件", SessionMode: "reuse"})
	}()
	select {
	case <-env.s.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timer prompt still waiting after 5s")
	}
	if took := time.Since(start); took < 2*time.Second || took > 4*time.Second {
		t.Fatalf("timer prompt started after %v, want about 3s", took)
	}
}
