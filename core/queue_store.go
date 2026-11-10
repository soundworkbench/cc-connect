package core

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

type queueRecord struct {
	Source            string            `json:"source,omitempty"`
	Urgent            bool              `json:"urgent,omitempty"`
	QueuedAt          time.Time         `json:"queued_at,omitempty"`
	SeparateReply     bool              `json:"separate_reply,omitempty"`
	Project           string            `json:"project"`
	Key               string            `json:"key"`
	SessionID         string            `json:"session_id"`
	Workspace         string            `json:"workspace,omitempty"`
	Platform          string            `json:"platform"`
	ReplySessionKey   string            `json:"reply_session_key"`
	Muted             bool              `json:"muted,omitempty"`
	Scheduled         bool              `json:"scheduled,omitempty"`
	InputID           string            `json:"input_id,omitempty"`
	MessageID         string            `json:"message_id,omitempty"`
	SessionKey        string            `json:"session_key"`
	Content           string            `json:"content"`
	Images            []ImageAttachment `json:"images,omitempty"`
	Files             []FileAttachment  `json:"files,omitempty"`
	Mode              string            `json:"mode,omitempty"`
	FromVoice         bool              `json:"from_voice,omitempty"`
	UserID            string            `json:"user_id,omitempty"`
	UserName          string            `json:"user_name,omitempty"`
	ChannelKey        string            `json:"channel_key,omitempty"`
	UserMessageTimeMs int64             `json:"user_message_time_ms,omitempty"`
}

func queueFileName(project, key string) string {
	return fmt.Sprintf("%x_%x.jsonl", sha256.Sum256([]byte(project)), sha256.Sum256([]byte(key)))
}

func (q queuedMessage) record(project string, state *interactiveState) queueRecord {
	replyKey := q.replySessionKey
	if replyKey == "" {
		replyKey = q.msgSessionKey
	}
	return queueRecord{Source: q.source, Urgent: q.urgent, QueuedAt: q.queuedAt, SeparateReply: q.separateReply, Project: project, Key: state.queueKey, SessionID: state.queueSessionID,
		Workspace: state.workspaceDir, Platform: q.platform.Name(), ReplySessionKey: replyKey,
		Muted: q.scheduledMute, Scheduled: q.scheduledPrompt, InputID: q.scheduledInputID,
		MessageID: q.messageID, SessionKey: q.msgSessionKey, Content: q.content,
		Images: q.images, Files: q.files, Mode: q.modeOverride, FromVoice: q.fromVoice,
		UserID: q.userID, UserName: q.userName, ChannelKey: q.channelKey, UserMessageTimeMs: q.userMessageTimeMs}
}

func (r queueRecord) queued() queuedMessage {
	return queuedMessage{source: r.Source, urgent: r.Urgent, queuedAt: r.QueuedAt, separateReply: r.SeparateReply, scheduledPrompt: r.Scheduled, scheduledMute: r.Muted, scheduledInputID: r.InputID,
		messageID: r.MessageID, msgPlatform: r.Platform, msgSessionKey: r.SessionKey,
		replySessionKey: r.ReplySessionKey, content: r.Content, images: r.Images, files: r.Files,
		modeOverride: r.Mode, fromVoice: r.FromVoice, userID: r.UserID, userName: r.UserName,
		channelKey: r.ChannelKey, userMessageTimeMs: r.UserMessageTimeMs, restored: &r}
}

