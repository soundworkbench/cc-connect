package t3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

func TestProjectIDForIsStableUUID(t *testing.T) {
	first, second := projectIDFor("/home/user/project"), projectIDFor("/home/user/project")
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

// fakeT3 mimics the T3 Code 0.1.12 WebSocket RPC as seen in the smoke test:
// requests need headers, message.dispatch needs createdBy/creationSource,
// launchThread is idempotent per commandId, thread streams send one Chunk at a
// time and wait for its Ack, and a fresh subscription starts with a snapshot.
type fakeT3 struct {
	t          *testing.T
	server     *httptest.Server
	credential string

	mu        sync.Mutex
	conns     []*websocket.Conn
	writeMu   map[*websocket.Conn]*sync.Mutex
	requests  []fakeRequest
	threads   map[string][]fakeLogged      // thread id -> event log
	byCommand map[string]string            // launch commandId -> thread id
	subs      map[string]*fakeSub          // requestId -> live subscription
	runs      map[string]map[string]string // thread id -> run id -> status
	nThreads  int
	ticketErr int
	// snapshotOnResume answers a resubscribe with a snapshot instead of a
	// replay, as T3 does when too many events were missed.
	snapshotOnResume bool
	// folderOwner is the project that already owns the workspace folder.
	folderOwner string
}

type fakeRequest struct {
	Tag     string
	ID      string
	Payload map[string]any
}

type fakeLogged struct {
	seq   uint64
	event map[string]any
}

type fakeSub struct {
	conn   *websocket.Conn
	id     string
	thread string
	queue  chan any
	acks   chan struct{}
	done   chan struct{}
}

func newFakeT3(t *testing.T, credential string) *fakeT3 {
	t.Helper()
	f := &fakeT3{t: t, credential: credential, writeMu: map[*websocket.Conn]*sync.Mutex{}, threads: map[string][]fakeLogged{}, byCommand: map[string]string{}, subs: map[string]*fakeSub{}, runs: map[string]map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/websocket-ticket", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		code := f.ticketErr
		f.mu.Unlock()
		if code != 0 {
			http.Error(w, "denied", code)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+credential {
			http.Error(w, "bad credential", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "ticket-test"})
	})
	mux.HandleFunc("/ws", f.handleWS)
	f.server = httptest.NewServer(mux)
	t.Cleanup(func() { f.dropAll(); f.server.Close() })
	return f
}

func (f *fakeT3) addr() string { u, _ := url.Parse(f.server.URL); return u.Host }

func (f *fakeT3) write(c *websocket.Conn, v any) {
	f.mu.Lock()
	m := f.writeMu[c]
	f.mu.Unlock()
	if m == nil {
		return
	}
	m.Lock()
	defer m.Unlock()
	_ = c.WriteJSON(v)
}

func (f *fakeT3) success(c *websocket.Conn, id string, value any) {
	f.write(c, map[string]any{"_tag": "Exit", "requestId": id, "exit": map[string]any{"_tag": "Success", "value": value}})
}

func (f *fakeT3) failDefect(c *websocket.Conn, id, defect string) {
	f.write(c, map[string]any{"_tag": "Exit", "requestId": id, "exit": map[string]any{"_tag": "Failure", "cause": []any{map[string]any{"_tag": "Die", "defect": defect}}}})
}

func (f *fakeT3) failError(c *websocket.Conn, id, tag, message string) {
	f.write(c, map[string]any{"_tag": "Exit", "requestId": id, "exit": map[string]any{"_tag": "Failure", "cause": []any{map[string]any{"_tag": "Fail", "error": map[string]any{"_tag": tag, "message": message, "payloadDump": "SECRET-PAYLOAD"}}}}})
}

func (f *fakeT3) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("wsTicket") != "ticket-test" || r.URL.Query().Get("orchestrationProtocol") != "2" {
		http.Error(w, "missing ticket", http.StatusUnauthorized)
		return
	}
	c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conns = append(f.conns, c)
	f.writeMu[c] = &sync.Mutex{}
	f.mu.Unlock()
	defer func() {
		_ = c.Close()
		f.mu.Lock()
		for id, sub := range f.subs {
			if sub.conn == c {
				close(sub.done)
				delete(f.subs, id)
			}
		}
		f.mu.Unlock()
	}()
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			Tag       string          `json:"_tag"`
			ID        string          `json:"id"`
			RPC       string          `json:"tag"`
			RequestID string          `json:"requestId"`
			Payload   map[string]any  `json:"payload"`
			Headers   json.RawMessage `json:"headers"`
		}
		if json.Unmarshal(b, &msg) != nil {
			continue
		}
		if msg.Tag == "Ack" {
			f.mu.Lock()
			sub := f.subs[msg.RequestID]
			f.mu.Unlock()
			if sub != nil {
				sub.acks <- struct{}{}
			}
			continue
		}
		f.mu.Lock()
		f.requests = append(f.requests, fakeRequest{Tag: msg.RPC, ID: msg.ID, Payload: msg.Payload})
		f.mu.Unlock()
		if len(msg.Headers) == 0 || string(msg.Headers) == "null" {
			f.failDefect(c, msg.ID, "Invalid request headers")
			continue
		}
		f.handleRPC(c, msg.RPC, msg.ID, msg.Payload)
	}
}

