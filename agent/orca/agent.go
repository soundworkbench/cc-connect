package orca

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func init() { core.RegisterAgent("orca", New) }

const (
	maxConsecutiveReadErrors = 5
	doneStableConfirmPolls   = 2
	homeChatResolveRetries   = 5
)

// Agent adapts an Orca Bot (accessed via the official `orca` CLI) to
// cc-connect's unified core.Agent interface.
type Agent struct {
	botID          string
	orcaBin        string
	orcaHome       string
	senderID       string
	dataDir        string
	pollInterval   time.Duration
	turnTimeout    time.Duration
	workPollEvery  time.Duration
	itemPollEvery  time.Duration
	readLimit      int
	syncNeedsYou   bool
	runnerOverride cliRunner // nil in production; set in tests

	mu       sync.Mutex
	sessions map[*Session]struct{}
}

// New constructs an Orca Bot agent from cc-connect options.
func New(opts map[string]any) (core.Agent, error) {
	getStr := func(k string) string {
		v, _ := opts[k].(string)
		return strings.TrimSpace(v)
	}
	getInt := func(k string, def int) int {
		v, ok := opts[k]
		if !ok || v == nil {
			return def
		}
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		case string:
			if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				return parsed
			}
		}
		return def
	}
	getBool := func(k string, def bool) bool {
		v, ok := opts[k]
		if !ok || v == nil {
			return def
		}
		if b, ok := v.(bool); ok {
			return b
		}
		return def
	}

	botID := getStr("bot_id")
	if botID == "" {
		return nil, errors.New("orca: bot_id is required")
	}

	orcaHome := getStr("orca_home")
	if orcaHome == "" {
		orcaHome = defaultOrcaHome
	}
	orcaBin := getStr("orca_bin")
	if orcaBin == "" {
		orcaBin = resolveDefaultOrcaBin(orcaHome)
	}
	senderID := getStr("sender_id")
	if senderID == "" {
		senderID = defaultSenderID
	}
	dataDir := getStr("cc_data_dir")
	if dataDir == "" {
		dataDir = ".cc-connect"
	}

	pollMs := getInt("poll_interval_ms", defaultPollIntervalMs)
	if pollMs <= 0 {
		pollMs = defaultPollIntervalMs
	}
	turnTimeoutSec := getInt("turn_timeout_sec", defaultTurnTimeoutSec)
	if turnTimeoutSec <= 0 {
		turnTimeoutSec = defaultTurnTimeoutSec
	}
	workPollMs := getInt("work_poll_interval_ms", defaultWorkPollMs)
	if workPollMs <= 0 {
		workPollMs = defaultWorkPollMs
	}
	itemPollMs := getInt("item_poll_interval_ms", defaultItemPollMs)
	if itemPollMs <= 0 {
		itemPollMs = defaultItemPollMs
	}
	readLimit := getInt("read_limit", defaultReadLimit)
	if readLimit <= 0 || readLimit > 100 {
		readLimit = defaultReadLimit
	}

	return &Agent{
		botID:         botID,
		orcaBin:       orcaBin,
		orcaHome:      orcaHome,
		senderID:      senderID,
		dataDir:       dataDir,
		pollInterval:  time.Duration(pollMs) * time.Millisecond,
		turnTimeout:   time.Duration(turnTimeoutSec) * time.Second,
		workPollEvery: time.Duration(workPollMs) * time.Millisecond,
		itemPollEvery: time.Duration(itemPollMs) * time.Millisecond,
		readLimit:     readLimit,
		syncNeedsYou:  getBool("sync_needs_you", true),
		sessions:      make(map[*Session]struct{}),
	}, nil
}

func (a *Agent) client() *orcaClient {
	if a.runnerOverride != nil {
		return &orcaClient{runner: a.runnerOverride}
	}
	return &orcaClient{
		runner: &execRunner{
			bin:        a.orcaBin,
			home:       a.orcaHome,
			cliTimeout: defaultCLITimeout,
		},
	}
}

func (a *Agent) Name() string           { return "orca" }
func (a *Agent) CLIBinaryName() string  { return a.orcaBin }
func (a *Agent) CLIDisplayName() string { return "Orca Bot" }

