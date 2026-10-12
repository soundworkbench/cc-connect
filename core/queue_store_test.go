package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type queueTestPlatform struct{ stubPlatformEngine }

func (p *queueTestPlatform) ReconstructReplyCtx(key string) (any, error) { return key, nil }

func queueTestEngine(t *testing.T, dir string, p Platform, s AgentSession) *Engine {
	t.Helper()
	e := NewEngine("durable-queue", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(dir, "sessions.json"), LangEnglish)
	e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
	e.SetDataDir(dir)
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	t.Cleanup(func() { e.cancel(); _ = e.Stop() })
	return e
}

func queueTestSession() *queuedLifecycleSession {
	return &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 256)}
}

func queueTestSend(e *Engine, p Platform, id, text string) {
	e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: p.Name(), MessageID: id, UserID: "user", Content: text, ReplyCtx: "test:user"})
}

func TestQueuePersistence_RestartRestoresFIFOAndDoesNotReplayCurrent(t *testing.T) {
	dir := t.TempDir()
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, dir, p, s)
	queueTestSend(e, p, "one", "current input")
	cc27Take(t, s.sent)
	queueTestSend(e, p, "two", "queued two")
	queueTestSend(e, p, "three", "queued three")
	path := filepath.Join(dir, "queues", queueFileName(e.name, "test:user"))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %o", info.Mode().Perm())
	}
	records, err := readQueueFile(path, e.name)
	if err != nil || len(records) != 2 || records[0].Content != "queued two" || records[1].Content != "queued three" {
		t.Fatalf("snapshot = %+v, %v", records, err)
	}
	// Cancellation simulates an abrupt process interruption, including the old
	// event loop's cleanup; queued inputs must remain durable.
	e.cancel()
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	p2 := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s2 := queueTestSession()
	e2 := queueTestEngine(t, dir, p2, s2)
	if err := e2.Start(); err != nil {
		t.Fatal(err)
	}
	if got := cc27Take(t, s2.sent); got != "queued two" {
		t.Fatal(got)
	}
	queueTestSend(e2, p2, "four", "new after restart")
	s2.events <- Event{Type: EventResult, Content: "two done", Done: true}
	if got := cc27Take(t, s2.sent); got != "queued three" {
		t.Fatal(got)
	}
	s2.events <- Event{Type: EventResult, Content: "three done", Done: true}
	if got := cc27Take(t, s2.sent); got != "new after restart" {
		t.Fatal(got)
	}
	s2.events <- Event{Type: EventResult, Content: "four done", Done: true}
	cc27WaitIdle(t, e2, "test:user")
	if len(s2.sent) != 0 {
		t.Fatal("duplicate input")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("finished snapshot remains: %v", err)
	}
}

func TestQueuePersistence_ShutdownKeepsPendingAndRejectsNextTurn(t *testing.T) {
	dir := t.TempDir()
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, dir, p, s)
	queueTestSend(e, p, "one", "current")
	cc27Take(t, s.sent)
	queueTestSend(e, p, "two", "pending")
	done := make(chan error, 1)
	go func() { done <- e.Stop() }()
	deadline := time.Now().Add(time.Second)
	for !e.isShuttingDown() {
		if time.Now().After(deadline) {
			t.Fatal("shutdown not started")
		}
		time.Sleep(time.Millisecond)
	}
	queueTestSend(e, p, "three", "rejected")
	if err := e.ExecuteTimerJob(&TimerJob{ID: "late", SessionKey: "test:other", Prompt: "must not start"}); err == nil {
		t.Fatal("timer accepted during shutdown")
	}
	if err := e.ExecuteCronJob(&CronJob{ID: "late", SessionKey: "test:other", Prompt: "must not start"}); err == nil {
		t.Fatal("cron accepted during shutdown")
	}
	s.events <- Event{Type: EventResult, Content: "current done", Done: true}
	if err := cc27Take(t, done); err != nil {
		t.Fatal(err)
	}
	if len(s.sent) != 0 {
		t.Fatal("started another round during shutdown")
	}
	records, err := readQueueFile(filepath.Join(dir, "queues", queueFileName(e.name, "test:user")), e.name)
	if err != nil || len(records) != 1 || records[0].Content != "pending" {
		t.Fatalf("pending lost: %+v %v", records, err)
	}
	visible := strings.Join(p.getSent(), "\n")
	if !strings.Contains(visible, "current done") || !strings.Contains(visible, e.i18n.T(MsgShutdownRetry)) {
		t.Fatal(visible)
	}
}