func (f *fakeT3) handleRPC(c *websocket.Conn, rpc, id string, p map[string]any) {
	switch rpc {
	case "projects.mutate":
		f.mu.Lock()
		owner := f.folderOwner
		f.mu.Unlock()
		if owner != "" {
			f.write(c, map[string]any{"_tag": "Exit", "requestId": id, "exit": map[string]any{"_tag": "Failure", "cause": []any{map[string]any{"_tag": "Fail", "error": map[string]any{"_tag": "ProjectConflictError", "projectId": p["projectId"], "workspaceRoot": p["workspaceRoot"], "conflictingProjectId": owner}}}}})
			return
		}
		f.success(c, id, map[string]any{})
	case "orchestration.launchThread":
		cmd, _ := p["commandId"].(string)
		f.mu.Lock()
		thread := f.byCommand[cmd]
		if thread == "" {
			f.nThreads++
			thread = fmt.Sprintf("thread-%d", f.nThreads)
			f.byCommand[cmd] = thread
			f.threads[thread] = nil
		}
		f.mu.Unlock()
		f.success(c, id, map[string]any{"threadId": thread})
	case "orchestration.subscribeThread":
		thread, _ := p["threadId"].(string)
		afterRaw, resume := p["afterSequence"].(float64)
		after := uint64(afterRaw)
		f.mu.Lock()
		log, ok := f.threads[thread]
		snapshot := !resume || f.snapshotOnResume
		var runs []any
		for rid, st := range f.runs[thread] {
			runs = append(runs, map[string]any{"id": rid, "ordinal": len(runs) + 1, "status": st})
		}
		f.mu.Unlock()
		// 0.1.12: a snapshot request for a missing thread fails; a resume from a
		// sequence just waits silently.
		if !ok && !resume {
			f.failError(c, id, "OrchestrationV2GetThreadProjectionError", "No orchestration projection exists for thread "+thread+".")
			return
		}
		sub := &fakeSub{conn: c, id: id, thread: thread, queue: make(chan any, 256), acks: make(chan struct{}, 256), done: make(chan struct{})}
		if snapshot {
			var top uint64
			if len(log) > 0 {
				top = log[len(log)-1].seq
			}
			sub.queue <- map[string]any{"kind": "snapshot", "snapshotSequence": top, "projection": map[string]any{"runs": runs}}
		} else {
			for _, l := range log {
				if l.seq > after {
					sub.queue <- map[string]any{"kind": "event", "sequence": l.seq, "event": l.event}
				}
			}
		}
		f.mu.Lock()
		f.subs[id] = sub
		f.mu.Unlock()
		go f.pump(sub)
	case "orchestration.dispatchCommand":
		if p["type"] == "message.dispatch" && (p["createdBy"] == nil || p["creationSource"] == nil) {
			f.failDefect(c, id, "Missing key\n  at [\"createdBy\"]")
			return
		}
		if p["threadId"] == "thread-broken" {
			f.failError(c, id, "OrchestrationV2DispatchCommandError", "No orchestration projection exists for thread thread-broken.")
			return
		}
		f.success(c, id, map[string]any{"sequence": 1})
	}
}