func (a *Agent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	chats, err := a.client().listChats(ctx, a.botID)
	if err != nil {
		return nil, err
	}
	out := make([]core.AgentSessionInfo, 0, len(chats))
	for _, ch := range chats {
		if ch.Closed || strings.TrimSpace(ch.ID) == "" {
			continue
		}
		summary := strings.TrimSpace(ch.Title)
		if summary == "" {
			summary = strings.TrimSpace(ch.LastAsked)
		}
		if summary == "" && ch.Home {
			summary = "Home"
		}
		var modified time.Time
		if ch.LastActivityAt > 0 {
			modified = time.UnixMilli(ch.LastActivityAt)
		}
		out = append(out, core.AgentSessionInfo{
			ID:         ch.ID,
			Summary:    oneLineClip(summary, 60),
			ModifiedAt: modified,
		})
	}
	return out, nil
}

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	s := newSession(ctx, a, strings.TrimSpace(sessionID))
	if s.CurrentSessionID() == "" {
		if homeID, err := s.client.findHomeChat(ctx, a.botID); err == nil && homeID != "" {
			s.chatID.Store(homeID)
		} else if err != nil {
			slog.Warn("orca: initial home chat lookup failed; will retry on send", "bot", a.botID, "error", err)
		}
	}

	// Establish initial baselines so restarts never replay historical tasks
	// or already-open needs-you items.
	s.workers.baseline(ctx, s.client, a.botID)
	if a.syncNeedsYou {
		s.items.baseline(ctx, s.client, a.botID)
	}

	s.startBackgroundLoop()

	a.mu.Lock()
	a.sessions[s] = struct{}{}
	a.mu.Unlock()
	return s, nil
}

func (a *Agent) Stop() error {
	a.mu.Lock()
	list := make([]*Session, 0, len(a.sessions))
	for s := range a.sessions {
		list = append(list, s)
	}
	a.sessions = make(map[*Session]struct{})
	a.mu.Unlock()
	for _, s := range list {
		_ = s.Close()
	}
	return nil
}

func (a *Agent) forgetSession(s *Session) {
	a.mu.Lock()
	delete(a.sessions, s)
	a.mu.Unlock()
}

var (
	_ core.Agent                   = (*Agent)(nil)
	_ core.AgentDoctorInfo         = (*Agent)(nil)
	_ core.AgentSession            = (*Session)(nil)
	_ core.AgentSessionCanceller   = (*Session)(nil)
	_ core.PermissionApprovalAll   = (*Session)(nil)
	_ core.BackgroundWorkReporter  = (*Session)(nil)
	_ core.RunningTasksReporter    = (*Session)(nil)
)

// Session manages one Orca Bot chat session bridged into cc-connect.
type Session struct {
	agent  *Agent
	client *orcaClient

	ctx    context.Context
	cancel context.CancelFunc

	chatID   atomic.Value // string
	events   chan core.Event
	evMu     sync.RWMutex
	evClosed bool

	closeMu   sync.Mutex
	closed    atomic.Bool
	closeOnce sync.Once
	wg        sync.WaitGroup

	turnMu     sync.Mutex
	turnActive bool
	turnCancel context.CancelFunc
	turnSeq    atomic.Uint64
	toolSeq    atomic.Uint64
	hasSent    atomic.Bool

	workers *workerTracker
	items   *itemTracker
}

func newSession(parent context.Context, a *Agent, initialChatID string) *Session {
	ctx, cancel := context.WithCancel(parent)
	s := &Session{
		agent:   a,
		client:  a.client(),
		ctx:     ctx,
		cancel:  cancel,
		events:  make(chan core.Event, 256),
		workers: newWorkerTracker(),
		items:   newItemTracker(),
	}
	if initialChatID != "" {
		s.chatID.Store(initialChatID)
	}
	return s
}

func (s *Session) startBackgroundLoop() {
	s.closeMu.Lock()
	if s.closed.Load() {
		s.closeMu.Unlock()
		return
	}
	s.wg.Add(1)
	s.closeMu.Unlock()

	go func() {
		defer s.wg.Done()
		s.backgroundLoop()
	}()

	// Dedicated closer ensures s.events is closed strictly after every
	// producer goroutine finishes, preventing send-on-closed-channel panics.
	go func() {
		<-s.ctx.Done()
		s.wg.Wait()
		s.closed.Store(true)
		s.evMu.Lock()
		if !s.evClosed {
			s.evClosed = true
			close(s.events)
		}
		s.evMu.Unlock()
	}()
}