func TestQueuePersistence_CorruptLineDoesNotHideLargeFollowingInput(t *testing.T) {
	dir := t.TempDir()
	r := queueRecord{Project: "project", Key: "key", Platform: "test", SessionKey: "test:user", Content: "good"}
	path := filepath.Join(dir, queueFileName(r.Project, r.Key))
	first, _ := json.Marshal(r)
	r.Content = strings.Repeat("image-text", 20000)
	r.Images = []ImageAttachment{{Data: bytes.Repeat([]byte("a"), 90000), FileName: "image.png"}}
	last, _ := json.Marshal(r)
	data := append(append(append(first, '\n'), []byte("{broken\n")...), last...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	records, err := readQueueFile(path, r.Project)
	if err != nil || len(records) != 2 || records[1].Content != r.Content || !bytes.Equal(records[1].Images[0].Data, r.Images[0].Data) {
		t.Fatalf("bad recovery: %d records, %v", len(records), err)
	}
	if !strings.Contains(logs.String(), "skipping corrupt line") || !strings.Contains(logs.String(), "line=2") {
		t.Fatal(logs.String())
	}
}

func TestQueuePersistence_ConcurrentAdmissionNoLossOrDuplicates(t *testing.T) {
	dir := t.TempDir()
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, dir, p, s)
	e.maxQueuedMessages = 200
	queueTestSend(e, p, "first", "current")
	cc27Take(t, s.sent)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("q%02d", i)
			queueTestSend(e, p, id, id)
			queueTestSend(e, p, id, id)
		}(i)
	}
	wg.Wait()
	records, err := readQueueFile(filepath.Join(dir, "queues", queueFileName(e.name, "test:user")), e.name)
	if err != nil || len(records) != 50 {
		t.Fatalf("accepted count = %d, %v", len(records), err)
	}
	e.cancel()
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	s2 := queueTestSession()
	e2 := queueTestEngine(t, dir, p, s2)
	if err := e2.Start(); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, r := range records {
		got := cc27Take(t, s2.sent)
		if got != r.Content || seen[got] {
			t.Fatalf("recovered order/duplicate %q, expected %q", got, r.Content)
		}
		seen[got] = true
		s2.events <- Event{Type: EventResult, Content: "done", Done: true}
	}
	cc27WaitIdle(t, e2, "test:user")
	if len(s2.sent) != 0 {
		t.Fatal("duplicate after recovery")
	}
}

func TestQueuePersistence_TimerAndAssistantInputsRestore(t *testing.T) {
	dir := t.TempDir()
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, dir, p, s)
	queueTestSend(e, p, "current", "current")
	cc27Take(t, s.sent)
	if err := e.ExecuteTimerJob(&TimerJob{ID: "t1", SessionKey: "test:user", Prompt: "timer input", ScheduledAt: time.Unix(100, 0), Mute: true, Mode: "plan"}); err != nil {
		t.Fatal(err)
	}
	e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: "test", UserID: "assistant", Content: "assistant completion", ReplyCtx: "test:user"})
	e.cancel()
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	s2 := queueTestSession()
	e2 := queueTestEngine(t, dir, p, s2)
	if err := e2.Start(); err != nil {
		t.Fatal(err)
	}
	if got := cc27Take(t, s2.sent); got != "timer input" {
		t.Fatal(got)
	}
	s2.events <- Event{Type: EventResult, Content: "muted timer answer", Done: true}
	if got := cc27Take(t, s2.sent); got != "assistant completion" {
		t.Fatal(got)
	}
	s2.events <- Event{Type: EventResult, Content: "assistant answer", Done: true}
	cc27WaitIdle(t, e2, "test:user")
	visible := strings.Join(p.getSent(), "\n")
	if !strings.Contains(visible, "muted timer answer") || !strings.Contains(visible, "assistant answer") {
		t.Fatal(visible)
	}
}

