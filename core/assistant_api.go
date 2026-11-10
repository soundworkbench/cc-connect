package core

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// These endpoints are local automation admission, not outbound platform send.
// They resolve existing workspace bindings and never create an unknown route.
type assistantRequest struct {
	Source               string `json:"source,omitempty"`
	Urgent               bool   `json:"urgent,omitempty"`
	Project              string `json:"project"`
	SessionKey           string `json:"session_key"`
	Message              string `json:"message,omitempty"`
	RequestID            string `json:"request_id"`
	ExpectedSessionID    string `json:"expected_session_id,omitempty"`
	ExpectedHistoryCount int    `json:"expected_history_count,omitempty"`
	Summary              string `json:"summary,omitempty"`
}
type assistantReceipt struct {
	digest     [32]byte
	result     map[string]any
	acceptedAt time.Time
}

func (s *APIServer) assistantEngine(project, key string) (*Engine, error) {
	if project == "" || key == "" {
		return nil, fmt.Errorf("project and session_key required")
	}
	s.mu.RLock()
	e := s.engines[project]
	s.mu.RUnlock()
	if e == nil {
		return nil, fmt.Errorf("unknown project")
	}

	return e, nil
}
func assistantDecode(w http.ResponseWriter, r *http.Request) (assistantRequest, bool) {
	var req assistantRequest
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return req, false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return req, false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		http.Error(w, "one JSON object required", 400)
		return req, false
	}
	if req.RequestID == "" || len(req.RequestID) > 128 {
		http.Error(w, "request_id required (max 128 bytes)", 400)
		return req, false
	}
	return req, true
}