// pump sends one Chunk and waits for its Ack, as T3's stream flow control does.
func (f *fakeT3) pump(sub *fakeSub) {
	for {
		select {
		case v := <-sub.queue:
			f.write(sub.conn, map[string]any{"_tag": "Chunk", "requestId": sub.id, "values": []any{v}})
			select {
			case <-sub.acks:
			case <-sub.done:
				return
			case <-time.After(2 * time.Second):
				f.t.Errorf("client never acked a stream chunk")
				return
			}
		case <-sub.done:
			return
		}
	}
}

// emit appends an event to the thread log and streams it to live subscribers.
func (f *fakeT3) emit(thread string, event map[string]any) {
	f.mu.Lock()
	log := f.threads[thread]
	var seq uint64 = 1
	if len(log) > 0 {
		seq = log[len(log)-1].seq + 1
	}
	f.threads[thread] = append(log, fakeLogged{seq: seq, event: event})
	if event["type"] == "run.updated" {
		r := event["payload"].(map[string]any)
		if f.runs[thread] == nil {
			f.runs[thread] = map[string]string{}
		}
		f.runs[thread][r["id"].(string)] = r["status"].(string)
	}
	var subs []*fakeSub
	for _, s := range f.subs {
		if s.thread == thread {
			subs = append(subs, s)
		}
	}
	f.mu.Unlock()
	for _, s := range subs {
		s.queue <- map[string]any{"kind": "event", "sequence": seq, "event": event}
	}
}

func (f *fakeT3) dropAll() {
	f.mu.Lock()
	conns := append([]*websocket.Conn(nil), f.conns...)
	f.conns = nil
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (f *fakeT3) dispatches(kind string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, r := range f.requests {
		if r.Tag == "orchestration.dispatchCommand" && r.Payload["type"] == kind {
			out = append(out, r.Payload)
		}
	}
	return out
}

func (f *fakeT3) requestsOf(tag string) []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeRequest
	for _, r := range f.requests {
		if r.Tag == tag {
			out = append(out, r)
		}
	}
	return out
}

func item(v map[string]any) map[string]any {
	return map[string]any{"type": "turn-item.updated", "payload": v}
}

func turn(id, node, status string) map[string]any {
	return map[string]any{"type": "provider-turn.updated", "payload": map[string]any{"id": id, "nodeId": node, "status": status}}
}

func runUpdate(id, status string) map[string]any {
	return map[string]any{"type": "run.updated", "runId": id, "payload": map[string]any{"id": id, "status": status}}
}