func (s *Session) CurrentSessionID() string {
	if v := s.chatID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

func (s *Session) Events() <-chan core.Event        { return s.events }
func (s *Session) Alive() bool                      { return !s.closed.Load() && s.ctx.Err() == nil }
func (s *Session) SupportsPermissionApprovalAll() bool { return false }
func (s *Session) RunningTasks() int                { return s.workers.RunningCount() }
func (s *Session) HasBackgroundWork() bool          { return s.Alive() && s.workers.RunningCount() > 0 }

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

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		s.closed.Store(true)
		s.closeMu.Unlock()

		s.turnMu.Lock()
		if s.turnCancel != nil {
			s.turnCancel()
			s.turnCancel = nil
		}
		s.turnActive = false
		s.turnMu.Unlock()

		s.cancel()
		s.agent.forgetSession(s)
	})
	return nil
}

// CancelTurn stops polling the active foreground turn cleanly while keeping
// the session alive for subsequent messages. Note: Orca CLI does not expose a
// remote turn-interrupt command for a bot chat, so cancellation is local to
// the bridge turn poller.
func (s *Session) CancelTurn() error {
	s.turnMu.Lock()
	cancel := s.turnCancel
	s.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Send delivers a user message (plus optional images/files) to the Orca bot
// and starts polling the bot's chat transcript until the turn settles.
func (s *Session) Send(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !s.Alive() {
		return errors.New("orca: session closed")
	}

	prompt, saved := prepareAttachments(s.agent.dataDir, messageID, prompt, images, files)
	body := strings.TrimSpace(prompt)
	if body == "" && !saved.Empty() {
		body = "请查看附件。"
	}

	req := sendMessageReq{
		BotID:   s.agent.botID,
		From:    s.agent.senderID,
		Type:    "question",
		Subject: buildSubject(body, messageID),
		Body:    body,
	}

	// Route cc-connect decision card replies directly to their Orca Item.
	// Only item-scoped sends may pass --file; ordinary messages embed saved
	// local paths directly in the message body so the bot reads them on disk.
	targetChatID := s.CurrentSessionID()
	if route, ok := s.items.matchDecisionPrompt(body); ok && route.Number > 0 {
		req.ItemNumber = route.Number
		req.Type = "handoff"
		req.Subject = fmt.Sprintf("飞书卡片决策 #%d", route.Number)
		req.Files = saved.AllPaths()
		if route.ChatID != "" {
			targetChatID = route.ChatID
		}
	} else if !saved.Empty() {
		req.Body = saved.appendRefsToBody(req.Body)
	}

	// Capture baseline BEFORE sending so previous turns are never replayed.
	var baseline []orcaEntry
	if targetChatID == "" {
		if homeID, err := s.client.findHomeChat(s.ctx, s.agent.botID); err == nil && homeID != "" {
			targetChatID = homeID
			s.chatID.Store(homeID)
		}
	}
	if targetChatID != "" {
		if snap, err := s.client.readSession(s.ctx, targetChatID, s.agent.readLimit); err == nil && snap != nil {
			baseline = snap.Entries
		} else if err != nil {
			slog.Warn("orca: baseline read failed before send", "chat", targetChatID, "error", err)
		}
	}

	// Deliver message via official Orca CLI (no manual feishu-reply hints!).
	if err := s.client.sendMessage(s.ctx, req); err != nil {
		return err
	}
	s.hasSent.Store(true)

	// If no Home Chat existed before the first message, look it up now.
	if targetChatID == "" {
		targetChatID = s.resolveHomeChatAfterSend()
		if targetChatID != "" {
			s.chatID.Store(targetChatID)
		}
	}

	s.closeMu.Lock()
	if s.closed.Load() {
		s.closeMu.Unlock()
		return errors.New("orca: session closed")
	}

	// Cancel any stale turn poller if still winding down.
	s.turnMu.Lock()
	if s.turnCancel != nil {
		s.turnCancel()
	}
	turnCtx, turnCancel := context.WithTimeout(s.ctx, s.agent.turnTimeout)
	s.turnCancel = turnCancel
	s.turnActive = true
	turnID := s.turnSeq.Add(1)
	s.turnMu.Unlock()

	s.wg.Add(1)
	s.closeMu.Unlock()

	go func() {
		defer s.wg.Done()
		defer turnCancel()
		defer func() {
			s.turnMu.Lock()
			if s.turnSeq.Load() == turnID {
				s.turnActive = false
				s.turnCancel = nil
			}
			s.turnMu.Unlock()
		}()
		s.pollTurn(turnCtx, targetChatID, baseline)
	}()

	return nil
}

func (s *Session) resolveHomeChatAfterSend() string {
	for i := 0; i < homeChatResolveRetries; i++ {
		if homeID, err := s.client.findHomeChat(s.ctx, s.agent.botID); err == nil && homeID != "" {
			return homeID
		}
		if i+1 < homeChatResolveRetries {
			select {
			case <-time.After(s.agent.pollInterval):
			case <-s.ctx.Done():
				return ""
			}
		}
	}
	return ""
}

// pollTurn watches the Orca chat transcript after Send(), emitting:
//   - EventText for each new assistant message (or in-place growth delta)
//   - EventToolUse with Chinese StepList metadata for each new tool entry
//   - EventTask for background worker lifecycle transitions
//   - EventResult{Done: true} once state settles at "done"
//   - EventError on timeout or sustained CLI failure
func (s *Session) pollTurn(turnCtx context.Context, chatID string, baseline []orcaEntry) {
	if chatID == "" {
		chatID = s.resolveHomeChatAfterSend()
		if chatID == "" {
			if s.ctx.Err() == nil {
				s.emit(core.Event{
					Type:  core.EventError,
					Error: errors.New("orca: could not locate bot Home Chat after sending message"),
				})
			}
			return
		}
		s.chatID.Store(chatID)
	}

	prev := baseline
	var (
		sawWorking        bool
		sawNewActivity    bool
		sawAssistantMsg   bool
		segmentText       string
		lastAssistantText string
		doneStablePolls   int
		consecutiveErrs   int
		lastWorkSync      time.Time
	)

	ticker := time.NewTicker(s.agent.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-turnCtx.Done():
			if s.ctx.Err() != nil {
				return
			}
			if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
				s.emit(core.Event{
					Type:  core.EventError,
					Error: fmt.Errorf("orca: turn timed out after %v", s.agent.turnTimeout),
				})
				return
			}
			// Cancelled via CancelTurn (/stop): end cleanly without error.
			s.emit(core.Event{
				Type:      core.EventResult,
				Content:   finalReplyText(segmentText, lastAssistantText),
				SessionID: chatID,
				Done:      true,
			})
			return
		case <-ticker.C:
		}

		// Periodically sync background workers during long turns.
		if time.Since(lastWorkSync) >= s.agent.workPollEvery {
			lastWorkSync = time.Now()
			s.workers.sync(turnCtx, s.client, s.agent.botID, s.emit)
		}

		snap, err := s.client.readSession(turnCtx, chatID, s.agent.readLimit)
		if err != nil {
			if turnCtx.Err() != nil {
				continue
			}
			consecutiveErrs++
			slog.Warn("orca: agent read failed", "chat", chatID, "attempt", consecutiveErrs, "error", err)
			if consecutiveErrs >= maxConsecutiveReadErrors {
				s.emit(core.Event{
					Type:  core.EventError,
					Error: fmt.Errorf("orca: agent read failed %d consecutive times: %w", consecutiveErrs, err),
				})
				return
			}
			continue
		}
		consecutiveErrs = 0

		deltaText, updatedFull, added := diffEntries(prev, snap.Entries)
		hadNew := deltaText != "" || len(added) > 0
		if hadNew {
			prev = snap.Entries
			doneStablePolls = 0
			sawNewActivity = true
		}

		if deltaText != "" {
			sawAssistantMsg = true
			segmentText = updatedFull
			lastAssistantText = updatedFull
			s.emit(core.Event{
				Type:      core.EventText,
				Content:   deltaText,
				SessionID: chatID,
			})
		}

		for _, entry := range added {
			switch entry.Kind {
			case "message":
				if entry.Role == "assistant" && entry.Text != "" {
					sawAssistantMsg = true
					if segmentText == "" {
						segmentText = entry.Text
					} else {
						segmentText += "\n\n" + entry.Text
					}
					lastAssistantText = segmentText
					s.emit(core.Event{
						Type:      core.EventText,
						Content:   entry.Text,
						SessionID: chatID,
					})
				}
			case "tool":
				segmentText = ""
				pid := fmt.Sprintf("orca-tool-%d", s.toolSeq.Add(1))
				s.emit(toolStepEvent(pid, entry.Name, entry.State))
			}
		}

		state := strings.ToLower(strings.TrimSpace(snap.State))
		if isWorkingState(state) {
			sawWorking = true
			doneStablePolls = 0
			continue
		}

		if isDoneState(state) && (sawAssistantMsg || sawWorking) {
			if !hadNew {
				doneStablePolls++
			}
			// Require either two consecutive stable "done" polls with no new
			// entries, or one stable poll if we already saw working -> done
			// with an assistant reply.
			needed := doneStableConfirmPolls
			if sawWorking && sawAssistantMsg {
				needed = 1
			}
			if doneStablePolls >= needed {
				// One final worker sync before settling the turn.
				s.workers.sync(turnCtx, s.client, s.agent.botID, s.emit)
				s.emit(core.Event{
					Type:      core.EventResult,
					Content:   finalReplyText(segmentText, lastAssistantText),
					SessionID: chatID,
					Done:      true,
				})
				// Check for any newly opened Needs-You items right after the
				// turn settles; cc-connect's unsolicitedReader picks them up
				// and renders native Feishu decision cards without blocking.
				if s.agent.syncNeedsYou {
					s.items.poll(s.ctx, s.client, s.agent.botID, s.emit)
				}
				return
			}
		} else if sawNewActivity && state == "" {
			// Unknown state with activity: keep waiting until done or timeout.
			doneStablePolls = 0
		}
	}
}