// A bounded receipt ledger never evicts accepted IDs silently. Restart clears it;
// an ambiguous transport failure plus timer fallback remains at-least-once.
func (s *APIServer) assistantReplay(w http.ResponseWriter, route string, req assistantRequest) (string, [32]byte, bool) {
	data, _ := json.Marshal(req)
	digest := sha256.Sum256(data)
	key := route + "\x00" + req.Project + "\x00" + req.SessionKey + "\x00" + req.RequestID
	for id, receipt := range s.assistantRequests {
		if time.Since(receipt.acceptedAt) > 48*time.Hour {
			delete(s.assistantRequests, id)
		}
	}
	if receipt, ok := s.assistantRequests[key]; ok {
		if receipt.digest != digest {
			http.Error(w, "request_id reused with different payload", 409)
		} else {
			apiJSON(w, 200, receipt.result)
		}
		return key, digest, true
	}
	if len(s.assistantRequests) >= 8192 {
		http.Error(w, "assistant receipt capacity reached; use timer fallback", 503)
		return key, digest, true
	}
	return key, digest, false
}
func (s *APIServer) assistantRemember(key string, digest [32]byte, result map[string]any) {
	if s.assistantRequests == nil {
		s.assistantRequests = make(map[string]assistantReceipt)
	}
	s.assistantRequests[key] = assistantReceipt{digest: digest, result: result, acceptedAt: time.Now()}
}
func (e *Engine) assistantTarget(key string) (Platform, any, error) {
	for _, p := range e.platforms {
		if !strings.HasPrefix(key, p.Name()+":") {
			continue
		}
		rc, ok := p.(ReplyContextReconstructor)
		if !ok {
			return nil, nil, fmt.Errorf("platform cannot reconstruct reply context")
		}
		ctx, err := rc.ReconstructReplyCtx(key)
		return p, ctx, err
	}
	return nil, nil, fmt.Errorf("unknown platform route")
}
func (e *Engine) assistantContext(p Platform, key string) (Agent, *SessionManager, string, string, error) {
	var agent Agent
	var sessions *SessionManager
	var interactiveKey, workspace string
	var err error
	if dir := e.sendWorkDirForSession(key); dir != "" {
		agent, sessions, err = e.getOrCreateWorkspaceAgent(dir)
		interactiveKey = dir + ":" + key
		workspace = dir
		if err == nil {
			agent, err = e.isolateAgent(agent, sessions, key, false)
		}
	} else {
		agent, sessions, interactiveKey, workspace, err = e.commandContextWithWorkspace(p, &Message{SessionKey: key, Platform: p.Name()})
	}
	if err != nil {
		return nil, nil, "", "", err
	}
	if sessions.ActiveSessionID(key) == "" {
		return nil, nil, "", "", fmt.Errorf("unknown session_key")
	}
	return agent, sessions, interactiveKey, workspace, nil
}
func (e *Engine) assistantBusy(key string, session *Session) bool {
	if session.Busy() {
		return true
	}
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	if st := e.interactiveStates[key]; st != nil {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.pending != nil || len(st.pendingMessages) > 0 || st.backgroundTurnLease
	}
	return false
}
func (s *APIServer) handleAssistantState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", 405)
		return
	}
	e, err := s.assistantEngine(r.URL.Query().Get("project"), r.URL.Query().Get("session_key"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !e.assistantAdmissionMu.TryLock() {
		http.Error(w, "admission busy", 409)
		return
	}
	defer e.assistantAdmissionMu.Unlock()
	key := r.URL.Query().Get("session_key")
	p, _, err := e.assistantTarget(key)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	_, sessions, iKey, _, err := e.assistantContext(p, key)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	session := sessions.FindByID(sessions.ActiveSessionID(key))
	if session == nil {
		http.Error(w, "session not found", 409)
		return
	}
	all := session.GetHistory(0)
	history := all
	if len(history) > 32 {
		history = history[len(history)-32:]
	}
	var first any
	if len(all) > 0 {
		first = all[0]
	}
	apiJSON(w, 200, map[string]any{"ok": true, "session_id": session.ID, "busy": e.assistantBusy(iKey, session), "history_count": len(all), "history": history, "first_message": first})
}
func (s *APIServer) handleAssistantInject(w http.ResponseWriter, r *http.Request) {
	req, ok := assistantDecode(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		http.Error(w, "message required", 400)
		return
	}
	if !s.assistantMu.TryLock() {
		http.Error(w, "assistant admission busy", 503)
		return
	}
	defer s.assistantMu.Unlock()
	receiptKey, digest, replayed := s.assistantReplay(w, "inject", req)
	if replayed {
		return
	}
	e, err := s.assistantEngine(req.Project, req.SessionKey)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !e.assistantAdmissionMu.TryLock() {
		http.Error(w, "admission busy; retry or use timer", 503)
		return
	}
	defer e.assistantAdmissionMu.Unlock()
	p, ctx, err := e.assistantTarget(req.SessionKey)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	agent, sessions, iKey, workspace, err := e.assistantContext(p, req.SessionKey)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	session := sessions.FindByID(sessions.ActiveSessionID(req.SessionKey))
	if session == nil {
		http.Error(w, "session not found", 409)
		return
	}
	msg := &Message{SessionKey: req.SessionKey, Platform: p.Name(), UserID: "assistant", UserName: "assistant", Content: req.Message, Source: req.Source, Urgent: req.Urgent, ReplyCtx: ctx, scheduledPrompt: true, scheduledInputID: "assistant:" + req.RequestID, admissionDeadline: time.Now().Add(250 * time.Millisecond)}
	queued, lockGen, err := e.admitScheduledPrompt(p, msg, session, sessions, agent, iKey, workspace)
	if err != nil {
		http.Error(w, "admission failed", 503)
		return
	}
	if !queued && !e.launchAcceptedWork(func() {
		e.processInteractiveMessageWith(p, msg, session, agent, sessions, iKey, workspace, req.SessionKey, lockGen)
	}) {
		session.Unlock(lockGen)
		http.Error(w, "engine stopping", 503)
		return
	}
	result := map[string]any{"ok": true, "accepted": true, "queued": queued, "request_id": req.RequestID}
	s.assistantRemember(receiptKey, digest, result)
	apiJSON(w, 200, result)
}
func validAssistantSummary(summary string) error {
	var handoff struct {
		SchemaVersion int    `json:"schema_version"`
		HandoffID     string `json:"handoff_id"`
		Unfinished    []struct {
			ID         string `json:"id"`
			Goal       string `json:"goal"`
			Status     string `json:"status"`
			NextAction string `json:"next_action"`
			Evidence   string `json:"evidence"`
			Owner      string `json:"owner"`
		} `json:"unfinished"`
		Decisions []string `json:"decisions"`
		Blockers  []string `json:"blockers"`
		Running   []struct {
			ID     string `json:"id"`
			Handle string `json:"handle"`
			Status string `json:"status"`
		} `json:"running"`
		Completed    []string `json:"completed"`
		ResumeChecks []string `json:"resume_checks"`
	}
	if err := json.Unmarshal([]byte(summary), &handoff); err != nil {
		return fmt.Errorf("summary must be JSON")
	}
	if handoff.SchemaVersion != 1 || strings.TrimSpace(handoff.HandoffID) == "" || handoff.Unfinished == nil || handoff.Decisions == nil || handoff.Blockers == nil || handoff.Running == nil || handoff.Completed == nil || handoff.ResumeChecks == nil {
		return fmt.Errorf("incomplete handoff schema")
	}
	for _, item := range handoff.Unfinished {
		if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.Goal) == "" || strings.TrimSpace(item.Status) == "" || strings.TrimSpace(item.NextAction) == "" || strings.TrimSpace(item.Owner) == "" || strings.TrimSpace(item.Evidence) == "" {
			return fmt.Errorf("incomplete unfinished item")
		}
	}
	for _, list := range [][]string{handoff.Decisions, handoff.Blockers, handoff.Completed, handoff.ResumeChecks} {
		for _, item := range list {
			if strings.TrimSpace(item) == "" {
				return fmt.Errorf("empty handoff list item")
			}
		}
	}
	for _, item := range handoff.Running {
		if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.Handle) == "" || strings.TrimSpace(item.Status) == "" {
			return fmt.Errorf("incomplete running item")
		}
	}
	return nil
}
func (s *APIServer) handleAssistantRotate(w http.ResponseWriter, r *http.Request) {
	req, ok := assistantDecode(w, r)
	if !ok {
		return
	}
	if err := validAssistantSummary(req.Summary); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !s.assistantMu.TryLock() {
		http.Error(w, "assistant admission busy", 503)
		return
	}
	defer s.assistantMu.Unlock()
	receiptKey, digest, replayed := s.assistantReplay(w, "rotate", req)
	if replayed {
		return
	}
	e, err := s.assistantEngine(req.Project, req.SessionKey)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !e.assistantAdmissionMu.TryLock() {
		http.Error(w, "admission busy", 409)
		return
	}
	defer e.assistantAdmissionMu.Unlock()
	p, ctx, err := e.assistantTarget(req.SessionKey)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	agent, sessions, iKey, workspace, err := e.assistantContext(p, req.SessionKey)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	old := sessions.FindByID(sessions.ActiveSessionID(req.SessionKey))
	if old == nil || old.ID != req.ExpectedSessionID || e.assistantBusy(iKey, old) {
		http.Error(w, "session changed or busy", 409)
		return
	}
	oldGen, locked := old.TryLock()
	if !locked {
		http.Error(w, "session changed or busy", 409)
		return
	}
	defer old.UnlockWithoutUpdate(oldGen)
	history := old.GetHistory(0)
	if len(history) != req.ExpectedHistoryCount || len(history) == 0 || history[len(history)-1].Role != "assistant" || strings.TrimSpace(history[len(history)-1].Content) != strings.TrimSpace(req.Summary) {
		http.Error(w, "handoff no longer matches completed assistant history", 409)
		return
	}
	done, accepted := e.trackWork(true)
	if !accepted {
		http.Error(w, "engine stopping", 503)
		return
	}
	// Start and confirm the first prompt on a detached candidate. No old state
	// is destroyed until both startup/Send and atomic persistence have succeeded.
	provisional := &Session{ID: "assistant-preflight", Name: "daily-handoff", ActiveProvider: old.GetActiveProvider()}
	candidateKey := iKey + "#assistant-preflight:" + req.RequestID
	provisionalManager := NewSessionManager("")
	candidate := e.getOrCreateInteractiveStateWith(candidateKey, p, ctx, provisional, provisionalManager, agent, req.SessionKey)
	candidate.mu.Lock()
	candidateAS := candidate.agentSession
	candidate.mu.Unlock()
	failCandidate := func() { e.cleanupInteractiveState(candidateKey, candidate); done() }
	if candidateAS == nil || !candidateAS.Alive() {
		failCandidate()
		http.Error(w, "new agent start failed; original session retained", 503)
		return
	}
	sendResult := make(chan error, 1)
	prompt := e.buildSenderPrompt(req.Summary, "assistant", "assistant", p.Name(), req.SessionKey, "")
	go func() { sendResult <- candidateAS.Send(prompt, "", nil, nil) }()
	select {
	case err = <-sendResult:
	case <-r.Context().Done():
		err = r.Context().Err()
	case <-time.After(5 * time.Second):
		err = fmt.Errorf("candidate prompt admission timed out")
	}
	if err != nil || !candidateAS.Alive() {
		failCandidate()
		http.Error(w, "new agent first Send failed; original session retained", 503)
		return
	}
	if id := candidateAS.CurrentSessionID(); id != "" {
		provisional.SetAgentSessionID(id, agent.Name())
	}
	fresh, freshGen, err := sessions.rotateAssistant(req.SessionKey, old, req.Summary, provisional)
	if err != nil {
		failCandidate()
		http.Error(w, "persist rotation failed; original session retained", 503)
		return
	}
	e.cleanupInteractiveState(iKey)
	e.interactiveMu.Lock()
	candidate.mu.Lock()
	candidate.queueKey = iKey
	candidate.queueSessionID = fresh.ID
	candidate.workspaceDir = workspace
	candidate.eventsNeedResync = false
	candidate.mu.Unlock()
	delete(e.interactiveStates, candidateKey)
	e.interactiveStates[iKey] = candidate
	e.interactiveMu.Unlock()
	msg := &Message{SessionKey: req.SessionKey, Platform: p.Name(), UserID: "assistant", UserName: "assistant", Content: req.Summary, ReplyCtx: ctx, scheduledPrompt: true, scheduledInputID: "rotation:" + req.RequestID, historyPreseeded: true, preflightSent: true, executionState: candidate}
	go func() {
		defer done()
		e.processInteractiveMessageWith(p, msg, fresh, agent, sessions, iKey, workspace, req.SessionKey, freshGen)
	}()
	result := map[string]any{"ok": true, "session_id": fresh.ID, "previous_session_id": old.ID, "request_id": req.RequestID}
	s.assistantRemember(receiptKey, digest, result)
	apiJSON(w, 200, result)
}

