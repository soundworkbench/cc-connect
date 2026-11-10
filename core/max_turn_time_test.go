package core

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedLogBuffer collects slog JSON lines written from engine goroutines.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) records(msg string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(b.buf.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func captureInfoSlog(t *testing.T) *lockedLogBuffer {
	t.Helper()
	prev := slog.Default()
	buf := &lockedLogBuffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func logDuration(t *testing.T, rec map[string]any, key string) time.Duration {
	t.Helper()
	v, ok := rec[key].(float64)
	if !ok {
		t.Fatalf("log %q has no numeric %q: %v", rec["msg"], key, rec)
	}
	return time.Duration(v)
}

// runMaxTurnScenario drives processInteractiveEvents through the initial turn
// plus len(durations)-1 queued messages. Each turn answers durations[i] after
// its prompt was sent, like an agent working on that single message.
func runMaxTurnScenario(t *testing.T, maxTurn time.Duration, durations []time.Duration) (*stubPlatformEngine, *lockedLogBuffer, int) {
	t.Helper()
	logs := captureInfoSlog(t)
	p := &stubPlatformEngine{n: "test"}
	sess := newQueuingSession("max-turn")
	e := NewEngine("test", &controllableAgent{nextSession: sess}, []Platform{p}, "", LangEnglish)
	e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
	e.SetMaxTurnTime(maxTurn)

	key := "test:user1"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx-turn0"}
	for i := 1; i < len(durations); i++ {
		state.pendingMessages = append(state.pendingMessages, queuedMessage{
			platform: p, replyCtx: "ctx-queued", content: "queued", messageID: "queued-msg",
		})
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	sendCount := func() int {
		sess.sendMu.Lock()
		defer sess.sendMu.Unlock()
		return len(sess.sendCalls)
	}
	go func() {
		for i, d := range durations {
			for sendCount() < i {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(d)
			sess.events <- Event{Type: EventText, Content: "answer"}
			sess.events <- Event{Type: EventResult, Content: "answer", Done: true}
		}
	}()

	sendDone := make(chan error, 1)
	sendDone <- nil
	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "msg0", time.Now(), nil, sendDone, nil, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("processInteractiveEvents did not return")
	}
	return p, logs, sendCount()
}

func sentContains(p *stubPlatformEngine, substr string) bool {
	for _, s := range p.getSent() {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// Queued messages processed in the same event loop are separate turns: the
// max_turn_time deadline and elapsed time restart for each of them.
func TestMaxTurnTime_ResetsForEachQueuedMessage(t *testing.T) {
	const maxTurn = 200 * time.Millisecond
	p, logs, sends := runMaxTurnScenario(t, maxTurn, []time.Duration{
		100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond,
	})

	if got := logs.records("agent turn exceeded max_turn_time: sending stop signal, will force-kill if needed"); len(got) != 0 {
		t.Fatalf("max_turn_time fired across queued messages: %v", got)
	}
	if sentContains(p, "exceeded maximum time") {
		t.Fatalf("user saw max_turn_time error: %q", p.getSent())
	}
	if sends != 3 {
		t.Fatalf("queued prompts sent = %d, want 3", sends)
	}
	turns := logs.records("turn complete")
	if len(turns) != 4 {
		t.Fatalf("turn complete logs = %d, want 4", len(turns))
	}
	for i, rec := range turns {
		if d := logDuration(t, rec, "turn_duration"); d >= maxTurn {
			t.Fatalf("turn %d turn_duration = %v, want per-message time below %v", i, d, maxTurn)
		}
	}
}

func TestMaxTurnTime_StopsSingleLongMessage(t *testing.T) {
	const maxTurn = 200 * time.Millisecond
	p, logs, _ := runMaxTurnScenario(t, maxTurn, []time.Duration{300 * time.Millisecond})

	got := logs.records("agent turn exceeded max_turn_time: sending stop signal, will force-kill if needed")
	if len(got) != 1 {
		t.Fatalf("max_turn_time stop logs = %d, want 1", len(got))
	}
	if d := logDuration(t, got[0], "elapsed"); d < maxTurn {
		t.Fatalf("elapsed = %v, want >= %v", d, maxTurn)
	}
	if !sentContains(p, "exceeded maximum time") {
		t.Fatalf("user was not told about max_turn_time: %q", p.getSent())
	}
	if len(logs.records("turn complete")) != 0 {
		t.Fatal("overlong turn completed normally")
	}
}

// A queued message that alone exceeds max_turn_time is still stopped, and the
// logged elapsed time counts only that message, not the earlier turn.
func TestMaxTurnTime_StopsLongQueuedMessageWithPerMessageElapsed(t *testing.T) {
	const maxTurn = 200 * time.Millisecond
	const first = 100 * time.Millisecond
	p, logs, sends := runMaxTurnScenario(t, maxTurn, []time.Duration{first, 300 * time.Millisecond})

	if sends != 1 {
		t.Fatalf("queued prompts sent = %d, want 1", sends)
	}
	if len(logs.records("turn complete")) != 1 {
		t.Fatal("first turn should complete before the queued message")
	}
	got := logs.records("agent turn exceeded max_turn_time: sending stop signal, will force-kill if needed")
	if len(got) != 1 {
		t.Fatalf("max_turn_time stop logs = %d, want 1", len(got))
	}
	if d := logDuration(t, got[0], "elapsed"); d < maxTurn || d >= maxTurn+first {
		t.Fatalf("elapsed = %v, want queued message only in [%v, %v)", d, maxTurn, maxTurn+first)
	}
	if !sentContains(p, "exceeded maximum time") {
		t.Fatalf("user was not told about max_turn_time: %q", p.getSent())
	}
}