func TestQueuePersistence_DiskFailureDoesNotAcknowledgeOrDispatch(t *testing.T) {
	dir := t.TempDir()
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, dir, p, s)
	queueTestSend(e, p, "one", "current")
	cc27Take(t, s.sent)
	if err := os.WriteFile(filepath.Join(dir, "queues"), []byte("obstruction"), 0o600); err != nil {
		t.Fatal(err)
	}
	accepted := false
	e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: "test", Content: "blocked", ReplyCtx: "ctx", OnAccepted: func() { accepted = true }})
	if accepted {
		t.Fatal("acknowledged an undurable input")
	}
	if strings.Contains(strings.Join(p.getSent(), "\n"), e.i18n.T(MsgMessageQueued)) {
		t.Fatal("queue success feedback on disk failure")
	}
	if err := os.Remove(filepath.Join(dir, "queues")); err != nil {
		t.Fatal(err)
	}
	queueTestSend(e, p, "two", "persisted")
	e.interactiveMu.Lock()
	state := e.interactiveStates["test:user"]
	e.interactiveMu.Unlock()
	state.mu.Lock()
	state.queueKey = "different" // failure injection at the snapshot replacement boundary
	badPath := filepath.Join(dir, "queues", queueFileName(e.name, state.queueKey))
	if err := os.Mkdir(badPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badPath, "block"), []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.popQueuedMessageLocked(state); ok {
		t.Fatal("dispatched despite snapshot failure")
	}
	if len(state.pendingMessages) != 1 {
		t.Fatal("failed pop lost input")
	}
	state.queueKey = "test:user"
	state.mu.Unlock()
}

var _ ReplyContextReconstructor = (*queueTestPlatform)(nil)

func TestProcessInteractiveEvents_SessionResetBeforeReaderStarts(t *testing.T) {
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := queueTestEngine(t, t.TempDir(), p, queueTestSession())
	session := e.sessions.GetOrCreateActive("test:user")
	state := e.getOrCreateInteractiveStateWith("test:user", p, "ctx", session, e.sessions, nil, "test:user")
	e.cleanupInteractiveState("test:user", state)
	stoppedTyping := false
	e.processInteractiveEvents(state, session, e.sessions, "test:user", "current", time.Now(), func() { stoppedTyping = true }, nil, "ctx")
	if !stoppedTyping {
		t.Fatal("reset reader left the typing indicator active")
	}
}

func TestQueuePersistence_StopDuringStartupDoesNotStartCancelledInput(t *testing.T) {
	testStopDuringAcceptedStartup(t)
}

func testStopDuringAcceptedStartup(t *testing.T) {
	dir := t.TempDir()
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, dir, p, s)
	e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: "test", MessageID: "first", Content: "cancelled input", ReplyCtx: "ctx", OnAccepted: func() {
		queueTestSend(e, p, "second", "pending after startup stop")
		queueTestSend(e, p, "stop", "/stop")
	}})
	if got := cc27Take(t, s.sent); got != "pending after startup stop" {
		t.Fatalf("cancelled input executed: %s", got)
	}
	s.events <- Event{Type: EventResult, Content: "pending done", Done: true}
	cc27WaitIdle(t, e, "test:user")
	if len(s.sent) != 0 {
		t.Fatal("cancelled input replayed")
	}
	visible := strings.Join(p.getSent(), "\n")
	if !strings.Contains(visible, e.i18n.T(MsgExecutionStopped)) || !strings.Contains(visible, "pending done") {
		t.Fatalf("startup stop lost pending output: %s", visible)
	}
}

