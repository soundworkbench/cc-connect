package core

import (
	"context"
	"testing"
	"time"
)

func withFailedTurnLimits(t *testing.T, ttl time.Duration, max, maxBytes int, sweep time.Duration) {
	t.Helper()
	oldTTL, oldMax, oldBytes, oldSweep := failedTurnTTL, failedTurnMax, failedTurnMaxBytes, failedTurnSweep
	failedTurnTTL, failedTurnMax, failedTurnMaxBytes, failedTurnSweep = ttl, max, maxBytes, sweep
	t.Cleanup(func() {
		failedTurnTTL, failedTurnMax, failedTurnMaxBytes, failedTurnSweep = oldTTL, oldMax, oldBytes, oldSweep
	})
}

func failedWith(bytes int) *failedTurn {
	return &failedTurn{input: Message{Files: []FileAttachment{{Data: make([]byte, bytes)}}}, expires: time.Now().Add(failedTurnTTL)}
}

func TestFailedTurnsDropOldestPastCountAndBytes(t *testing.T) {
	withFailedTurnLimits(t, time.Hour, 3, 100, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var f failedTurns
	for _, tok := range []string{"a", "b", "c"} {
		if !f.put(ctx, tok, failedWith(10)) {
			t.Fatal("put refused")
		}
	}
	f.take("b") // a taken token does not count
	f.put(ctx, "d", failedWith(10))
	if f.take("a") == nil {
		t.Fatal("a dropped while under the count limit")
	}
	f.put(ctx, "e", failedWith(10))
	f.put(ctx, "g", failedWith(10)) // c, d, e, g: c is the oldest
	if f.take("c") != nil {
		t.Fatal("oldest kept past the count limit")
	}
	f.put(ctx, "big", failedWith(85)) // 3 x 10 + 85 > 100: d and e go
	if f.take("d") != nil || f.take("e") != nil || f.take("g") == nil || f.take("big") == nil {
		t.Fatal("byte limit did not drop the oldest first")
	}
	if f.put(ctx, "huge", failedWith(101)) {
		t.Fatal("a request over the byte limit was kept")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.turns) != 0 || f.bytes != 0 {
		t.Fatalf("left %d turns, %d bytes", len(f.turns), f.bytes)
	}
}

func TestFailedTurnsExpireWithoutNewFailures(t *testing.T) {
	withFailedTurnLimits(t, 30*time.Millisecond, 200, 1<<20, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var f failedTurns
	f.put(ctx, "a", failedWith(1000))
	waitUntil(t, "sweep", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.turns) == 0 && f.bytes == 0
	})
}

func TestOversizedRequestFailsWithoutRetryButton(t *testing.T) {
	withFailedTurnLimits(t, time.Hour, 200, 4, time.Hour)
	f := newFailureEnv(t)
	f.e.ReceiveMessage(f.p, &Message{SessionKey: f.key, Platform: f.p.Name(), MessageID: "1", UserID: "user", Content: "看附件", ReplyCtx: "ctx",
		Files: []FileAttachment{{FileName: "a.bin", Data: []byte("12345")}}})
	select {
	case <-f.s.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt missing")
	}
	f.s.events <- Event{Type: EventError, Error: &ModelFailure{HTTPStatus: 529}}
	f.waitIdle()
	_, failures := f.p.snapshot()
	if len(failures) != 1 || failures[0].Action != "" || failures[0].Title == "" {
		t.Fatalf("failures=%+v", failures)
	}
}