// state.mu owns both the in-memory FIFO and its durable snapshot. Retired states
// cannot overwrite a snapshot after ownership passes to a replacement process.
func (e *Engine) persistQueueLocked(state *interactiveState) error {
	if e.dataDir == "" || state.queueKey == "" || state.queueRetired {
		return nil
	}
	dir := filepath.Join(e.dataDir, "queues")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("queue: create directory: %w", err)
	}
	path := filepath.Join(dir, queueFileName(e.name, state.queueKey))
	if len(state.pendingMessages) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("queue: remove snapshot: %w", err)
		}
		return syncQueueDir(dir)
	}
	f, err := os.CreateTemp(dir, ".queue-*")
	if err != nil {
		return fmt.Errorf("queue: create snapshot: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, q := range state.pendingMessages {
		var record queueRecord
		if q.restored != nil {
			record = *q.restored
		} else {
			record = q.record(e.name, state)
		}
		if err := enc.Encode(record); err != nil {
			return fmt.Errorf("queue: encode snapshot: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("queue: sync snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("queue: close snapshot: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("queue: replace snapshot: %w", err)
	}
	return syncQueueDir(dir)
}

func syncQueueDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("queue: open directory: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("queue: sync directory: %w", err)
	}
	return nil
}

func readQueueFile(path, project string) ([]queueRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("queue: open snapshot: %w", err)
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var records []queueRecord
	for line := 1; ; line++ {
		data, err := reader.ReadBytes('\n')
		if len(data) > 0 {
			var r queueRecord
			if decodeErr := json.Unmarshal(data, &r); decodeErr != nil || r.Project != project || r.Key == "" || r.SessionKey == "" || r.Platform == "" || filepath.Base(path) != queueFileName(r.Project, r.Key) {
				slog.Warn("queue: skipping corrupt line", "path", path, "line", line)
			} else {
				records = append(records, r)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("queue: read snapshot: %w", err)
		}
	}
	return records, nil
}

func (e *Engine) loadQueues() error {
	if e.dataDir == "" {
		return nil
	}
	pattern := filepath.Join(e.dataDir, "queues", fmt.Sprintf("%x_*.jsonl", sha256.Sum256([]byte(e.name))))
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("queue: list snapshots: %w", err)
	}
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	for _, path := range paths {
		records, err := readQueueFile(path, e.name)
		if err != nil {
			return err
		}
		for _, r := range records {
			state := e.interactiveStates[r.Key]
			if state == nil {
				state = &interactiveState{queueKey: r.Key, queueSessionID: r.SessionID, workspaceDir: r.Workspace, eventsNeedResync: true}
				e.interactiveStates[r.Key] = state
			}
			state.pendingMessages = append(state.pendingMessages, r.queued())
		}
	}
	return nil
}

func (e *Engine) isShuttingDown() bool {
	e.platformLifecycleMu.Lock()
	defer e.platformLifecycleMu.Unlock()
	return e.stopping || e.ctx.Err() != nil
}

// Beginning a queued execution and shutdown are serialized. The input leaves
// disk before dispatch so an interrupted in-flight turn is never replayed.
func (e *Engine) popQueuedMessageLocked(state *interactiveState) (queuedMessage, bool) {
	return e.popQueuedLocked(state, false)
}
func (e *Engine) popQueuedBatchLocked(state *interactiveState) (queuedMessage, bool) {
	return e.popQueuedLocked(state, true)
}
func (e *Engine) popQueuedLocked(state *interactiveState, batch bool) (queuedMessage, bool) {
	e.platformLifecycleMu.Lock()
	defer e.platformLifecycleMu.Unlock()
	if e.stopping || e.ctx.Err() != nil || state.stopped || state.queueRetired || len(state.pendingMessages) == 0 || state.pendingMessages[0].platform == nil {
		return queuedMessage{}, false
	}
	if state.queueReady != nil {
		select {
		case <-state.queueReady:
		default:
			return queuedMessage{}, false
		}
	}
	old := state.pendingMessages
	n := 1
	if batch {
		n = e.queuedBatchSize(old)
	}
	q := e.mergeQueuedMessages(old[:n])
	state.pendingMessages = old[n:]
	if err := e.persistQueueLocked(state); err != nil {
		state.pendingMessages = old
		slog.Error("queue: cannot begin execution", "session", state.queueKey, "error", err)
		return queuedMessage{}, false
	}
	if n > 1 {
		slog.Info("processing queued messages batched", "session", state.queueKey, "count", n, "chars", utf8.RuneCountInString(q.content))
	}
	return q, true
}

func (e *Engine) flushQueues() error {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	var errs []error
	for _, state := range e.interactiveStates {
		state.mu.Lock()
		err := e.persistQueueLocked(state)
		state.mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func queuedInput(q queuedMessage) *Message {
	return &Message{Source: q.source, Urgent: q.urgent, individualReply: q.separateReply, scheduledPrompt: q.scheduledPrompt, scheduledMute: q.scheduledMute, scheduledInputID: q.scheduledInputID,
		replySessionKey: q.replySessionKey, Platform: q.msgPlatform, SessionKey: q.msgSessionKey,
		MessageID: q.messageID, Content: q.content, ReplyCtx: q.replyCtx, Images: q.images,
		Files: q.files, FromVoice: q.fromVoice, UserID: q.userID, UserName: q.userName,
		ChannelKey: q.channelKey, ModeOverride: q.modeOverride, UserMessageTimeMs: q.userMessageTimeMs}
}

func (e *Engine) resumeQueuesForPlatform(p Platform) {
	e.interactiveMu.Lock()
	var keys []string
	for key, state := range e.interactiveStates {
		state.mu.Lock()
		for i := range state.pendingMessages {
			q := &state.pendingMessages[i]
			if q.restored == nil || q.platform != nil || q.msgPlatform != p.Name() {
				continue
			}
			rc, ok := p.(ReplyContextReconstructor)
			if !ok {
				slog.Warn("queue: platform cannot restore reply route", "platform", p.Name())
				continue
			}
			replyCtx, err := rc.ReconstructReplyCtx(q.replySessionKey)
			if err != nil {
				slog.Warn("queue: restore reply route failed", "session", key, "error", err)
				continue
			}
			q.platform, q.replyCtx = p, replyCtx
			q.scheduledMute = q.restored.Muted
		}
		if len(state.pendingMessages) > 0 && state.pendingMessages[0].platform != nil {
			keys = append(keys, key)
		}
		state.mu.Unlock()
	}
	e.interactiveMu.Unlock()
	for _, key := range keys {
		e.resumeQueue(key)
	}
}

func (e *Engine) resumeQueue(key string) {
	e.launchAcceptedWork(func() {
		e.interactiveMu.Lock()
		state := e.interactiveStates[key]
		if state == nil {
			e.interactiveMu.Unlock()
			return
		}
		state.mu.Lock()
		if len(state.pendingMessages) == 0 {
			state.mu.Unlock()
			e.interactiveMu.Unlock()
			return
		}
		q := state.pendingMessages[0]
		workspace, sessionID := state.workspaceDir, state.queueSessionID
		state.mu.Unlock()
		e.interactiveMu.Unlock()
		agent, sessions := e.agent, e.sessions
		if workspace != "" {
			var err error
			agent, sessions, err = e.getOrCreateWorkspaceAgent(workspace)
			if err != nil {
				slog.Error("queue: restore workspace failed", "session", key, "error", err)
				return
			}
		}
		var err error
		agent, err = e.isolateAgent(agent, sessions, q.msgSessionKey, false)
		if err != nil {
			slog.Error("queue: restore agent failed", "session", key, "error", err)
			return
		}
		if sessionID != "" && sessions.ActiveSessionID(q.msgSessionKey) != sessionID {
			e.notifyDroppedQueuedMessages(state, fmt.Errorf("session reset"))
			return
		}
		session := sessions.GetOrCreateActive(q.msgSessionKey)
		for !e.isShuttingDown() {
			if gen, ok := session.TryLock(); ok {
				e.drainOrphanedQueue(session, sessions, key, agent, workspace, gen)
				return
			}
			select {
			case <-e.ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
}

func (e *Engine) stopInteractiveSessionPreservingQueue(key string) bool {
	e.interactiveMu.Lock()
	state := e.interactiveStates[key]
	if state == nil {
		e.interactiveMu.Unlock()
		return false
	}
	e.stopUnsolicitedReader(state)
	state.mu.Lock()
	as, pending, preview := state.agentSession, state.pending, state.activePreview
	closePlatform, closeReplyCtx := state.platform, state.replyCtx
	if as == nil && state.queueReady != nil {
		state.mu.Unlock()
		e.interactiveMu.Unlock()
		return true
	}
	ready := make(chan struct{})
	state.pending = nil
	if len(state.pendingMessages) > 0 {
		next := &interactiveState{queueKey: key, queueSessionID: state.queueSessionID, queueReady: ready,
			workspaceDir: state.workspaceDir, platform: state.platform, replyCtx: state.replyCtx,
			pendingMessages: state.pendingMessages, eventsNeedResync: true}
		e.interactiveStates[key] = next
	} else {
		delete(e.interactiveStates, key)
	}
	state.pendingMessages = nil
	state.queueRetired = true
	state.mu.Unlock()
	state.markStopped()
	e.interactiveMu.Unlock()
	if preview != nil {
		preview.discard()
	}
	if pending != nil {
		pending.resolve()
	}
	go func() {
		e.closeAgentSessionWithTimeout(key, as, closePlatform, closeReplyCtx)
		close(ready)
		e.resumeQueue(key)
	}()
	e.hooks.Emit(HookEvent{Event: HookEventSessionEnded, SessionKey: key})
	return true
}

func (e *Engine) setQueuedWorkspace(key, workspace string) {
	if workspace == "" {
		return
	}
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	if state := e.interactiveStates[key]; state != nil {
		state.mu.Lock()
		state.workspaceDir = workspace
		state.mu.Unlock()
	}
}

func (e *Engine) discardStaleQueuedMessagesLocked(state *interactiveState) (int, error) {
	e.platformLifecycleMu.Lock()
	defer e.platformLifecycleMu.Unlock()
	if e.stopping || state.queueRetired || state.stopped {
		return 0, nil
	}
	old := state.pendingMessages
	count := 0
	for count < len(old) && e.isQueuedUserMessageStaleForDrainLocked(state, old[count].userMessageTimeMs) {
		count++
	}
	if count == 0 {
		return 0, nil
	}
	state.pendingMessages = old[count:]
	if err := e.persistQueueLocked(state); err != nil {
		state.pendingMessages = old
		return 0, err
	}
	return count, nil
}