func TestHandleMessage_SteeredAcceptanceCanReenterAndStop(t *testing.T) {
	_, e, as, _, key := cc17Fixture(t)
	p := e.platforms[0]
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "first", Content: "current", ReplyCtx: key})
	cc27Take(t, as.started)
	accepted := make(chan struct{})
	go func() {
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "second", Content: "supplement", ReplyCtx: key, OnAccepted: func() {
			e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), Content: "/stop", ReplyCtx: key})
			close(accepted)
		}})
	}()
	cc27Take(t, accepted)
	if got := cc27Take(t, as.steered); got != "supplement" {
		t.Fatal(got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for as.Alive() {
		if time.Now().After(deadline) {
			t.Fatal("steered callback did not stop the process")
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(strings.Join(p.(*scheduledTestPlatform).getSent(), "\n"), e.i18n.T(MsgExecutionStopped)) {
		t.Fatal("steered callback did not acknowledge stop")
	}
}

func TestQueuePersistence_QueuedAcceptanceCanReenterAndStop(t *testing.T) {
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	first, next := queueTestSession(), queueTestSession()
	e := queueTestEngine(t, t.TempDir(), p, first)
	queueTestSend(e, p, "first", "current")
	cc27Take(t, first.sent)
	e.agent.(*controllableAgent).nextSession = next
	accepted := make(chan struct{})
	go func() {
		e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: "test", MessageID: "second", Content: "queued second", ReplyCtx: "ctx", OnAccepted: func() {
			queueTestSend(e, p, "third", "queued third")
			queueTestSend(e, p, "stop", "/stop")
			close(accepted)
		}})
	}()
	cc27Take(t, accepted)
	if got := cc27Take(t, next.sent); got != "queued second" {
		t.Fatal(got)
	}
	next.events <- Event{Type: EventResult, Content: "second answer", Done: true}
	if got := cc27Take(t, next.sent); got != "queued third" {
		t.Fatal(got)
	}
	next.events <- Event{Type: EventResult, Content: "third answer", Done: true}
	cc27WaitIdle(t, e, "test:user")
	visible := strings.Join(p.getSent(), "\n")
	if !strings.Contains(visible, "second answer") || !strings.Contains(visible, "third answer") || len(next.sent) != 0 {
		t.Fatalf("queued callback lost or duplicated a turn: %s", visible)
	}
}

func TestProcessInteractiveMessage_PreflightDoesNotAdoptReplacementAfterStop(t *testing.T) {
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, t.TempDir(), p, s)
	session := e.sessions.GetOrCreateActive("test:user")
	if !session.TryLock() {
		t.Fatal("session busy")
	}
	retired := &interactiveState{stopped: true, queueRetired: true}
	replacement := &interactiveState{agentSession: s, currentMessageID: "next", eventsNeedResync: false}
	e.interactiveStates["test:user"] = replacement
	done := make(chan struct{})
	go func() {
		e.processInteractiveMessageWith(p, &Message{SessionKey: "test:user", preflightSent: true, historyPreseeded: true, executionState: retired}, session, e.agent, e.sessions, "test:user", "", "test:user")
		close(done)
	}()
	cc27Take(t, done)
	replacement.mu.Lock()
	id := replacement.currentMessageID
	replacement.mu.Unlock()
	if id != "next" {
		t.Fatalf("cancelled preflight adopted the next turn: message ID %q", id)
	}
}

type queueClosingSession struct {
	*queuedLifecycleSession
	closing chan struct{}
	release chan struct{}
}

func (s *queueClosingSession) Close() error {
	close(s.closing)
	<-s.release
	return s.queuedLifecycleSession.Close()
}

func TestQueuePersistence_StopWaitsForProcessCloseBeforeContinuing(t *testing.T) {
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	old := &queueClosingSession{queuedLifecycleSession: queueTestSession(), closing: make(chan struct{}), release: make(chan struct{})}
	next := queueTestSession()
	var starts int
	a := &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
		starts++
		if starts == 1 {
			return old, nil
		}
		return next, nil
	}}
	e := NewEngine("durable-queue", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
	t.Cleanup(func() { e.cancel(); _ = e.Stop() })
	queueTestSend(e, p, "first", "current")
	cc27Take(t, old.sent)
	queueTestSend(e, p, "second", "queued two")
	queueTestSend(e, p, "stop", "/stop")
	cc27Take(t, old.closing)
	queueTestSend(e, p, "stop-again", "/stop")
	queueTestSend(e, p, "third", "queued three")
	select {
	case got := <-next.sent:
		t.Fatalf("new process started before Close returned: %s", got)
	case <-time.After(30 * time.Millisecond):
	}
	close(old.release)
	if got := cc27Take(t, next.sent); got != "queued two" {
		t.Fatal(got)
	}
	next.events <- Event{Type: EventResult, Content: "two done", Done: true}
	if got := cc27Take(t, next.sent); got != "queued three" {
		t.Fatal(got)
	}
	next.events <- Event{Type: EventResult, Content: "three done", Done: true}
	cc27WaitIdle(t, e, "test:user")
}