func (s *Session) backgroundLoop() {
	tickEvery := s.agent.workPollEvery
	if s.agent.syncNeedsYou && s.agent.itemPollEvery < tickEvery {
		tickEvery = s.agent.itemPollEvery
	}
	if tickEvery <= 0 {
		tickEvery = defaultWorkPollMs * time.Millisecond
	}

	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()

	var lastWork, lastItem time.Time
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			if !s.hasSent.Load() {
				continue
			}
			s.turnMu.Lock()
			busy := s.turnActive
			s.turnMu.Unlock()
			if busy {
				continue
			}
			if now.Sub(lastWork) >= s.agent.workPollEvery {
				lastWork = now
				s.workers.sync(s.ctx, s.client, s.agent.botID, s.emit)
			}
			if s.agent.syncNeedsYou && now.Sub(lastItem) >= s.agent.itemPollEvery {
				lastItem = now
				s.items.poll(s.ctx, s.client, s.agent.botID, s.emit)
			}
		}
	}
}

// RespondPermission handles answers for Orca `needs_you` items when cc-connect
// falls back to the blocking AskUserQuestion prompt (i.e. [decision] disabled
// or single-option questions). When [decision] is enabled, cc-connect calls
// RespondPermission with Behavior="deny" immediately after posting a
// non-blocking decision card, which we safely ignore because the user's pick
// will arrive through Send().
func (s *Session) RespondPermission(requestID string, result core.PermissionResult) error {
	route, ok := s.items.consumePending(requestID)
	if !ok {
		return nil
	}
	if !strings.EqualFold(result.Behavior, "allow") {
		return nil
	}

	choice := formatPermissionAnswers(result)
	if choice == "" {
		return nil
	}
	body := fmt.Sprintf("[飞书决策卡回传]\nItem: #%d\n用户选择/回复：%s\n请按此决定继续推进，并更新 Item #%d 的状态与评论。", route.Number, choice, route.Number)
	return s.client.sendMessage(s.ctx, sendMessageReq{
		BotID:      s.agent.botID,
		From:       s.agent.senderID,
		Type:       "handoff",
		Subject:    fmt.Sprintf("飞书卡片决策 #%d", route.Number),
		Body:       body,
		ItemNumber: route.Number,
	})
}