func errItem(run, id, msg string, retry bool) map[string]any {
	v := map[string]any{"id": id, "type": "error", "status": "failed", "failure": map[string]any{"message": msg}}
	if retry {
		v["retry"] = map[string]any{"attempt": 1}
		v["status"] = "running"
	}
	return map[string]any{"type": "turn-item.updated", "runId": run, "payload": v}
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func nextEvent(t *testing.T, s core.AgentSession) core.Event {
	t.Helper()
	select {
	case ev := <-s.Events():
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
	}
	return core.Event{}
}

// noEvent asserts nothing else arrives; a leftover terminal event would be
// consumed by the next turn as its result.
func noEvent(t *testing.T, s core.AgentSession) {
	t.Helper()
	select {
	case ev := <-s.Events():
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}

type t3Env struct {
	f    *fakeT3
	dir  string
	opts map[string]any
}

func newEnv(t *testing.T) *t3Env {
	const credential = "test-credential-do-not-log"
	f := newFakeT3(t, credential)
	dir := t.TempDir()
	cred := filepath.Join(dir, "credential")
	if err := os.WriteFile(cred, []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	return &t3Env{f: f, dir: dir, opts: map[string]any{"server": f.addr(), "credential_file": cred, "cc_data_dir": dir, "work_dir": dir}}
}

func (e *t3Env) start(t *testing.T, thread string) *Session {
	t.Helper()
	a, err := New(e.opts)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.StartSession(context.Background(), thread)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess.(*Session)
}

func TestStreamingTurnMatchesRealProtocol(t *testing.T) {
	e := newEnv(t)
	s := e.start(t, "")
	th := s.CurrentSessionID()
	if th == "" {
		t.Fatal("no thread")
	}
	if err := s.Send("hi", "m1", nil, nil); err != nil {
		t.Fatalf("dispatch rejected (headers/createdBy): %v", err)
	}
	d := e.f.dispatches("message.dispatch")[0]
	if d["dispatchMode"].(map[string]any)["type"] != "queue_after_active" {
		t.Fatalf("dispatch mode = %v", d["dispatchMode"])
	}
	// T3 resends the whole text each update and updates one tool item twice.
	e.f.emit(th, runUpdate("r1", "running"))
	e.f.emit(th, item(map[string]any{"id": "r1", "type": "reasoning", "text": "think"}))
	e.f.emit(th, item(map[string]any{"id": "r1", "type": "reasoning", "text": "thinking"}))
	e.f.emit(th, item(map[string]any{"id": "a1", "type": "assistant_message", "text": "hel", "streaming": true}))
	e.f.emit(th, item(map[string]any{"id": "c1", "type": "command_execution", "status": "running", "input": "rm -rf /secret"}))
	e.f.emit(th, item(map[string]any{"id": "c1", "type": "command_execution", "status": "completed", "input": "rm -rf /secret"}))
	e.f.emit(th, item(map[string]any{"id": "a1", "type": "assistant_message", "text": "hello", "streaming": false}))
	e.f.emit(th, turn("pt1", "n1", "completed"))
	e.f.emit(th, runUpdate("r1", "completed"))
	e.f.emit(th, runUpdate("r1", "completed")) // a later update of the finished run

	var text, thinking string
	var tools, results int
	for results == 0 {
		ev := nextEvent(t, s)
		switch ev.Type {
		case core.EventText:
			text += ev.Content
		case core.EventThinking:
			thinking += ev.Content
		case core.EventToolUse:
			tools++
			if strings.Contains(ev.ToolInput, "secret") {
				t.Fatalf("tool input leaked into card: %q", ev.ToolInput)
			}
		case core.EventResult:
			results++
		}
	}
	if text != "hello" || thinking != "thinking" || tools != 1 {
		t.Fatalf("text=%q thinking=%q tools=%d", text, thinking, tools)
	}
	noEvent(t, s)
}

func TestFreshBridgesDoNotReuseLaunchCommandIDs(t *testing.T) {
	e := newEnv(t)
	s1 := e.start(t, "")
	_ = os.RemoveAll(filepath.Join(e.dir, "t3"))
	s2 := e.start(t, "")
	if s1.CurrentSessionID() == s2.CurrentSessionID() {
		t.Fatalf("second bridge got the old thread %q back", s1.CurrentSessionID())
	}
}

func TestPermissionAllowOnceAndDeduplicated(t *testing.T) {
	e := newEnv(t)
	s := e.start(t, "")
	th := s.CurrentSessionID()
	if s.SupportsPermissionApprovalAll() {
		t.Fatal("T3 relay must offer allow-once only")
	}
	req := map[string]any{"id": "i1", "type": "approval_request", "requestId": "req-1", "requestKind": "command", "status": "waiting", "prompt": "run date"}
	e.f.emit(th, item(req))
	e.f.emit(th, item(req))
	ev := nextEvent(t, s)
	if ev.Type != core.EventPermissionRequest || ev.RequestID != "req-1" || ev.ToolName != "command" {
		t.Fatalf("permission = %+v", ev)
	}
	noEvent(t, s)
	if err := s.RespondPermission("req-1", core.PermissionResult{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	if got := e.f.dispatches("runtime-request.respond"); len(got) != 1 || got[0]["decision"] != "accept" {
		t.Fatalf("responses = %v", got)
	}
	// The resolved item comes back once more; it must not prompt again.
	req["status"] = "completed"
	e.f.emit(th, item(map[string]any{"id": "i2", "type": "approval_request", "requestId": "req-2", "requestKind": "command", "status": "completed"}))
	noEvent(t, s)
}

func TestUserInputQuestionRoundTrip(t *testing.T) {
	e := newEnv(t)
	s := e.start(t, "")
	th := s.CurrentSessionID()
	q := map[string]any{"id": "u1", "type": "user_input_request", "requestId": "ask-1", "status": "waiting", "questions": []any{
		map[string]any{"id": "q-db", "header": "DB", "question": "Which database?", "options": []any{
			map[string]any{"label": "Postgres", "description": "relational", "value": "pg"},
			map[string]any{"label": "SQLite", "description": "file"},
		}},
		map[string]any{"id": "q-ext", "header": "Ext", "question": "Which extras?", "multiSelect": true, "options": []any{
			map[string]any{"label": "Cache", "description": "c", "value": "cache"},
			map[string]any{"label": "Queue", "description": "q", "value": "queue"},
		}},
	}}
	e.f.emit(th, item(q))
	e.f.emit(th, item(q))
	ev := nextEvent(t, s)
	if ev.Type != core.EventPermissionRequest || ev.ToolName != "AskUserQuestion" || len(ev.Questions) != 2 || ev.Questions[1].MultiSelect != true || len(ev.Questions[0].Options) != 2 {
		t.Fatalf("question event = %+v", ev)
	}
	noEvent(t, s)
	answers := map[string]any{"Which database?": "Postgres", "Which extras?": "Cache, Queue"}
	if err := s.RespondPermission("ask-1", core.PermissionResult{Behavior: "allow", UpdatedInput: map[string]any{"answers": answers}}); err != nil {
		t.Fatal(err)
	}
	got := e.f.dispatches("runtime-request.respond")
	if len(got) != 1 {
		t.Fatalf("responses = %v", got)
	}
	a := got[0]["answers"].(map[string]any)
	if a["q-db"] != "pg" || fmt.Sprint(a["q-ext"]) != "[cache queue]" {
		t.Fatalf("answers = %v", a)
	}
	// Answered request is replayed with its answer; no second prompt.
	q["questionAnswer"] = map[string]any{"q-db": "pg"}
	e.f.emit(th, item(q))
	noEvent(t, s)

	// Declining dismisses the question.
	e.f.emit(th, item(map[string]any{"id": "u2", "type": "user_input_request", "requestId": "ask-2", "status": "waiting", "questions": []any{
		map[string]any{"id": "q1", "header": "H", "question": "Continue?", "options": []any{map[string]any{"label": "Yes", "description": "y"}}},
	}}))
	if ev := nextEvent(t, s); ev.RequestID != "ask-2" {
		t.Fatalf("second question = %+v", ev)
	}
	if err := s.RespondPermission("ask-2", core.PermissionResult{Behavior: "deny"}); err != nil {
		t.Fatal(err)
	}
	if got := e.f.dispatches("thread.user-input.dismiss"); len(got) != 1 || got[0]["requestId"] != "ask-2" {
		t.Fatalf("dismiss = %v", got)
	}
}

func TestStopEndsTurnOnce(t *testing.T) {
	e := newEnv(t)
	s := e.start(t, "")
	th := s.CurrentSessionID()
	// /stop before any turn started: only the stop command, no events.
	if err := s.CancelTurn(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.dispatches("thread.stop")) != 1 {
		t.Fatal("thread.stop not sent")
	}
	noEvent(t, s)

	// /stop while the run is still starting: T3 writes no provider turn,
	// only the interrupt result and the run's final status.
	_ = s.Send("early", "m0", nil, nil)
	e.f.emit(th, runUpdate("r0", "starting"))
	_ = s.CancelTurn()
	e.f.emit(th, item(map[string]any{"id": "x0", "type": "run_interrupt_result", "message": "stopped"}))
	e.f.emit(th, runUpdate("r0", "interrupted"))
	if ev := nextEvent(t, s); ev.Type != core.EventResult {
		t.Fatalf("early stop end = %+v", ev)
	}
	noEvent(t, s)

	_ = s.Send("long job", "m1", nil, nil)
	e.f.emit(th, runUpdate("r1", "running"))
	e.f.emit(th, item(map[string]any{"id": "a1", "type": "assistant_message", "text": "working"}))
	_ = s.CancelTurn()
	e.f.emit(th, turn("pt1", "n1", "interrupted"))
	e.f.emit(th, runUpdate("r1", "interrupted"))
	var results int
	for results == 0 {
		if ev := nextEvent(t, s); ev.Type == core.EventResult {
			results++
		}
	}
	noEvent(t, s)

	// The next message gets its own reply, not a leftover result.
	_ = s.Send("again", "m2", nil, nil)
	e.f.emit(th, item(map[string]any{"id": "a2", "type": "assistant_message", "text": "fresh"}))
	e.f.emit(th, runUpdate("r2", "completed"))
	if ev := nextEvent(t, s); ev.Type != core.EventText || ev.Content != "fresh" {
		t.Fatalf("next turn = %+v", ev)
	}
	if ev := nextEvent(t, s); ev.Type != core.EventResult {
		t.Fatalf("next turn end = %+v", ev)
	}
}

func TestErrorsEndTurnOnceWithReason(t *testing.T) {
	e := newEnv(t)
	s := e.start(t, "")
	th := s.CurrentSessionID()

	// Retried errors are transient; the run then completes normally.
	e.f.emit(th, runUpdate("r1", "running"))
	e.f.emit(th, errItem("r1", "e0", "overloaded", true))
	noEvent(t, s)
	e.f.emit(th, runUpdate("r1", "completed"))
	if ev := nextEvent(t, s); ev.Type != core.EventResult {
		t.Fatalf("after retry = %+v", ev)
	}

	// Retries ran out: the error still carries retry, and its reason is shown once.
	e.f.emit(th, runUpdate("r2", "running"))
	e.f.emit(th, errItem("r2", "e1", "quota exhausted", true))
	e.f.emit(th, turn("pt2", "n2", "failed"))
	e.f.emit(th, runUpdate("r2", "failed"))
	e.f.emit(th, runUpdate("r2", "failed"))
	ev := nextEvent(t, s)
	if ev.Type != core.EventError || !strings.Contains(ev.Error.Error(), "quota exhausted") {
		t.Fatalf("error = %+v", ev)
	}
	noEvent(t, s)

	// A rejected model fails before any provider turn; its error item may
	// arrive after the run update.
	e.f.emit(th, runUpdate("r3", "failed"))
	e.f.emit(th, errItem("r3", "e2", "The agent did not accept the model", false))
	if ev := nextEvent(t, s); ev.Type != core.EventError || !strings.Contains(ev.Error.Error(), "did not accept") {
		t.Fatalf("model error = %+v", ev)
	}
	noEvent(t, s)

	// agy exits mid-turn: provider turn interrupted, then the error, then the run fails.
	e.f.emit(th, runUpdate("r4", "running"))
	e.f.emit(th, item(map[string]any{"id": "a4", "type": "assistant_message", "text": "half"}))
	e.f.emit(th, turn("pt4", "n4", "interrupted"))
	e.f.emit(th, errItem("r4", "e4", "provider exited", false))
	e.f.emit(th, runUpdate("r4", "failed"))
	if ev := nextEvent(t, s); ev.Type != core.EventText {
		t.Fatalf("partial text = %+v", ev)
	}
	if ev := nextEvent(t, s); ev.Type != core.EventError || !strings.Contains(ev.Error.Error(), "provider exited") {
		t.Fatalf("crash = %+v", ev)
	}
	noEvent(t, s)

	// A failed run with no error item still ends with an error.
	e.f.emit(th, runUpdate("r5", "failed"))
	if ev := nextEvent(t, s); ev.Type != core.EventError {
		t.Fatalf("bare failure = %+v", ev)
	}
	noEvent(t, s)
}

func TestRPCErrorsCarryReasonNotPayload(t *testing.T) {
	e := newEnv(t)
	s := e.start(t, "")
	s.threadID.Store("thread-broken")
	err := s.Send("x", "m", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "orchestration.dispatchCommand") || !strings.Contains(err.Error(), "No orchestration projection exists") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-PAYLOAD") {
		t.Fatalf("error carries the whole payload: %v", err)
	}
}

func TestReconnectMidTurnReplaysWithoutDuplicates(t *testing.T) {
	e := newEnv(t)
	s := e.start(t, "")
	th := s.CurrentSessionID()
	_ = s.Send("count", "m1", nil, nil)
	e.f.emit(th, item(map[string]any{"id": "a1", "type": "assistant_message", "text": "1 "}))
	if ev := nextEvent(t, s); ev.Content != "1 " {
		t.Fatalf("first = %+v", ev)
	}
	e.f.dropAll()
	e.f.emit(th, item(map[string]any{"id": "a1", "type": "assistant_message", "text": "1 2 "}))
	e.f.emit(th, item(map[string]any{"id": "a1", "type": "assistant_message", "text": "1 2 3"}))
	e.f.emit(th, runUpdate("r1", "completed"))
	var text string
	for {
		ev := nextEvent(t, s)
		if ev.Type == core.EventText {
			text += ev.Content
		}
		if ev.Type == core.EventResult {
			break
		}
	}
	if text != "2 3" {
		t.Fatalf("replayed text = %q", text)
	}
	noEvent(t, s)
	subs := e.f.requestsOf("orchestration.subscribeThread")
	if last := subs[len(subs)-1].Payload["afterSequence"].(float64); last != 1 {
		t.Fatalf("resubscribed after %v, want 1", last)
	}
}

func TestDropBeforeFirstEventResumesFromSnapshot(t *testing.T) {
	e := newEnv(t)
	first := e.start(t, "")
	th := first.CurrentSessionID()
	for i := 0; i < 3; i++ {
		e.f.emit(th, item(map[string]any{"id": fmt.Sprintf("old%d", i), "type": "assistant_message", "text": "old"}))
	}
	_ = first.Close()

	// A restarted bridge resumes the thread: snapshot, no replay of old turns.
	s := e.start(t, th)
	if s.CurrentSessionID() != th {
		t.Fatalf("resumed %q, want %q", s.CurrentSessionID(), th)
	}
	_ = s.Send("new", "m1", nil, nil)
	waitFor(t, "snapshot", func() bool { s.seqMu.Lock(); defer s.seqMu.Unlock(); return s.sequence == 3 })
	e.f.dropAll()
	e.f.emit(th, item(map[string]any{"id": "a9", "type": "assistant_message", "text": "answer"}))
	e.f.emit(th, runUpdate("r9", "completed"))
	if ev := nextEvent(t, s); ev.Type != core.EventText || ev.Content != "answer" {
		t.Fatalf("after early drop = %+v", ev)
	}
	if ev := nextEvent(t, s); ev.Type != core.EventResult {
		t.Fatalf("end = %+v", ev)
	}
}

// A restarted bridge must not take the last finished run of the resumed
// thread for the end of the next turn.
func TestResumedThreadIgnoresEarlierRuns(t *testing.T) {
	e := newEnv(t)
	first := e.start(t, "")
	th := first.CurrentSessionID()
	_ = first.Send("before restart", "m1", nil, nil)
	e.f.emit(th, item(map[string]any{"id": "a1", "type": "assistant_message", "text": "old"}))
	e.f.emit(th, runUpdate("r1", "completed"))
	for ev := nextEvent(t, first); ev.Type != core.EventResult; ev = nextEvent(t, first) {
	}
	_ = first.Close()

	s := e.start(t, th)
	_ = s.Send("after restart", "m2", nil, nil)
	noEvent(t, s)

	// A reconnect before the new run shows up must not settle the old one either.
	e.f.mu.Lock()
	e.f.snapshotOnResume = true
	e.f.mu.Unlock()
	e.f.dropAll()
	noEvent(t, s)

	e.f.emit(th, item(map[string]any{"id": "a2", "type": "assistant_message", "text": "fresh"}))
	e.f.emit(th, runUpdate("r2", "completed"))
	if ev := nextEvent(t, s); ev.Type != core.EventText || ev.Content != "fresh" {
		t.Fatalf("first event = %+v", ev)
	}
	if ev := nextEvent(t, s); ev.Type != core.EventResult {
		t.Fatalf("end = %+v", ev)
	}
	noEvent(t, s)
}

func TestResumeMissingThreadStartsNewOne(t *testing.T) {
	e := newEnv(t)
	s := e.start(t, "thread-deleted")
	if id := s.CurrentSessionID(); id == "" || id == "thread-deleted" {
		t.Fatalf("thread = %q", id)
	}
	if err := s.Send("hi", "m1", nil, nil); err != nil {
		t.Fatalf("send on replacement thread: %v", err)
	}
	e.f.emit(s.CurrentSessionID(), runUpdate("r1", "completed"))
	if ev := nextEvent(t, s); ev.Type != core.EventResult {
		t.Fatalf("replacement thread is not subscribed: %+v", ev)
	}
	a, _ := New(e.opts)
	ids, _ := a.ListSessions(context.Background())
	if len(ids) == 0 || ids[len(ids)-1].ID != s.CurrentSessionID() {
		t.Fatalf("mapping = %+v", ids)
	}
}

func TestTicketRejectionIsReportedWithoutCredential(t *testing.T) {
	const secret = "credential-secret-for-test"
	var logs strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	f := newFakeT3(t, "other")
	f.ticketErr = http.StatusUnauthorized
	a := &Agent{server: f.addr(), credential: secret, dataDir: t.TempDir(), sessions: map[*Session]struct{}{}}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := newSession(ctx, a, "thread")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("credential leaked, err=%q logs=%q", err, logs.String())
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunEndedWhileDisconnectedSettlesFromSnapshot(t *testing.T) {
	e := newEnv(t)
	s := e.start(t, "")
	th := s.CurrentSessionID()
	_ = s.Send("job", "m1", nil, nil)
	e.f.emit(th, runUpdate("r1", "running"))
	waitFor(t, "run seen", func() bool { s.turnMu.Lock(); defer s.turnMu.Unlock(); return s.runsOpen["r1"] })
	e.f.mu.Lock()
	e.f.snapshotOnResume = true
	e.f.mu.Unlock()
	e.f.dropAll()
	e.f.emit(th, runUpdate("r1", "completed"))
	if ev := nextEvent(t, s); ev.Type != core.EventResult {
		t.Fatalf("snapshot settle = %+v", ev)
	}
	noEvent(t, s)
}

func TestCloseDuringStreamDoesNotPanic(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 20; i++ {
		s := e.start(t, "")
		th := s.CurrentSessionID()
		for j := 0; j < 40; j++ {
			e.f.emit(th, item(map[string]any{"id": fmt.Sprintf("a%d", j), "type": "assistant_message", "text": "x"}))
		}
		_ = s.Close()
		for range s.Events() {
		}
	}
}

func TestSelectedModelTravelsWithEachMessage(t *testing.T) {
	e := newEnv(t)
	a, err := New(e.opts)
	if err != nil {
		t.Fatal(err)
	}
	ms := a.(core.ModelSwitcher)
	ms.SetModel("gemini-first")
	sess, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	launch := e.f.requestsOf("orchestration.launchThread")
	if got := launch[len(launch)-1].Payload["modelSelection"].(map[string]any)["model"]; got != "gemini-first" {
		t.Fatalf("launch model = %v", got)
	}
	ms.SetModel("gemini-second")
	if err := sess.Send("hi", "", nil, nil); err != nil {
		t.Fatalf("send with empty message id: %v", err)
	}
	d := e.f.dispatches("message.dispatch")
	last := d[len(d)-1]
	if last["modelSelection"].(map[string]any)["model"] != "gemini-second" || last["modelSelection"].(map[string]any)["instanceId"] != "antigravity" {
		t.Fatalf("dispatch model = %v", last["modelSelection"])
	}
	if id, _ := last["messageId"].(string); id == "" {
		t.Fatal("empty message id sent to T3")
	}
}

func TestUnmatchedAnswerToChoiceQuestionIsDismissed(t *testing.T) {
	e := newEnv(t)
	s := e.start(t, "")
	th := s.CurrentSessionID()
	e.f.emit(th, item(map[string]any{"id": "u1", "type": "user_input_request", "requestId": "ask-1", "status": "waiting", "questions": []any{
		map[string]any{"id": "q1", "header": "H", "question": "Pick one", "allowCustomAnswer": false, "options": []any{
			map[string]any{"label": "A", "description": "a", "value": "opt-a"},
		}},
	}}))
	_ = nextEvent(t, s)
	answers := map[string]any{"Pick one": "something else"}
	if err := s.RespondPermission("ask-1", core.PermissionResult{Behavior: "allow", UpdatedInput: map[string]any{"answers": answers}}); err != nil {
		t.Fatal(err)
	}
	if len(e.f.dispatches("runtime-request.respond")) != 0 || len(e.f.dispatches("thread.user-input.dismiss")) != 1 {
		t.Fatal("an answer T3 would drop must dismiss the question instead")
	}
}

func TestLaunchUsesProjectThatOwnsTheFolder(t *testing.T) {
	e := newEnv(t)
	e.f.folderOwner = "project-from-t3-app"
	e.start(t, "")
	launch := e.f.requestsOf("orchestration.launchThread")
	if got := launch[len(launch)-1].Payload["projectId"]; got != "project-from-t3-app" {
		t.Fatalf("launched in project %v", got)
	}
}