type queueRecoveringPlatform struct {
	queueTestPlatform
	handler   PlatformLifecycleHandler
	failRoute bool
	routes    []string
}

func (p *queueRecoveringPlatform) SetLifecycleHandler(h PlatformLifecycleHandler) { p.handler = h }
func (p *queueRecoveringPlatform) ReconstructReplyCtx(key string) (any, error) {
	if p.failRoute {
		return nil, fmt.Errorf("route temporarily unavailable")
	}
	p.routes = append(p.routes, key)
	return key, nil
}

func TestQueuePersistence_AsyncRecoveryRetriesThreadRouteWithoutDuplicate(t *testing.T) {
	dir := t.TempDir()
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, dir, p, s)
	queueTestSend(e, p, "current", "current")
	cc27Take(t, s.sent)
	e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: "test", MessageID: "thread", Content: "thread input", replySessionKey: "test:thread:user", ReplyCtx: "thread"})
	e.cancel()
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	p2 := &queueRecoveringPlatform{queueTestPlatform: queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}, failRoute: true}
	s2 := queueTestSession()
	e2 := queueTestEngine(t, dir, p2, s2)
	if err := e2.Start(); err != nil {
		t.Fatal(err)
	}
	if len(s2.sent) != 0 {
		t.Fatal("dispatched before async readiness")
	}
	p2.handler.OnPlatformReady(p2)
	if len(s2.sent) != 0 {
		t.Fatal("dispatched without reply route")
	}
	p2.handler.OnPlatformUnavailable(p2, fmt.Errorf("reconnecting"))
	p2.failRoute = false
	p2.handler.OnPlatformReady(p2)
	if got := cc27Take(t, s2.sent); got != "thread input" {
		t.Fatal(got)
	}
	p2.handler.OnPlatformUnavailable(p2, fmt.Errorf("reconnecting"))
	p2.handler.OnPlatformReady(p2)
	s2.events <- Event{Type: EventResult, Content: "thread answer", Done: true}
	cc27WaitIdle(t, e2, "test:user")
	if len(s2.sent) != 0 || len(p2.routes) != 1 || p2.routes[0] != "test:thread:user" {
		t.Fatalf("duplicate or wrong route: %v", p2.routes)
	}
}

func TestQueuePersistence_NewRemovesSnapshotAndListsInput(t *testing.T) {
	dir := t.TempDir()
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, dir, p, s)
	queueTestSend(e, p, "one", "current")
	cc27Take(t, s.sent)
	queueTestSend(e, p, "two", "task completion to resend")
	queueTestSend(e, p, "new", "/new")
	path := filepath.Join(dir, "queues", queueFileName(e.name, "test:user"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reset left snapshot: %v", err)
	}
	if !strings.Contains(strings.Join(p.getSent(), "\n"), "task completion to resend") {
		t.Fatal(p.getSent())
	}
}

func TestQueuePersistence_RecallRemovesOnlyMatchingInputFromDisk(t *testing.T) {
	dir := t.TempDir()
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, dir, p, s)
	queueTestSend(e, p, "one", "current")
	cc27Take(t, s.sent)
	queueTestSend(e, p, "two", "recalled")
	queueTestSend(e, p, "three", "retained")
	e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: "test", MessageID: "two", Recalled: true})
	records, err := readQueueFile(filepath.Join(dir, "queues", queueFileName(e.name, "test:user")), e.name)
	if err != nil || len(records) != 1 || records[0].Content != "retained" {
		t.Fatalf("recall snapshot: %+v, %v", records, err)
	}
}
