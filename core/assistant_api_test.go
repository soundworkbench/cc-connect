package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const cc17Summary = `{"schema_version":1,"handoff_id":"test-handoff","unfinished":[{"id":"task1","goal":"finish test","status":"active","next_action":"verify","evidence":"receipt","owner":"assistant"}],"decisions":[],"blockers":[],"running":[],"completed":[],"resume_checks":["read receipt"]}`

func cc17Fixture(t *testing.T) (*APIServer, *Engine, *scheduledTestSession, *http.Client, string) {
	t.Helper()
	p := &scheduledTestPlatform{&steerReactionPlatform{stubPlatformEngine: stubPlatformEngine{n: "local"}, reactions: make(chan string, 16)}}
	as := &scheduledTestSession{steeringJourneySession: &steeringJourneySession{cujAgentSession: newCUJAgentSession(), started: make(chan string, 16), steered: make(chan string, 16)}, modes: make(chan string, 16), ids: make(chan string, 16)}
	e := NewEngine("project", &controllableAgent{nextSession: as}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	key := "local:chat:user"
	e.sessions.GetOrCreateActive(key)
	socketDir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "cc17-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	api, err := NewAPIServer(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	api.RegisterEngine("project", e)
	api.Start()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", api.SocketPath())
	}}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	t.Cleanup(func() { e.cancel(); api.Stop(); transport.CloseIdleConnections() })
	return api, e, as, client, key
}
func cc17Post(t *testing.T, c *http.Client, path string, req assistantRequest) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(req)
	r, err := c.Post("http://local"+path, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	var result map[string]any
	if r.StatusCode == 200 {
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
	}
	return r.StatusCode, result
}
func TestAssistantInjectUnixLatencyFIFOAndReplay(t *testing.T) {
	_, e, as, c, key := cc17Fixture(t)
	req := assistantRequest{Project: "project", SessionKey: key, Message: "first", RequestID: "first"}
	before := time.Now()
	status, _ := cc17Post(t, c, "/assistant/inject", req)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	if got := cc27Take(t, as.started); !strings.Contains(got, "first") {
		t.Fatalf("first got %q", got)
	}
	elapsed := time.Since(before)
	if elapsed >= 2*time.Second {
		t.Fatal("admission exceeded 2 seconds")
	}
	t.Logf("real Unix API -> engine -> agent Send: %s", elapsed)
	req.Message = "second"
	req.RequestID = "second"
	status, _ = cc17Post(t, c, "/assistant/inject", req)
	if status != 200 {
		t.Fatalf("queue status=%d", status)
	}
	status, _ = cc17Post(t, c, "/assistant/inject", req)
	if status != 200 {
		t.Fatal("replay failed")
	}
	req.Message = "conflicting"
	status, _ = cc17Post(t, c, "/assistant/inject", req)
	if status != 409 {
		t.Fatalf("conflicting replay status=%d", status)
	}
	as.events <- Event{Type: EventResult, Content: "done", Done: true}
	if got := cc27Take(t, as.started); !strings.Contains(got, "second") {
		t.Fatalf("FIFO got %q", got)
	}
	as.events <- Event{Type: EventResult, Content: "done", Done: true}
	cc27WaitIdle(t, e, key)
	if len(as.started) != 0 {
		t.Fatal("duplicate replayed")
	}
	r, err := c.Get("http://local/assistant/state?project=project&session_key=" + key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var state map[string]any
	json.NewDecoder(r.Body).Decode(&state)
	if r.StatusCode != 200 || state["history_count"] != float64(4) || state["busy"] != false {
		t.Fatalf("state=%v status=%d", state, r.StatusCode)
	}
}
func TestAssistantRotatePreservesHistoryAndStartsSummary(t *testing.T) {
	_, e, as, c, key := cc17Fixture(t)
	old := e.sessions.GetOrCreateActive(key)
	old.AddHistory("user", "unfinished original task")
	old.AddHistory("assistant", cc17Summary)
	old.SetAgentSessionID("old-agent-id", "test")
	e.sessions.Save()
	oldLive := newCUJAgentSession()
	e.interactiveStates[key] = &interactiveState{agentSession: oldLive, stopCh: make(chan struct{})}
	req := assistantRequest{Project: "project", SessionKey: key, RequestID: "rotate", ExpectedSessionID: old.ID, ExpectedHistoryCount: 2, Summary: cc17Summary}
	status, result := cc17Post(t, c, "/assistant/rotate", req)
	if status != 200 {
		t.Fatalf("rotate status %d", status)
	}
	if got := cc27Take(t, as.started); !strings.Contains(got, cc17Summary) {
		t.Fatalf("first agent message is not summary: %q", got)
	}
	if oldLive.Alive() {
		t.Fatal("old live process was not closed")
	}
	fresh := e.sessions.GetOrCreateActive(key)
	if fresh.ID == old.ID || result["session_id"] != fresh.ID {
		t.Fatal("active session not changed")
	}
	if old.HistoryLen() != 2 || old.GetAgentSessionID() != "old-agent-id" {
		t.Fatal("old history or agent ID destroyed")
	}
	h := fresh.GetHistory(0)
	if len(h) != 1 || h[0].Role != "user" || h[0].Content != cc17Summary {
		t.Fatalf("first history duplicated or lost: %v", h)
	}
	as.events <- Event{Type: EventResult, Content: "handoff loaded", Done: true}
	cc27WaitIdle(t, e, key)
	reloaded := NewSessionManager(e.sessions.StorePath())
	if reloaded.ActiveSessionID(key) != fresh.ID || reloaded.FindByID(old.ID).HistoryLen() != 2 {
		t.Fatal("durable history missing")
	}
	status, replay := cc17Post(t, c, "/assistant/rotate", req)
	if status != 200 || replay["session_id"] != fresh.ID {
		t.Fatal("rotation replay created another session")
	}
}
func TestAssistantRotateRejectsBusyStaleForgedAndWriteFailure(t *testing.T) {
	_, e, _, c, key := cc17Fixture(t)
	old := e.sessions.GetOrCreateActive(key)
	old.AddHistory("assistant", cc17Summary)
	req := assistantRequest{Project: "project", SessionKey: key, RequestID: "rotate", ExpectedSessionID: old.ID, ExpectedHistoryCount: 1, Summary: cc17Summary}
	old.TryLock()
	status, _ := cc17Post(t, c, "/assistant/rotate", req)
	old.UnlockWithoutUpdate()
	if status != 409 {
		t.Fatalf("busy status=%d", status)
	}
	req.ExpectedHistoryCount = 0
	status, _ = cc17Post(t, c, "/assistant/rotate", req)
	if status != 409 {
		t.Fatalf("stale status=%d", status)
	}
	req.ExpectedHistoryCount = 1
	req.Summary = strings.Replace(cc17Summary, "test-handoff", "forged", 1)
	status, _ = cc17Post(t, c, "/assistant/rotate", req)
	if status != 409 {
		t.Fatalf("forged status=%d", status)
	}
	req.Summary = cc17Summary
	// Parent is a regular file, making atomic persistence fail without permissions tricks.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	e.sessions.storePath = filepath.Join(blocker, "sessions.json")
	status, _ = cc17Post(t, c, "/assistant/rotate", req)
	if status != 503 {
		t.Fatalf("write failure status=%d", status)
	}
	if e.sessions.ActiveSessionID(key) != old.ID || len(e.sessions.ListSessions(key)) != 1 || old.HistoryLen() != 1 || old.Busy() {
		t.Fatal("failed rotation modified original session")
	}
}
func TestAssistantRotateCompetingUserAdmissionNoLoss(t *testing.T) {
	_, e, as, c, key := cc17Fixture(t)
	old := e.sessions.GetOrCreateActive(key)
	old.AddHistory("assistant", cc17Summary)
	// Hold gate to model an in-flight ordinary message selecting its active session.
	e.assistantAdmissionMu.Lock()
	status, _ := cc17Post(t, c, "/assistant/rotate", assistantRequest{Project: "project", SessionKey: key, RequestID: "r", ExpectedSessionID: old.ID, ExpectedHistoryCount: 1, Summary: cc17Summary})
	e.assistantAdmissionMu.Unlock()
	if status != 409 {
		t.Fatalf("concurrent admission status=%d", status)
	}
	status, _ = cc17Post(t, c, "/assistant/inject", assistantRequest{Project: "project", SessionKey: key, RequestID: "message", Message: "must survive"})
	if status != 200 {
		t.Fatal("message failed")
	}
	cc27Take(t, as.started)
	as.events <- Event{Type: EventResult, Content: "done", Done: true}
	cc27WaitIdle(t, e, key)
	if old.HistoryLen() != 3 || e.sessions.ActiveSessionID(key) != old.ID {
		t.Fatal("competing admission lost or moved")
	}
}

func TestAssistantRotateConcurrentRealUserNoLoss(t *testing.T) {
	for iteration := 0; iteration < 12; iteration++ {
		t.Run(fmt.Sprint(iteration), func(t *testing.T) {
			_, e, _, c, key := cc17Fixture(t)
			e.agent.(*controllableAgent).startSessionFn = func(context.Context, string) (AgentSession, error) { return newCUJAgentSession(), nil }
			old := e.sessions.GetOrCreateActive(key)
			old.AddHistory("assistant", cc17Summary)
			start := make(chan struct{})
			userDone := make(chan struct{})
			statusDone := make(chan int, 1)
			go func() {
				<-start
				e.ReceiveMessage(e.platforms[0], &Message{SessionKey: key, Platform: e.platforms[0].Name(), UserID: "real-user", MessageID: "real-msg", Content: "concurrent user must survive", ReplyCtx: "user"})
				close(userDone)
			}()
			go func() {
				<-start
				status, _ := cc17Post(t, c, "/assistant/rotate", assistantRequest{Project: "project", SessionKey: key, RequestID: "concurrent", ExpectedSessionID: old.ID, ExpectedHistoryCount: 1, Summary: cc17Summary})
				statusDone <- status
			}()
			close(start)
			<-userDone
			status := <-statusDone
			if status != 200 && status != 409 {
				t.Fatalf("rotate status %d", status)
			}
			if !e.waitForAcceptedWork(3 * time.Second) {
				t.Fatal("work did not drain")
			}
			seen := 0
			for _, session := range e.sessions.ListSessions(key) {
				for _, h := range session.GetHistory(0) {
					if h.Role == "user" && h.Content == "concurrent user must survive" {
						seen++
					}
				}
			}
			if seen != 1 {
				t.Fatalf("user message histories=%d (want exactly 1)", seen)
			}
		})
	}
}

type cc17FailSendSession struct{ *cujAgentSession }

func (s *cc17FailSendSession) Send(string, string, []ImageAttachment, []FileAttachment) error {
	return fmt.Errorf("first send failed")
}
func TestAssistantRotateStartupAndFirstSendFailureKeepsOldLive(t *testing.T) {
	for _, kind := range []string{"start", "send", "dead"} {
		t.Run(kind, func(t *testing.T) {
			_, e, _, c, key := cc17Fixture(t)
			old := e.sessions.GetOrCreateActive(key)
			old.AddHistory("assistant", cc17Summary)
			old.SetAgentSessionID("preserve-old", "test")
			oldLive := newCUJAgentSession()
			e.interactiveStates[key] = &interactiveState{agentSession: oldLive, stopCh: make(chan struct{})}
			candidate := &cc17FailSendSession{newCUJAgentSession()}
			e.agent.(*controllableAgent).startSessionFn = func(context.Context, string) (AgentSession, error) {
				if kind == "start" {
					return nil, fmt.Errorf("start failed")
				}
				if kind == "dead" {
					candidate.Close()
				}
				return candidate, nil
			}
			status, _ := cc17Post(t, c, "/assistant/rotate", assistantRequest{Project: "project", SessionKey: key, RequestID: "r", ExpectedSessionID: old.ID, ExpectedHistoryCount: 1, Summary: cc17Summary})
			if status != 503 {
				t.Fatalf("status=%d", status)
			}
			if e.sessions.ActiveSessionID(key) != old.ID || !oldLive.Alive() || old.GetAgentSessionID() != "preserve-old" || old.HistoryLen() != 1 || old.Busy() {
				t.Fatal("failed candidate damaged old session")
			}
			if kind == "send" && candidate.Alive() {
				t.Fatal("failed candidate not closed")
			}
		})
	}
}

type cc17BlockingSendSession struct {
	*cujAgentSession
	closed chan struct{}
}

func (s *cc17BlockingSendSession) Send(string, string, []ImageAttachment, []FileAttachment) error {
	<-s.closed
	return fmt.Errorf("closed")
}
func (s *cc17BlockingSendSession) Close() error { close(s.closed); return s.cujAgentSession.Close() }
func TestAssistantRotateBlockedFirstSendKeepsOld(t *testing.T) {
	_, e, _, c, key := cc17Fixture(t)
	c.Timeout = 8 * time.Second
	old := e.sessions.GetOrCreateActive(key)
	old.AddHistory("assistant", cc17Summary)
	oldLive := newCUJAgentSession()
	e.interactiveStates[key] = &interactiveState{agentSession: oldLive, stopCh: make(chan struct{})}
	candidate := &cc17BlockingSendSession{newCUJAgentSession(), make(chan struct{})}
	e.agent.(*controllableAgent).nextSession = candidate
	status, _ := cc17Post(t, c, "/assistant/rotate", assistantRequest{Project: "project", SessionKey: key, RequestID: "blocked", ExpectedSessionID: old.ID, ExpectedHistoryCount: 1, Summary: cc17Summary})
	if status != 503 || e.sessions.ActiveSessionID(key) != old.ID || old.HistoryLen() != 1 || !oldLive.Alive() || candidate.Alive() || old.Busy() {
		t.Fatal("blocked first Send damaged old session or leaked candidate")
	}
}