func formatPermissionAnswers(result core.PermissionResult) string {
	answers, _ := result.UpdatedInput["answers"].(map[string]any)
	if len(answers) == 0 {
		return strings.TrimSpace(result.Message)
	}
	if len(answers) == 1 {
		for _, v := range answers {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	keys := make([]string, 0, len(answers))
	for k := range answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if s, ok := answers[k].(string); ok && strings.TrimSpace(s) != "" {
			parts = append(parts, fmt.Sprintf("%s：%s", k, strings.TrimSpace(s)))
		}
	}
	return strings.Join(parts, "\n")
}

func isWorkingState(state string) bool {
	switch state {
	case "working", "running", "in_progress", "thinking", "streaming":
		return true
	}
	return false
}

func isDoneState(state string) bool {
	switch state {
	case "done", "idle", "completed":
		return true
	}
	return false
}

func finalReplyText(segmentText, lastAssistantText string) string {
	if strings.TrimSpace(segmentText) != "" {
		return segmentText
	}
	return lastAssistantText
}

func buildSubject(prompt, messageID string) string {
	firstLine := strings.TrimSpace(strings.SplitN(prompt, "\n", 2)[0])
	if firstLine != "" {
		return oneLineClip(firstLine, 40)
	}
	if id := strings.TrimSpace(messageID); id != "" {
		if len(id) > 6 {
			id = id[len(id)-6:]
		}
		return "飞书消息 " + id
	}
	return "飞书消息"
}
