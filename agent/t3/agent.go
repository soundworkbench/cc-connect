package t3

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

func init() { core.RegisterAgent("t3", New) }

type Agent struct {
	server, credential, provider, model, workDir, dataDir string
	mu                                                    sync.Mutex
	sessions                                              map[*Session]struct{}
}

func New(opts map[string]any) (core.Agent, error) {
	get := func(k string) string { v, _ := opts[k].(string); return strings.TrimSpace(v) }
	a := &Agent{server: get("server"), provider: get("provider"), model: get("model"), workDir: get("work_dir"), dataDir: get("cc_data_dir"), sessions: map[*Session]struct{}{}}
	if a.server == "" {
		a.server = "127.0.0.1:6768"
	}
	if a.provider == "" {
		a.provider = "antigravity"
	}
	if a.workDir == "" {
		a.workDir = "."
	}
	if a.dataDir == "" {
		a.dataDir = ".cc-connect"
	}
	if file := get("credential_file"); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("t3: read credential_file: %w", err)
		}
		a.credential = strings.TrimSpace(string(b))
		if a.credential == "" {
			return nil, errors.New("t3: credential_file is empty")
		}
	}
	return a, nil
}
func (a *Agent) Name() string { return "t3" }
func (a *Agent) Stop() error {
	a.mu.Lock()
	ss := make([]*Session, 0, len(a.sessions))
	for s := range a.sessions {
		ss = append(ss, s)
	}
	a.sessions = map[*Session]struct{}{}
	a.mu.Unlock()
	for _, s := range ss {
		_ = s.Close()
	}
	return nil
}
func (a *Agent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) {
	b, err := os.ReadFile(filepath.Join(a.dataDir, "t3", "threads.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("t3: read thread mapping: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		return nil, fmt.Errorf("t3: decode thread mapping: %w", err)
	}
	out := make([]core.AgentSessionInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, core.AgentSessionInfo{ID: id})
	}
	return out, nil
}
func (a *Agent) remember(id string) {
	if id == "" {
		return
	}
	dir := filepath.Join(a.dataDir, "t3")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("t3: save thread mapping", "error", err)
		return
	}
	path := filepath.Join(dir, "threads.json")
	var ids []string
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &ids)
	}
	for _, old := range ids {
		if old == id {
			return
		}
	}
	ids = append(ids, id)
	_ = core.AtomicWriteFile(path, mustJSON(ids), 0o600)
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	s, err := newSession(ctx, a, sessionID)
	if err != nil {
		return nil, err
	}
	if sessionID == "" {
		if err := s.createThread(ctx); err != nil {
			_ = s.Close()
			return nil, err
		}
	} else if _, err := s.subscribe(ctx, sessionID); err != nil {
		_ = s.Close()
		return nil, err
	}
	a.remember(s.CurrentSessionID())
	a.mu.Lock()
	a.sessions[s] = struct{}{}
	a.mu.Unlock()
	return s, nil
}

// AgentDoctorInfo keeps diagnostics useful without exposing the credential.
func (a *Agent) CLIBinaryName() string  { return "t3" }
func (a *Agent) CLIDisplayName() string { return "T3 Code" }

var _ core.Agent = (*Agent)(nil)
var _ core.AgentDoctorInfo = (*Agent)(nil)

type rpcRequest struct {
	Tag       string `json:"tag"`
	ID        string `json:"id"`
	Payload   any    `json:"payload"`
	TagLegacy string `json:"method,omitempty"`
}
type rpcChunk struct {
	Tag       string            `json:"_tag"`
	RequestID string            `json:"requestId"`
	Values    []json.RawMessage `json:"values"`
	Value     json.RawMessage   `json:"value"`
	Error     json.RawMessage   `json:"error"`
	Exit      json.RawMessage   `json:"exit"`
}

type pending struct {
	ch     chan rpcChunk
	stream bool
}

type Session struct {
	agent       *Agent
	ctx         context.Context
	cancel      context.CancelFunc
	threadID    atomic.Value
	connMu      sync.RWMutex
	conn        *websocket.Conn
	writeMu     sync.Mutex
	pendingMu   sync.Mutex
	pending     map[string]*pending
	next        atomic.Uint64
	events      chan core.Event
	seqMu       sync.Mutex
	sequence    uint64
	subscribed  atomic.Bool
	closed      atomic.Bool
	reconnect   chan struct{}
	closeOnce   sync.Once
	permMu      sync.Mutex
	permissions map[string]bool
	turnMu      sync.Mutex
	turnOpen    bool
}

