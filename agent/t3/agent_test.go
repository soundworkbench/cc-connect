package t3

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

func TestNewDefaultsAndCredentialFile(t *testing.T) {
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credential")
	if err := os.WriteFile(credentialPath, []byte("opaque-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent, err := New(map[string]any{"credential_file": credentialPath, "cc_data_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	a := agent.(*Agent)
	if a.server != "127.0.0.1:6768" || a.provider != "antigravity" {
		t.Fatalf("defaults = %#v", a)
	}
	if a.credential != "opaque-token" {
		t.Fatalf("credential was not loaded")
	}
	if strings.Contains(a.server, a.credential) {
		t.Fatal("credential leaked into server address")
	}
}

func TestProjectIDForIsStableUUID(t *testing.T) {
	first, second := projectIDFor("/srv/xiaoman/butler"), projectIDFor("/srv/xiaoman/butler")
	if first != second || len(first) != 36 || strings.Count(first, "-") != 4 {
		t.Fatalf("project id = %q", first)
	}
}

func TestListSessionsMissingMapping(t *testing.T) {
	agent, err := New(map[string]any{"cc_data_dir": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := agent.ListSessions(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("sessions = %#v, err = %v", got, err)
	}
}

type fakeT3Server struct {
	t             *testing.T
	server        *httptest.Server
	credential    string
	mu            sync.Mutex
	writeMu       sync.Mutex
	connections   []*websocket.Conn
	requests      []map[string]any
	sequence      uint64
	threadID      string
	closedOnce    bool
	closeAfterSeq uint64
	permissionID  string
	responses     chan map[string]any
}

func newFakeT3Server(t *testing.T, credential string) *fakeT3Server {
	t.Helper()
	f := &fakeT3Server{t: t, credential: credential, threadID: "thread-test", responses: make(chan map[string]any, 32)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/websocket-ticket", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+credential {
			t.Errorf("ticket authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "ticket-test"})
	})
	mux.HandleFunc("/ws", f.handleWS)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeT3Server) addr() string {
	u, _ := url.Parse(f.server.URL)
	return u.Host
}

func (f *fakeT3Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("wsTicket") != "ticket-test" {
		http.Error(w, "missing ticket", http.StatusUnauthorized)
		return
	}
	c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.connections = append(f.connections, c)
	f.mu.Unlock()
	defer c.Close()
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			Tag     string         `json:"tag"`
			ID      string         `json:"id"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal(b, &req) != nil {
			continue
		}
		f.mu.Lock()
		f.requests = append(f.requests, map[string]any{"tag": req.Tag, "id": req.ID, "payload": req.Payload})
		f.mu.Unlock()
		switch req.Tag {
		case "projects.mutate":
			f.exit(c, req.ID, map[string]any{})
		case "orchestration.launchThread":
			f.exit(c, req.ID, map[string]any{"threadId": f.threadID})
		case "orchestration.subscribeThread":
			if f.closeAfterSeq > 0 {
				f.emitStored(c, map[string]any{"type": "turn-item.updated", "payload": map[string]any{"type": "unknown"}})
			}
		case "orchestration.dispatchCommand":
			f.responses <- map[string]any{"id": req.ID, "payload": req.Payload}
			f.exit(c, req.ID, map[string]any{})
		}
	}
}

func (f *fakeT3Server) exit(c *websocket.Conn, id string, value any) {
	f.writeJSON(c, map[string]any{"_tag": "Exit", "requestId": id, "exit": map[string]any{"_tag": "Success", "value": value}})
}

func (f *fakeT3Server) emitStored(c *websocket.Conn, event any) {
	f.mu.Lock()
	f.sequence++
	seq := f.sequence
	closeNow := f.closeAfterSeq > 0 && seq >= f.closeAfterSeq && !f.closedOnce
	if closeNow {
		f.closedOnce = true
	}
	f.mu.Unlock()
	f.writeJSON(c, map[string]any{"kind": "event", "sequence": seq, "event": event})
	if closeNow {
		_ = c.Close()
	}
}

func (f *fakeT3Server) emitSequence(c *websocket.Conn, sequence uint64, event any) {
	f.writeJSON(c, map[string]any{"kind": "event", "sequence": sequence, "event": event})
}

func (f *fakeT3Server) writeJSON(c *websocket.Conn, value any) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if err := c.WriteJSON(value); err != nil {
		f.t.Logf("fake event write: %v", err)
	}
}

func (f *fakeT3Server) emit(event any) {
	f.mu.Lock()
	var c *websocket.Conn
	if len(f.connections) > 0 {
		c = f.connections[len(f.connections)-1]
	}
	f.mu.Unlock()
	if c != nil {
		f.emitStored(c, event)
	}
}

func (f *fakeT3Server) requestsSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.requests...)
}

func waitT3(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFakeT3ProtocolLifecycleAndPersistence(t *testing.T) {
	const credential = "test-credential-do-not-log"
	f := newFakeT3Server(t, credential)
	dir := t.TempDir()
	credentialFile := filepath.Join(dir, "credential")
	if err := os.WriteFile(credentialFile, []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(map[string]any{"server": f.addr(), "credential_file": credentialFile, "cc_data_dir": dir, "work_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	s := sess.(*Session)
	t.Cleanup(func() { _ = s.Close() })
	if s.CurrentSessionID() != f.threadID {
		t.Fatalf("thread id = %q", s.CurrentSessionID())
	}

	if err := s.Send("first", "message-1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Send("second", "message-2", nil, nil); err != nil {
		t.Fatal(err)
	}
	var dispatches []map[string]any
	waitT3(t, func() bool {
		for _, req := range f.requestsSnapshot() {
			if req["tag"] == "orchestration.dispatchCommand" {
				p := req["payload"].(map[string]any)
				if p["type"] == "message.dispatch" {
					dispatches = append(dispatches, p)
				}
			}
		}
		return len(dispatches) >= 2
	})
	if got := dispatches[0]["dispatchMode"].(map[string]any)["type"]; got != "queue_after_active" {
		t.Fatalf("dispatch mode = %v", got)
	}

	f.emit(map[string]any{"type": "turn-item.updated", "payload": map[string]any{"type": "assistant_message", "text": "hel", "streaming": true}})
	f.emit(map[string]any{"type": "turn-item.updated", "payload": map[string]any{"type": "assistant_message", "text": "hello", "streaming": false}})
	f.emit(map[string]any{"type": "turn-item.updated", "payload": map[string]any{"type": "command_execution"}})
	f.emit(map[string]any{"type": "provider-turn.updated", "payload": map[string]any{"status": "completed"}})
	var gotText, gotResult bool
	var text string
	for !gotResult {
		select {
		case ev := <-s.Events():
			if ev.Type == core.EventText {
				text += ev.Content
				gotText = text == "hello"
			}
			if ev.Type == core.EventResult && ev.Done {
				gotResult = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("stream did not finish")
		}
	}
	if !gotText {
		t.Fatal("stream text missing")
	}

	f.permissionID = "request-1"
	f.emit(map[string]any{"type": "turn-item.updated", "payload": map[string]any{"type": "approval_request", "requestId": f.permissionID, "requestKind": "command"}})
	var permission core.Event
	select {
	case permission = <-s.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("permission event missing")
	}
	if permission.Type != core.EventPermissionRequest || permission.RequestID != f.permissionID {
		t.Fatalf("permission event = %+v", permission)
	}
	if c, ok := sess.(core.PermissionApprovalAll); !ok || c.SupportsPermissionApprovalAll() {
		t.Fatal("T3 must expose allow-once only")
	}
	if err := s.RespondPermission(f.permissionID, core.PermissionResult{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	waitT3(t, func() bool {
		for _, req := range f.requestsSnapshot() {
			if req["tag"] != "orchestration.dispatchCommand" {
				continue
			}
			p := req["payload"].(map[string]any)
			if p["type"] == "runtime-request.respond" {
				return p["decision"] == "accept"
			}
		}
		return false
	})
	if err := s.CancelTurn(); err != nil {
		t.Fatal(err)
	}
	waitT3(t, func() bool {
		for _, req := range f.requestsSnapshot() {
			if req["tag"] == "orchestration.dispatchCommand" && req["payload"].(map[string]any)["type"] == "thread.stop" {
				return true
			}
		}
		return false
	})

	ids, err := a.ListSessions(context.Background())
	if err != nil || len(ids) != 1 || ids[0].ID != f.threadID {
		t.Fatalf("persisted sessions = %+v, err=%v", ids, err)
	}
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}
	a2, err := New(map[string]any{"server": f.addr(), "credential_file": credentialFile, "cc_data_dir": dir, "work_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := a2.StartSession(context.Background(), f.threadID)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.CurrentSessionID() != f.threadID {
		t.Fatalf("resumed thread = %q", s2.CurrentSessionID())
	}
}

func TestFakeT3ReconnectReplaysAfterSequence(t *testing.T) {
	f := newFakeT3Server(t, "reconnect-credential")
	f.closeAfterSeq = 1
	dir := t.TempDir()
	cred := filepath.Join(dir, "credential")
	if err := os.WriteFile(cred, []byte("reconnect-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(map[string]any{"server": f.addr(), "credential_file": cred, "cc_data_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.StartSession(context.Background(), "thread-reconnect")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	s := sess.(*Session)
	waitT3(t, func() bool {
		if len(f.connectionsSnapshot()) < 2 {
			return false
		}
		for _, req := range f.requestsSnapshot() {
			if req["tag"] != "orchestration.subscribeThread" {
				continue
			}
			p := req["payload"].(map[string]any)
			if n, ok := p["afterSequence"].(float64); ok && n >= 1 {
				return true
			}
		}
		return false
	})
	var found []float64
	for _, req := range f.requestsSnapshot() {
		if req["tag"] != "orchestration.subscribeThread" {
			continue
		}
		p := req["payload"].(map[string]any)
		if n, ok := p["afterSequence"].(float64); ok {
			found = append(found, n)
		}
	}
	if len(found) == 0 || found[len(found)-1] < 1 {
		t.Fatalf("reconnect did not use afterSequence: %v", found)
	}
	latest := f.connectionsSnapshot()[1]
	f.emitSequence(latest, 1, map[string]any{"type": "turn-item.updated", "payload": map[string]any{"type": "assistant_message", "text": "duplicate", "streaming": false}})
	f.emit(map[string]any{"type": "turn-item.updated", "payload": map[string]any{"type": "assistant_message", "text": "replayed", "streaming": false}})
	f.emit(map[string]any{"type": "provider-turn.updated", "payload": map[string]any{"status": "completed"}})
	var textCount, resultCount int
	deadline := time.After(2 * time.Second)
	for resultCount == 0 {
		select {
		case ev := <-s.Events():
			if ev.Type == core.EventText && ev.Content == "replayed" {
				textCount++
			}
			if ev.Type == core.EventResult {
				resultCount++
			}
		case <-deadline:
			t.Fatal("reconnect event did not finish")
		}
	}
	if textCount != 1 {
		t.Fatalf("replayed text count = %d", textCount)
	}
}

func (f *fakeT3Server) connectionsSnapshot() []*websocket.Conn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*websocket.Conn(nil), f.connections...)
}

func TestT3CredentialNeverAppearsInFailureOrLogs(t *testing.T) {
	const secret = "credential-secret-for-test"
	var logs strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	a := &Agent{server: "127.0.0.1:1", credential: secret, dataDir: t.TempDir(), sessions: map[*Session]struct{}{}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := newSession(ctx, a, "thread")
	if err == nil {
		t.Fatal("expected connection failure")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("credential leaked, err=%q logs=%q", err, logs.String())
	}
}
