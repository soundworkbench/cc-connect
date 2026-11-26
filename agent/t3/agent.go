package t3

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

// SetModel takes the model picked in the chat menu; every message carries it,
// so a switch applies from the next message on existing threads too.
func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	a.model = strings.TrimSpace(model)
	a.mu.Unlock()
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.model == "" {
		return "gemini-2.5-pro"
	}
	return a.model
}

// AvailableModels has no lookup: the menu comes from the configured providers.
func (a *Agent) AvailableModels(context.Context) []core.ModelOption { return nil }

func (a *Agent) modelSelection() map[string]any {
	return map[string]any{"instanceId": a.provider, "model": a.GetModel()}
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
	} else if err := s.subscribe(ctx, sessionID, true); err != nil {
		if !errors.Is(err, errThreadMissing) {
			_ = s.Close()
			return nil, err
		}
		// The thread was deleted in T3 or its data was reset; start a new one
		// rather than failing on the next message.
		slog.Warn("t3: saved thread no longer exists, starting a new one", "thread", sessionID)
		s.threadID.Store("")
		s.seqMu.Lock()
		s.sequence = 0
		s.seqMu.Unlock()
		if err := s.createThread(ctx); err != nil {
			_ = s.Close()
			return nil, err
		}
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
var _ core.ModelSwitcher = (*Agent)(nil)

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
	runID       string
	itemMu      sync.Mutex
	itemText    map[string]string
	itemSeen    map[string]bool
	questions   map[string][]t3Question
	runsOpen    map[string]bool
	runsEnded   map[string]bool
	runReason   map[string]string
	evMu        sync.RWMutex
	evClosed    bool
	subMu       sync.Mutex
	subID       string
	lastErr     atomic.Value
}

type t3Question struct {
	ID          string `json:"id"`
	Header      string `json:"header"`
	Question    string `json:"question"`
	MultiSelect bool   `json:"multiSelect"`
	AllowCustom bool   `json:"allowCustomAnswer"`
	Options     []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
		Value       string `json:"value"`
	} `json:"options"`
}

func newSession(parent context.Context, a *Agent, id string) (*Session, error) {
	ctx, cancel := context.WithCancel(parent)
	s := &Session{agent: a, ctx: ctx, cancel: cancel, pending: map[string]*pending{}, events: make(chan core.Event, 256), reconnect: make(chan struct{}, 1), permissions: map[string]bool{}, runID: randomID(), itemText: map[string]string{}, itemSeen: map[string]bool{}, questions: map[string][]t3Question{}, runsOpen: map[string]bool{}, runsEnded: map[string]bool{}, runReason: map[string]string{}}
	if id != "" {
		s.threadID.Store(id)
	}
	go s.run()
	select {
	case <-time.After(15 * time.Second):
		s.closeAndWait()
		if v, ok := s.lastErr.Load().(error); ok {
			return nil, fmt.Errorf("t3: WebSocket connection timeout: %w", v)
		}
		return nil, errors.New("t3: WebSocket connection timeout")
	case <-s.reconnect:
		return s, nil
	case <-ctx.Done():
		s.closeAndWait()
		if v, ok := s.lastErr.Load().(error); ok {
			return nil, fmt.Errorf("t3: connect: %w (%v)", ctx.Err(), v)
		}
		return nil, ctx.Err()
	}
}

