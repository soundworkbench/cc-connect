package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func steerQueueFixture(t *testing.T) (*Engine, *steeringJourneySession, *steerReactionPlatform, *interactiveState, *Session) {
	t.Helper()
	p := &steerReactionPlatform{reactions: make(chan string, 16)}
	s := &steeringJourneySession{cujAgentSession: newCUJAgentSession(), steered: make(chan string, 8), accept: true, ready: make(chan struct{})}
	e := NewEngine("steering", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
	e.dataDir = t.TempDir()
	t.Cleanup(e.cancel)
	state := &interactiveState{agentSession: s}
	e.interactiveStates["key"] = state
	return e, s, p, state, e.sessions.GetOrCreateActive("key")
}

func snapshotPath(e *Engine) string {
	return filepath.Join(e.dataDir, "queues", queueFileName(e.name, "key"))
}

func TestSteerQueued_InOrderWithDataDirAndNoReplay(t *testing.T) {
	e, s, p, state, session := steerQueueFixture(t)
	for _, c := range []string{"one", "two"} {
		msg := &Message{MessageID: "id-" + c, Content: c}
		if !e.deliverMessageForBusySession(p, msg, "key", session, e.sessions) {
			t.Fatal("not handled")
		}
	}
	if len(state.pendingMessages) != 2 {
		t.Fatalf("queued=%d", len(state.pendingMessages))
	}
	if _, err := os.Stat(snapshotPath(e)); err != nil {
		t.Fatalf("snapshot missing while queued: %v", err)
	}
	close(s.ready)
	e.steerQueuedMessages(state, session, e.sessions)
	for _, want := range []string{"one", "two"} {
		select {
		case got := <-s.steered:
			if !strings.Contains(got, want) {
				t.Fatalf("got %q want %q", got, want)
			}
		default:
			t.Fatalf("%s not injected", want)
		}
	}
	if len(state.pendingMessages) != 0 {
		t.Fatal("queue not drained")
	}
	if _, err := os.Stat(snapshotPath(e)); !os.IsNotExist(err) {
		t.Fatalf("injected messages still persisted: %v", err)
	}
	// Restart: a fresh engine on the same data dir has nothing to replay.
	e2 := NewEngine("steering", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e2.dataDir = e.dataDir
	defer e2.cancel()
	if err := e2.loadQueues(); err != nil {
		t.Fatal(err)
	}
	for _, st := range e2.interactiveStates {
		if len(st.pendingMessages) != 0 {
			t.Fatal("replayed after restart")
		}
	}
}

func TestSteerQueued_ImageMessageNotQueuedWhenAccepted(t *testing.T) {
	e, s, p, state, session := steerQueueFixture(t)
	close(s.ready)
	msg := &Message{MessageID: "img", Content: "see", Images: []ImageAttachment{{Data: []byte("x")}}}
	if !e.deliverMessageForBusySession(p, msg, "key", session, e.sessions) {
		t.Fatal("not handled")
	}
	select {
	case <-s.steered:
	case <-time.After(time.Second):
		t.Fatal("not injected")
	}
	if len(state.pendingMessages) != 0 {
		t.Fatal("queued")
	}
}

func TestBatchContent_BeijingTime(t *testing.T) {
	e := &Engine{i18n: NewI18n(LangEnglish)}
	// 2026-11-09 00:30 UTC is 08:30 in Beijing.
	at := time.Date(2026, 11, 9, 0, 30, 0, 0, time.UTC)
	out := e.batchContent([]queuedMessage{{source: "human", content: "hi", userMessageTimeMs: at.UnixMilli()}})
	if !strings.Contains(out, "08:30") {
		t.Fatalf("header=%q", out)
	}
}