func newSession(parent context.Context, a *Agent, id string) (*Session, error) {
	ctx, cancel := context.WithCancel(parent)
	s := &Session{agent: a, ctx: ctx, cancel: cancel, pending: map[string]*pending{}, events: make(chan core.Event, 256), reconnect: make(chan struct{}, 1), permissions: map[string]bool{}}
	if id != "" {
		s.threadID.Store(id)
	}
	go s.run()
	select {
	case <-time.After(15 * time.Second):
		_ = s.Close()
		return nil, errors.New("t3: WebSocket connection timeout")
	case <-s.reconnect:
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *Session) CurrentSessionID() string {
	if v := s.threadID.Load(); v != nil {
		return v.(string)
	}
	return ""
}
func (s *Session) Events() <-chan core.Event { return s.events }
func (s *Session) Alive() bool               { return !s.closed.Load() }
func (s *Session) nextID() string            { n := s.next.Add(1); return fmt.Sprintf("cc-t3-%d", n) }
func (s *Session) dial() (*websocket.Conn, error) {
	server := s.agent.server
	base := server
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("t3: parse server: %w", err)
	}
	ticketURL := strings.TrimRight(u.String(), "/") + "/api/auth/websocket-ticket"
	h := http.Header{}
	if s.agent.credential != "" {
		h.Set("Authorization", "Bearer "+s.agent.credential)
	}
	req, _ := http.NewRequestWithContext(s.ctx, http.MethodPost, ticketURL, nil)
	req.Header = h
	if resp, e := http.DefaultClient.Do(req); e == nil {
		defer resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			var body struct {
				Ticket string `json:"ticket"`
			}
			if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Ticket != "" {
				u.Path = "/ws"
				q := u.Query()
				q.Set("orchestrationProtocol", "2")
				q.Set("wsTicket", body.Ticket)
				u.RawQuery = q.Encode()
				h = http.Header{}
			}
		}
	}
	if u.Scheme == "http" {
		u.Scheme = "ws"
	} else if u.Scheme == "https" {
		u.Scheme = "wss"
	}
	if u.Path == "" {
		u.Path = "/ws"
	}
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	c, _, err := d.DialContext(s.ctx, u.String(), h)
	if err != nil {
		return nil, fmt.Errorf("t3: connect server: %w", err)
	}
	return c, nil
}
func (s *Session) run() {
	defer func() {
		s.closed.Store(true)
		s.cancel()
		s.failPending(errors.New("t3: session closed"))
		close(s.events)
	}()
	backoff := 200 * time.Millisecond
	ready := false
	for s.ctx.Err() == nil {
		c, err := s.dial()
		if err != nil {
			time.Sleep(backoff)
			if backoff < 5*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 200 * time.Millisecond
		s.connMu.Lock()
		s.conn = c
		s.connMu.Unlock()
		if !ready {
			ready = true
			select {
			case s.reconnect <- struct{}{}:
			default:
			}
		} else {
			_ = s.resubscribe()
		}
		err = s.readLoop(c)
		_ = c.Close()
		s.subscribed.Store(false)
		s.failPending(errors.New("t3: connection lost"))
		s.connMu.Lock()
		if s.conn == c {
			s.conn = nil
		}
		s.connMu.Unlock()
		if s.ctx.Err() != nil {
			return
		}
		_ = err
	}
}
func (s *Session) readLoop(c *websocket.Conn) error {
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			return err
		}
		var m rpcChunk
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if m.RequestID != "" {
			s.pendingMu.Lock()
			p := s.pending[m.RequestID]
			s.pendingMu.Unlock()
			if p != nil {
				select {
				case p.ch <- m:
				default:
				}
			}
			continue
		}
		s.handleEvent(b)
	}
}
func (s *Session) handleEvent(b []byte) {
	var env struct {
		Kind     string          `json:"kind"`
		Sequence uint64          `json:"sequence"`
		Event    json.RawMessage `json:"event"`
		Type     string          `json:"type"`
		Payload  json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(b, &env) != nil {
		return
	}
	if env.Sequence > 0 {
		s.seqMu.Lock()
		if env.Sequence <= s.sequence {
			s.seqMu.Unlock()
			return
		}
		s.sequence = env.Sequence
		s.seqMu.Unlock()
	}
	raw := env.Event
	if len(raw) == 0 {
		raw = env.Payload
	}
	if len(raw) == 0 {
		return
	}
	var e struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
		Item    json.RawMessage `json:"item"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	if e.Type == "turn-item.updated" || e.Type == "turn_item.updated" {
		s.emitItem(e.Payload, e.Item)
		return
	}
	if e.Type == "provider-turn.updated" || e.Type == "provider_turn.updated" {
		var p struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if p.Status == "completed" || p.Status == "failed" || p.Status == "cancelled" || p.Status == "interrupted" {
			s.turnMu.Lock()
			s.turnOpen = false
			s.turnMu.Unlock()
			s.emit(core.Event{Type: core.EventResult, Done: true})
		}
		return
	}
	if e.Type == "runtime-request.updated" {
		var p struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.Status == "pending" {
			s.emit(core.Event{Type: core.EventPermissionRequest, RequestID: p.ID, ToolName: p.Kind})
		}
		return
	}
}
func (s *Session) emitItem(payload, item json.RawMessage) {
	raw := item
	if len(raw) == 0 {
		raw = payload
	}
	var v struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		ToolName  string `json:"toolName"`
		RequestID string `json:"requestId"`
		Reason    string `json:"reason"`
		Prompt    string `json:"prompt"`
		Streaming bool   `json:"streaming"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	switch v.Type {
	case "assistant_message":
		s.emit(core.Event{Type: core.EventText, Content: v.Text, Done: !v.Streaming})
	case "reasoning":
		s.emit(core.Event{Type: core.EventThinking, Content: v.Text})
	case "command_execution":
		s.emit(core.Event{Type: core.EventToolUse, ToolName: "command", ToolInput: "execution"})
	case "dynamic_tool":
		s.emit(core.Event{Type: core.EventToolUse, ToolName: v.ToolName, ToolInput: "tool call"})
	case "approval_request":
		s.emit(core.Event{Type: core.EventPermissionRequest, RequestID: v.RequestID, ToolName: v.ToolName, Content: v.Prompt})
	case "error":
		s.emit(core.Event{Type: core.EventError, Error: errors.New(v.Reason)})
	case "run_interrupt_result":
		s.emit(core.Event{Type: core.EventResult, Content: v.Text, Done: true})
	}
}
func (s *Session) emit(ev core.Event) {
	select {
	case s.events <- ev:
	case <-s.ctx.Done():
	}
}
func (s *Session) failPending(err error) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for id, p := range s.pending {
		select {
		case p.ch <- rpcChunk{Error: json.RawMessage(fmt.Sprintf("%q", err.Error()))}:
		default:
		}
		delete(s.pending, id)
	}
}
func (s *Session) request(ctx context.Context, tag string, payload any, stream bool) (*pending, string, error) {
	s.connMu.RLock()
	c := s.conn
	s.connMu.RUnlock()
	if c == nil {
		return nil, "", errors.New("t3: server disconnected")
	}
	id := s.nextID()
	p := &pending{ch: make(chan rpcChunk, 8), stream: stream}
	s.pendingMu.Lock()
	s.pending[id] = p
	s.pendingMu.Unlock()
	b, _ := json.Marshal(map[string]any{"_tag": "Request", "id": id, "tag": tag, "payload": payload})
	s.writeMu.Lock()
	err := c.WriteMessage(websocket.TextMessage, b)
	s.writeMu.Unlock()
	if err != nil {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
		return nil, "", fmt.Errorf("t3: send %s: %w", tag, err)
	}
	return p, id, nil
}
func (s *Session) await(ctx context.Context, p *pending, id string) (json.RawMessage, error) {
	defer func() { s.pendingMu.Lock(); delete(s.pending, id); s.pendingMu.Unlock() }()
	select {
	case ch := <-p.ch:
		if len(ch.Error) > 0 {
			return nil, fmt.Errorf("t3: rpc failed")
		}
		if len(ch.Values) > 0 {
			return ch.Values[0], nil
		}
		if len(ch.Exit) > 0 {
			var exit struct {
				Tag   string          `json:"_tag"`
				Value json.RawMessage `json:"value"`
			}
			if json.Unmarshal(ch.Exit, &exit) == nil {
				if exit.Tag == "Failure" {
					return nil, errors.New("t3: rpc failed")
				}
				return exit.Value, nil
			}
		}
		return ch.Value, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *Session) call(ctx context.Context, tag string, payload any) (json.RawMessage, error) {
	p, id, e := s.request(ctx, tag, payload, false)
	if e != nil {
		return nil, e
	}
	return s.await(ctx, p, id)
}
func (s *Session) resubscribe() error {
	id := s.CurrentSessionID()
	if id == "" {
		return nil
	}
	_, err := s.subscribe(s.ctx, id)
	return err
}
func (s *Session) subscribe(ctx context.Context, id string) (*pending, error) {
	s.pendingMu.Lock()
	if s.subscribed.Load() {
		s.pendingMu.Unlock()
		return nil, nil
	}
	s.pendingMu.Unlock()
	p, rid, e := s.request(ctx, "orchestration.subscribeThread", map[string]any{"threadId": id, "afterSequence": s.sequence}, true)
	if e != nil {
		return nil, e
	}
	s.subscribed.Store(true)
	go func() {
		for {
			select {
			case ch := <-p.ch:
				for _, v := range ch.Values {
					s.handleEvent(v)
				}
			case <-s.ctx.Done():
				return
			}
		}
	}()
	_ = rid
	return p, nil
}
func (s *Session) createThread(ctx context.Context) error {
	projectID := projectIDFor(s.agent.workDir)
	_, _ = s.call(ctx, "projects.mutate", map[string]any{"type": "project.create", "commandId": s.nextID(), "projectId": projectID, "title": "cc-connect", "workspaceRoot": s.agent.workDir})
	model := s.agent.model
	if model == "" {
		model = "gemini-2.5-pro"
	}
	payload := map[string]any{"commandId": s.nextID(), "projectId": projectID, "title": "cc-connect", "modelSelection": map[string]any{"instanceId": s.agent.provider, "model": model}, "runtimeMode": "approval-required", "interactionMode": "default", "workspaceStrategy": map[string]any{"type": "root"}}
	raw, e := s.call(ctx, "orchestration.launchThread", payload)
	if e != nil {
		return e
	}
	var out struct {
		ThreadID string `json:"threadId"`
	}
	if json.Unmarshal(raw, &out) != nil || out.ThreadID == "" {
		return errors.New("t3: launchThread returned no thread id")
	}
	s.threadID.Store(out.ThreadID)
	s.agent.remember(out.ThreadID)
	_, e = s.subscribe(ctx, out.ThreadID)
	return e
}

func projectIDFor(workDir string) string {
	h := sha256.Sum256([]byte(workDir))
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", h[:4], h[4:6], (uint16(h[6])<<8|uint16(h[7]))&0x0fff|0x4000, (uint16(h[8])<<8|uint16(h[9]))&0x3fff|0x8000, h[10:16])
}
func (s *Session) Send(prompt, messageID string, _ []core.ImageAttachment, _ []core.FileAttachment) error {
	id := s.CurrentSessionID()
	if id == "" {
		return errors.New("t3: no thread")
	}
	s.turnMu.Lock()
	s.turnOpen = true
	s.turnMu.Unlock()
	payload := map[string]any{"type": "message.dispatch", "commandId": s.nextID(), "threadId": id, "messageId": messageID, "text": prompt, "attachments": []any{}, "dispatchMode": map[string]any{"type": "queue_after_active"}}
	_, e := s.call(s.ctx, "orchestration.dispatchCommand", payload)
	return e
}
func (s *Session) RespondPermission(requestID string, result core.PermissionResult) error {
	id := s.CurrentSessionID()
	decision := "decline"
	if result.Behavior == "allow" {
		decision = "accept"
	}
	payload := map[string]any{"type": "runtime-request.respond", "commandId": s.nextID(), "threadId": id, "requestId": requestID, "decision": decision}
	_, e := s.call(s.ctx, "orchestration.dispatchCommand", payload)
	return e
}
func (s *Session) CancelTurn() error {
	id := s.CurrentSessionID()
	if id == "" {
		return nil
	}
	payload := map[string]any{"type": "thread.stop", "commandId": s.nextID(), "threadId": id, "reason": "user"}
	_, e := s.call(s.ctx, "orchestration.dispatchCommand", payload)
	return e
}
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.cancel()
		s.connMu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.connMu.Unlock()
	})
	return nil
}

var _ core.AgentSession = (*Session)(nil)
var _ core.AgentSessionCanceller = (*Session)(nil)