// closeAndWait stops a session that never became ready and waits for its
// connection loop to exit (run closes events last).
func (s *Session) closeAndWait() {
	_ = s.Close()
	for range s.events {
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
func (s *Session) nextID() string            { n := s.next.Add(1); return fmt.Sprintf("cc-t3-%s-%d", s.runID, n) }
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("t3: websocket ticket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("t3: websocket ticket: server returned %s", resp.Status)
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Ticket == "" {
		return nil, errors.New("t3: websocket ticket: no ticket in response")
	}
	u.Path = "/ws"
	q := u.Query()
	q.Set("orchestrationProtocol", "2")
	q.Set("wsTicket", body.Ticket)
	u.RawQuery = q.Encode()
	h = http.Header{}
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
		// Stream goroutines may still be emitting; emit checks evClosed under evMu.
		s.evMu.Lock()
		s.evClosed = true
		close(s.events)
		s.evMu.Unlock()
	}()
	backoff := 200 * time.Millisecond
	ready := false
	for s.ctx.Err() == nil {
		c, err := s.dial()
		if err != nil {
			s.lastErr.Store(err)
			slog.Debug("t3: connect failed", "error", err)
			select {
			case <-time.After(backoff):
			case <-s.ctx.Done():
				return
			}
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
			if err := s.resubscribe(); err != nil {
				slog.Warn("t3: resubscribe after reconnect", "error", err)
			}
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
		slog.Info("t3: connection lost, reconnecting", "error", err)
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
		Snapshot uint64          `json:"snapshotSequence"`
		Event    json.RawMessage `json:"event"`
		Type     string          `json:"type"`
		Payload  json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(b, &env) != nil {
		return
	}
	if env.Kind == "snapshot" {
		s.seqMu.Lock()
		s.sequence = max(s.sequence, env.Snapshot)
		s.seqMu.Unlock()
		s.settleFromSnapshot(b)
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
		RunID   string          `json:"runId"`
		Payload json.RawMessage `json:"payload"`
		Item    json.RawMessage `json:"item"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	if e.Type == "turn-item.updated" || e.Type == "turn_item.updated" {
		s.emitItem(e.Payload, e.Item, e.RunID)
		return
	}
	if e.Type == "run.updated" {
		var r struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if json.Unmarshal(e.Payload, &r) == nil && r.ID != "" {
			s.trackRun(r.ID, r.Status)
		}
		return
	}
}

func runTerminal(status string) bool {
	switch status {
	case "completed", "interrupted", "failed", "cancelled", "rolled_back":
		return true
	}
	return false
}

// trackRun ends the engine's turn once per T3 run. Every run, including one
// stopped before the provider started or one whose model was rejected, ends
// with a terminal run.updated; provider turns and error items do not cover
// all of those cases and can arrive in either order.
func (s *Session) trackRun(id, status string) {
	s.turnMu.Lock()
	if s.runsEnded[id] {
		s.turnMu.Unlock()
		return
	}
	if !runTerminal(status) {
		s.runsOpen[id] = true
		s.turnMu.Unlock()
		return
	}
	s.runsEnded[id] = true
	delete(s.runsOpen, id)
	s.turnOpen = false
	s.turnMu.Unlock()
	if status != "failed" {
		s.emit(core.Event{Type: core.EventResult, Done: true})
		return
	}
	// The failure's error item can follow the run update; give it a moment.
	go func() {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-s.ctx.Done():
			return
		}
		s.turnMu.Lock()
		reason := s.runReason[id]
		delete(s.runReason, id)
		s.turnMu.Unlock()
		if reason == "" {
			reason = "turn failed"
		}
		s.emit(core.Event{Type: core.EventError, Error: errors.New("t3: " + reason)})
	}()
}

// settleFromSnapshot ends a turn whose run finished while we were
// disconnected and T3 answered the resubscribe with a snapshot.
func (s *Session) settleFromSnapshot(b []byte) {
	var snap struct {
		Projection struct {
			Runs []struct {
				ID      string `json:"id"`
				Ordinal int    `json:"ordinal"`
				Status  string `json:"status"`
			} `json:"runs"`
		} `json:"projection"`
	}
	if json.Unmarshal(b, &snap) != nil {
		return
	}
	s.turnMu.Lock()
	waiting := s.turnOpen
	latest, latestOrd := "", -1
	var settle []string
	for _, r := range snap.Projection.Runs {
		if r.Ordinal > latestOrd {
			latest, latestOrd = r.ID, r.Ordinal
		}
		if runTerminal(r.Status) && s.runsOpen[r.ID] {
			settle = append(settle, r.ID+"\x00"+r.Status)
		}
	}
	for _, r := range snap.Projection.Runs {
		if r.ID == latest && waiting && runTerminal(r.Status) && !s.runsOpen[r.ID] && !s.runsEnded[r.ID] {
			settle = append(settle, r.ID+"\x00"+r.Status)
		}
	}
	s.turnMu.Unlock()
	for _, x := range settle {
		id, status, _ := strings.Cut(x, "\x00")
		s.trackRun(id, status)
	}
}

func (s *Session) emitItem(payload, item json.RawMessage, runID string) {
	raw := item
	if len(raw) == 0 {
		raw = payload
	}
	var v struct {
		ID        string `json:"id"`
		NodeID    string `json:"nodeId"`
		Kind      string `json:"requestKind"`
		Type      string `json:"type"`
		Status    string `json:"status"`
		Text      string `json:"text"`
		ToolName  string `json:"toolName"`
		RequestID string `json:"requestId"`
		Failure   struct {
			Message string `json:"message"`
		} `json:"failure"`
		Retry     json.RawMessage `json:"retry"`
		Prompt    string          `json:"prompt"`
		Questions []t3Question    `json:"questions"`
		Answer    json.RawMessage `json:"questionAnswer"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	switch v.Type {
	case "assistant_message":
		if delta := s.itemDelta(v.ID, v.Text); delta != "" {
			s.emit(core.Event{Type: core.EventText, Content: delta})
		}
	case "reasoning":
		if delta := s.itemDelta(v.ID, v.Text); delta != "" {
			s.emit(core.Event{Type: core.EventThinking, Content: delta})
		}
	case "command_execution":
		if s.firstSight(v.ID) {
			s.emit(core.Event{Type: core.EventToolUse, ProgressID: v.ID, ToolName: "command", ToolInput: "execution"})
		}
	case "dynamic_tool":
		if s.firstSight(v.ID) {
			s.emit(core.Event{Type: core.EventToolUse, ProgressID: v.ID, ToolName: v.ToolName, ToolInput: "tool call"})
		}
	case "approval_request":
		if requestSettled(v.Status) || !s.firstRequest(v.RequestID) {
			return
		}
		s.emit(core.Event{Type: core.EventPermissionRequest, RequestID: v.RequestID, ToolName: v.Kind, ToolInput: v.Prompt})
	case "user_input_request":
		if requestSettled(v.Status) || len(v.Answer) > 0 || len(v.Questions) == 0 || !s.firstRequest(v.RequestID) {
			return
		}
		s.permMu.Lock()
		s.questions[v.RequestID] = v.Questions
		s.permMu.Unlock()
		qs := make([]core.UserQuestion, 0, len(v.Questions))
		for _, q := range v.Questions {
			uq := core.UserQuestion{Question: q.Question, Header: q.Header, MultiSelect: q.MultiSelect}
			for _, o := range q.Options {
				uq.Options = append(uq.Options, core.UserQuestionOption{Label: o.Label, Description: o.Description})
			}
			qs = append(qs, uq)
		}
		s.emit(core.Event{Type: core.EventPermissionRequest, RequestID: v.RequestID, ToolName: "AskUserQuestion", ToolInput: qs[0].Question, Questions: qs})
	case "error":
		// Errors end nothing themselves: a retried one is transient and a final
		// one is reported when its run fails.
		if runID == "" {
			slog.Warn("t3: provider error outside a run", "error", v.Failure.Message)
			return
		}
		s.turnMu.Lock()
		s.runReason[runID] = v.Failure.Message
		s.turnMu.Unlock()
	}
}

// itemDelta returns the new part of an item's text: T3 resends the whole text
// on every update while the engine appends.
func (s *Session) itemDelta(id, text string) string {
	s.itemMu.Lock()
	defer s.itemMu.Unlock()
	prev := s.itemText[id]
	s.itemText[id] = text
	if !strings.HasPrefix(text, prev) {
		return text
	}
	return text[len(prev):]
}

// firstSight reports whether an item id is new, so updates of one tool call
// show as one step.
func (s *Session) firstSight(id string) bool {
	s.itemMu.Lock()
	defer s.itemMu.Unlock()
	if id != "" && s.itemSeen[id] {
		return false
	}
	if id != "" {
		s.itemSeen[id] = true
	}
	return true
}

func (s *Session) firstRequest(id string) bool {
	s.permMu.Lock()
	defer s.permMu.Unlock()
	if s.permissions[id] {
		return false
	}
	s.permissions[id] = true
	return true
}

func requestSettled(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "interrupted":
		return true
	}
	return false
}

func (s *Session) emit(ev core.Event) {
	s.evMu.RLock()
	defer s.evMu.RUnlock()
	if s.evClosed {
		return
	}
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
	b, _ := json.Marshal(map[string]any{"_tag": "Request", "id": id, "tag": tag, "payload": payload, "headers": []any{}})
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
			return nil, &rpcError{detail: errDetail(ch.Error), raw: ch.Error}
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
					return nil, &rpcError{detail: errDetail(ch.Exit), raw: ch.Exit}
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
	out, err := s.await(ctx, p, id)
	if err != nil {
		err = fmt.Errorf("%s: %w", tag, err)
	}
	return out, err
}
func (s *Session) resubscribe() error {
	id := s.CurrentSessionID()
	if id == "" {
		return nil
	}
	return s.subscribe(s.ctx, id, false)
}

// subscribe opens the thread's event stream from the last sequence seen. With
// wait it blocks until T3 accepts or rejects the subscription, so a thread that
// no longer exists is reported here rather than on the next message.
func (s *Session) subscribe(ctx context.Context, id string, wait bool) error {
	s.subMu.Lock()
	if s.subscribed.Load() {
		s.subMu.Unlock()
		return nil
	}
	s.seqMu.Lock()
	after := s.sequence
	s.seqMu.Unlock()
	// Without afterSequence T3 starts with a snapshot, and reports a thread
	// that does not exist instead of waiting silently for its first event.
	payload := map[string]any{"threadId": id}
	if after > 0 {
		payload["afterSequence"] = after
	}
	p, rid, e := s.request(ctx, "orchestration.subscribeThread", payload, true)
	if e != nil {
		s.subMu.Unlock()
		return e
	}
	s.subID = rid
	s.subscribed.Store(true)
	s.subMu.Unlock()
	first := make(chan error, 1)
	go s.consumeStream(p, rid, first)
	if !wait {
		return nil
	}
	select {
	case err := <-first:
		if err != nil {
			return fmt.Errorf("orchestration.subscribeThread: %w", err)
		}
		return nil
	case <-time.After(15 * time.Second):
		s.dropSubscription(rid)
		return errors.New("t3: thread subscription timeout")
	case <-ctx.Done():
		s.dropSubscription(rid)
		return ctx.Err()
	}
}

var errThreadMissing = errors.New("t3: thread does not exist")

// dropSubscription forgets a subscription so the next subscribe starts afresh.
func (s *Session) dropSubscription(rid string) {
	s.pendingMu.Lock()
	delete(s.pending, rid)
	s.pendingMu.Unlock()
	s.subMu.Lock()
	if s.subID == rid {
		s.subscribed.Store(false)
		s.subID = ""
	}
	s.subMu.Unlock()
}

func (s *Session) consumeStream(p *pending, rid string, first chan<- error) {
	report := func(err error) {
		select {
		case first <- err:
		default:
		}
	}
	end := func(err error) {
		report(err)
		s.dropSubscription(rid)
	}
	for {
		select {
		case ch := <-p.ch:
			if len(ch.Error) > 0 {
				end(fmt.Errorf("t3: rpc failed: %s", errDetail(ch.Error)))
				return
			}
			if ch.Tag == "Exit" || len(ch.Exit) > 0 {
				var exit struct {
					Tag string `json:"_tag"`
				}
				_ = json.Unmarshal(ch.Exit, &exit)
				if exit.Tag == "Failure" {
					detail := errDetail(ch.Exit)
					err := fmt.Errorf("t3: rpc failed: %s", detail)
					if strings.Contains(detail, "No orchestration projection exists") {
						err = fmt.Errorf("%w: %s", errThreadMissing, detail)
					}
					slog.Warn("t3: thread subscription ended", "error", err)
					end(err)
				} else {
					end(errors.New("t3: thread subscription ended"))
				}
				return
			}
			report(nil)
			for _, v := range ch.Values {
				s.handleEvent(v)
			}
			if ch.Tag == "Chunk" {
				s.ack(rid)
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Session) createThread(ctx context.Context) error {
	projectID := projectIDFor(s.agent.workDir)
	// The project usually exists already. When the folder was added in the T3
	// app it belongs to that project, and threads must be launched there.
	if _, err := s.call(ctx, "projects.mutate", map[string]any{"type": "project.create", "commandId": s.nextID(), "projectId": projectID, "title": "cc-connect", "workspaceRoot": s.agent.workDir}); err != nil {
		if owner := conflictingProject(err); owner != "" {
			projectID = owner
		} else {
			slog.Debug("t3: create project", "error", err)
		}
	}
	payload := map[string]any{"commandId": s.nextID(), "projectId": projectID, "title": "cc-connect", "modelSelection": s.agent.modelSelection(), "runtimeMode": "approval-required", "interactionMode": "default", "workspaceStrategy": map[string]any{"type": "root"}}
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
	// Wait for the snapshot so the first message is sent on a live stream.
	return s.subscribe(ctx, out.ThreadID, true)
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
	if strings.TrimSpace(messageID) == "" {
		messageID = s.nextID() // T3 rejects an empty message id
	}
	s.turnMu.Lock()
	s.turnOpen = true
	s.turnMu.Unlock()
	payload := map[string]any{"type": "message.dispatch", "createdBy": "user", "creationSource": "web", "commandId": s.nextID(), "threadId": id, "messageId": messageID, "text": prompt, "attachments": []any{}, "dispatchMode": map[string]any{"type": "queue_after_active"}, "modelSelection": s.agent.modelSelection()}
	_, e := s.call(s.ctx, "orchestration.dispatchCommand", payload)
	return e
}
func (s *Session) RespondPermission(requestID string, result core.PermissionResult) error {
	id := s.CurrentSessionID()
	s.permMu.Lock()
	qs, isQuestion := s.questions[requestID]
	delete(s.questions, requestID)
	s.permMu.Unlock()
	if isQuestion {
		answers := questionAnswers(qs, result)
		payload := map[string]any{"type": "thread.user-input.dismiss", "commandId": s.nextID(), "threadId": id, "requestId": requestID}
		if len(answers) > 0 {
			payload = map[string]any{"type": "runtime-request.respond", "commandId": s.nextID(), "threadId": id, "requestId": requestID, "answers": answers}
		}
		_, e := s.call(s.ctx, "orchestration.dispatchCommand", payload)
		return e
	}
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

// errDetail returns T3's error text only, never the whole payload.
func errDetail(raw json.RawMessage) string {
	var exit struct {
		Cause []struct {
			Defect any `json:"defect"`
			Error  struct {
				Tag     string `json:"_tag"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"cause"`
	}
	if json.Unmarshal(raw, &exit) == nil {
		for _, c := range exit.Cause {
			if c.Error.Tag != "" || c.Error.Message != "" {
				return strings.TrimSpace(c.Error.Tag + " " + c.Error.Message)
			}
			if d, ok := c.Defect.(string); ok {
				return d
			}
		}
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	return "no reason given"
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ack releases the next chunk of a streamed RPC; the server waits for it.
func (s *Session) ack(requestID string) {
	s.connMu.RLock()
	c := s.conn
	s.connMu.RUnlock()
	if c == nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := c.WriteMessage(websocket.TextMessage, mustJSON(map[string]any{"_tag": "Ack", "requestId": requestID})); err != nil {
		slog.Warn("t3: ack stream chunk", "error", err)
	}
}

// SupportsPermissionApprovalAll hides "allow all": the relay only approves once.
func (s *Session) SupportsPermissionApprovalAll() bool { return false }

// questionAnswers turns the engine's answers (keyed by question text, labels
// joined by ", " for multi-select) into T3's answers keyed by question id,
// using each option's value when it has one.
func questionAnswers(qs []t3Question, result core.PermissionResult) map[string]any {
	if !strings.EqualFold(result.Behavior, "allow") {
		return nil
	}
	given, _ := result.UpdatedInput["answers"].(map[string]any)
	out := map[string]any{}
	for _, q := range qs {
		ans, _ := given[q.Question].(string)
		ans = strings.TrimSpace(ans)
		if ans == "" {
			continue
		}
		// T3 drops an answer that matches no option unless the question takes
		// free text, so such an answer is left out rather than lost silently.
		valueOf := func(label string) (string, bool) {
			for _, o := range q.Options {
				if o.Label == label {
					if o.Value != "" {
						return o.Value, true
					}
					return label, true
				}
			}
			return label, q.AllowCustom || len(q.Options) == 0
		}
		if q.MultiSelect {
			var picked []string
			for _, part := range strings.Split(ans, ", ") {
				if v, ok := valueOf(part); ok {
					picked = append(picked, v)
				}
			}
			if len(picked) > 0 {
				out[q.ID] = picked
			}
			continue
		}
		if v, ok := valueOf(ans); ok {
			out[q.ID] = v
		}
	}
	return out
}

// rpcError keeps T3's failure for callers that need a field from it; its text
// is only T3's tag and message.
type rpcError struct {
	detail string
	raw    json.RawMessage
}

func (e *rpcError) Error() string { return "t3: rpc failed: " + e.detail }

// conflictingProject returns the project that already owns the workspace
// folder, from T3's ProjectConflictError.
func conflictingProject(err error) string {
	var re *rpcError
	if !errors.As(err, &re) {
		return ""
	}
	var exit struct {
		Cause []struct {
			Error struct {
				Tag      string `json:"_tag"`
				Conflict string `json:"conflictingProjectId"`
			} `json:"error"`
		} `json:"cause"`
	}
	if json.Unmarshal(re.raw, &exit) != nil {
		return ""
	}
	for _, c := range exit.Cause {
		if c.Error.Tag == "ProjectConflictError" && c.Error.Conflict != "" {
			return c.Error.Conflict
		}
	}
	return ""
}