// Persist summary and activation together; rollback all routing changes if the
// atomic snapshot write fails. The old session remains registered and resumable.
func (sm *SessionManager) rotateAssistant(key string, old *Session, summary string, provisional *Session) (*Session, uint64, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.activeSession[key] != old.ID {
		return nil, 0, fmt.Errorf("active session changed")
	}
	counter := sm.counter
	ids := append([]string(nil), sm.userSessions[key]...)
	fresh := sm.createLocked(key, "daily-handoff-"+time.Now().UTC().Format("20060102"))
	fresh.ActiveProvider = old.GetActiveProvider()
	fresh.AgentSessionID = provisional.GetAgentSessionID()
	fresh.AgentType = provisional.AgentType
	// fresh is not reachable by other goroutines until this returns, so the
	// busy lock is taken directly; its generation is returned to the caller.
	fresh.busy = true
	fresh.busySince = time.Now()
	fresh.lockGen++
	gen := fresh.lockGen
	fresh.History = []HistoryEntry{{Role: "user", Content: summary, Timestamp: time.Now()}}
	oldAgentID := old.GetAgentSessionID()
	if err := sm.saveLocked(); err != nil {
		old.mu.Lock()
		old.AgentSessionID = oldAgentID
		old.mu.Unlock()
		delete(sm.sessions, fresh.ID)
		sm.activeSession[key] = old.ID
		sm.userSessions[key] = ids
		sm.counter = counter
		return nil, 0, err
	}
	return fresh, gen, nil
}
