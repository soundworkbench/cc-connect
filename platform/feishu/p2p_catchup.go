package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	p2pCatchupWindow     = 10 * time.Minute
	p2pDedupRetention    = 11 * time.Minute
	p2pMaxMessageIDs     = 10000
	p2pHeartbeatInterval = 15 * time.Second
)

type p2pCatchupSnapshot struct {
	Heartbeat time.Time            `json:"heartbeat"`
	Processed map[string]time.Time `json:"processed"`
	// Only event-confirmed p2p chats are eligible. Old session keys alone cannot
	// distinguish private chats from groups, so never infer chat type from a key.
	PrivateChats map[string]time.Time `json:"private_chats"`
}

type p2pCatchup struct {
	bufferMu     sync.Mutex
	recovering   bool
	pending      []*larkim.P2MessageReceiveV1
	mu           sync.Mutex
	path         string
	sessionStore string
	project      string
	snapshot     p2pCatchupSnapshot
}

func (p *Platform) configureP2PCatchup(opts map[string]any) {
	if enabled, ok := opts["p2p_catchup"].(bool); ok && !enabled {
		return
	}
	dataDir, _ := opts["cc_data_dir"].(string)
	// An embedded platform without a data directory must retain the old policy:
	// accepting historical messages without durable deduplication is unsafe.
	if dataDir == "" {
		return
	}
	project, _ := opts["cc_project"].(string)
	sessionStore, _ := opts["cc_session_store"].(string)
	key := sha256.Sum256([]byte(p.domain + "\x00" + p.appID + "\x00" + project))
	path := filepath.Join(dataDir, "feishu", fmt.Sprintf("p2p-%x.json", key[:16]))
	state, err := loadP2PCatchup(path, sessionStore, project)
	if err != nil {
		slog.Warn(p.tag()+": cannot load p2p catchup state; catchup disabled", "error", err)
		return
	}
	p.catchup = state
}

func loadP2PCatchup(path, sessionStore, project string) (*p2pCatchup, error) {
	s := &p2pCatchup{path: path, sessionStore: sessionStore, project: project}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read p2p catchup state: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &s.snapshot); err != nil {
			return nil, fmt.Errorf("decode p2p catchup state: %w", err)
		}
	}
	if s.snapshot.Processed == nil {
		s.snapshot.Processed = make(map[string]time.Time)
	}
	if s.snapshot.PrivateChats == nil {
		s.snapshot.PrivateChats = make(map[string]time.Time)
	}
	s.prune(time.Now())
	return s, nil
}

// claim serializes WS retries against startup history replay and commits the ID
// before handing off to the asynchronous handler. Like the existing in-memory
// dedup, this provides at-most-once admission, not transactional agent delivery.
func (s *p2pCatchup) claim(id, chat string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	if _, ok := s.snapshot.Processed[id]; id != "" && ok {
		return true, nil
	}
	if id == "" || chat == "" {
		return true, nil
	}
	s.snapshot.Processed[id] = now
	s.snapshot.PrivateChats[chat] = now
	s.prune(now)
	if err := s.saveLocked(); err != nil {
		delete(s.snapshot.Processed, id)
		return false, err
	}
	return false, nil
}

func (s *p2pCatchup) prune(now time.Time) {
	pruneP2PEntries(s.snapshot.Processed, now.Add(-p2pDedupRetention), p2pMaxMessageIDs)
	pruneP2PEntries(s.snapshot.PrivateChats, time.Time{}, p2pMaxMessageIDs)
}

func pruneP2PEntries(entries map[string]time.Time, cutoff time.Time, limit int) {
	for id, t := range entries {
		if t.Before(cutoff) {
			delete(entries, id)
		}
	}
	if len(entries) <= limit {
		return
	}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if entries[ids[i]].Equal(entries[ids[j]]) {
			return ids[i] < ids[j]
		}
		return entries[ids[i]].Before(entries[ids[j]])
	})
	for _, id := range ids[:len(ids)-limit] {
		delete(entries, id)
	}
}

// Keep the temporary file on the destination filesystem. core.AtomicWriteFile
// can fall back to a non-atomic overwrite on EXDEV, which is unsuitable here.
func (s *p2pCatchup) saveLocked() error {
	data, err := json.Marshal(s.snapshot)
	if err != nil {
		return fmt.Errorf("encode p2p catchup state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("create p2p state directory: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".p2p-*")
	if err != nil {
		return fmt.Errorf("create p2p state snapshot: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		err = os.Rename(f.Name(), s.path)
	}
	if err != nil {
		return fmt.Errorf("commit p2p catchup state: %w", err)
	}
	return nil
}

func (s *p2pCatchup) heartbeat(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Heartbeat = now
	s.prune(now)
	if err := s.saveLocked(); err != nil {
		slog.Warn("feishu: persist p2p heartbeat failed", "error", err)
	}
}

func (p *Platform) startP2PCatchup(ready <-chan struct{}) {
	if p.catchup == nil {
		return
	}
	p.catchup.bufferMu.Lock()
	p.catchup.recovering = true
	p.catchup.bufferMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	p.catchupCancel = cancel
	p.catchupDone = make(chan struct{})
	go func() {
		defer close(p.catchupDone)
		defer p.finishP2PCatchup(ctx, nil)
		select {
		case <-ctx.Done():
			return
		case <-ready:
		}
		// Do not advance the persisted watermark until recovery completes. A crash
		// during recovery must leave the old interval available on the next start.
		p.recoverP2PMessages(ctx, time.Now())
		if ctx.Err() != nil {
			return
		}
		p.catchup.heartbeat(time.Now())
		ticker := time.NewTicker(p2pHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				p.catchup.heartbeat(now)
			}
		}
	}()
}

func (p *Platform) stopP2PCatchup() {
	if p.catchupCancel == nil {
		return
	}
	p.catchupCancel()
	<-p.catchupDone
	// Leave the last heartbeat intact: writes at shutdown can skip messages
	// created after our event source stopped but before teardown completed.
}

// knownPrivateChats gets IDs only from this project's existing session records,
// intersected with event-confirmed private-chat metadata. No chat-list API is used.
func (s *p2pCatchup) knownPrivateChats(platform string) ([]string, error) {
	if s.sessionStore == "" {
		return nil, nil
	}
	paths := []string{s.sessionStore}
	// Multi-workspace engines save sibling stores with this exact naming scheme.
	if s.project != "" {
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(s.sessionStore), s.project+"_ws_????????.json"))
		if err != nil {
			return nil, fmt.Errorf("find workspace session records: %w", err)
		}
		paths = append(paths, matches...)
	}
	known := make(map[string]bool)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read catchup session records: %w", err)
		}
		var records struct {
			Active map[string]string   `json:"active_session"`
			Users  map[string][]string `json:"user_sessions"`
		}
		if err := json.Unmarshal(data, &records); err != nil {
			return nil, fmt.Errorf("decode catchup session records: %w", err)
		}
		for key := range records.Active {
			addSessionChat(known, platform, key)
		}
		for key := range records.Users {
			addSessionChat(known, platform, key)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chats := make([]string, 0, len(known))
	for chat := range known {
		if _, ok := s.snapshot.PrivateChats[chat]; ok {
			chats = append(chats, chat)
		}
	}
	sort.Strings(chats)
	return chats, nil
}

func addSessionChat(chats map[string]bool, platform, key string) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) == 3 && parts[0] == platform && parts[1] != "" {
		chats[parts[1]] = true
	}
}

func (p *Platform) recoverP2PMessages(parent context.Context, end time.Time) {
	if p.catchup == nil {
		return
	}
	var events []*larkim.P2MessageReceiveV1
	defer func() { p.finishP2PCatchup(parent, events) }()
	if p.groupOnly {
		return
	}
	p.catchup.mu.Lock()
	start := p.catchup.snapshot.Heartbeat
	p.catchup.mu.Unlock()
	// First run establishes a baseline; it must not replay existing conversation
	// history without a prior heartbeat and private-chat classification.
	if start.IsZero() {
		return
	}
	if cutoff := end.Add(-p2pCatchupWindow); start.Before(cutoff) {
		start = cutoff
	}
	if !start.Before(end) {
		return
	}
	chats, err := p.catchup.knownPrivateChats(p.platformName)
	if err != nil {
		slog.Warn(p.tag()+": p2p catchup skipped", "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	for _, chat := range chats {
		items, err := p.listP2PMessages(ctx, chat, start, end)
		if err != nil {
			if parent.Err() == nil {
				slog.Warn(p.tag()+": p2p catchup skipped", "error", err)
			}
			return // In particular, permission errors produce just one Warn per startup.
		}
		for _, msg := range items {
			if event := catchupMessageEvent(msg); event != nil {
				events = append(events, event)
			}
		}
	}
}

func messageTime(msg *larkim.Message) time.Time {
	ms, _ := strconv.ParseInt(stringValue(msg.CreateTime), 10, 64)
	return time.UnixMilli(ms)
}

func (p *Platform) listP2PMessages(ctx context.Context, chat string, start, end time.Time) ([]*larkim.Message, error) {
	var items []*larkim.Message
	token := ""
	seenTokens := make(map[string]bool)
	for {
		req := larkim.NewListMessageReqBuilder().ContainerIdType("chat").ContainerId(chat).
			StartTime(strconv.FormatInt(start.Unix(), 10)).EndTime(strconv.FormatInt(end.Unix()+1, 10)).
			SortType("ByCreateTimeAsc").PageSize(50).PageToken(token).Build()
		resp, err := p.client.Im.Message.List(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("list private messages: %w", err)
		}
		if !resp.Success() {
			return nil, fmt.Errorf("list private messages: API code %d (history scope may be missing)", resp.Code)
		}
		if resp.Data == nil {
			return nil, fmt.Errorf("list private messages: missing response data")
		}
		for _, msg := range resp.Data.Items {
			if msg == nil || messageTime(msg).Before(start) || messageTime(msg).After(end) {
				continue
			}
			// The requested chat, not a response-supplied chat ID, owns these messages.
			msg.ChatId = &chat
			items = append(items, msg)
		}
		if resp.Data.HasMore == nil || !*resp.Data.HasMore {
			return items, nil
		}
		token = stringValue(resp.Data.PageToken)
		if token == "" || seenTokens[token] {
			return nil, fmt.Errorf("list private messages: invalid pagination token")
		}
		seenTokens[token] = true
	}
}

func catchupMessageEvent(msg *larkim.Message) *larkim.P2MessageReceiveV1 {
	if msg.Sender == nil || stringValue(msg.Sender.SenderType) != "user" || msg.Body == nil ||
		stringValue(msg.MessageId) == "" || stringValue(msg.Sender.Id) == "" || (msg.Deleted != nil && *msg.Deleted) {
		return nil
	}
	user := &larkim.UserId{}
	switch stringValue(msg.Sender.IdType) {
	case "open_id":
		user.OpenId = msg.Sender.Id
	case "user_id":
		user.UserId = msg.Sender.Id
	case "union_id":
		user.UnionId = msg.Sender.Id
	default:
		return nil
	}
	chatType := "p2p"
	mentions := make([]*larkim.MentionEvent, 0, len(msg.Mentions))
	for _, mention := range msg.Mentions {
		if mention == nil {
			continue
		}
		id := &larkim.UserId{}
		switch stringValue(mention.IdType) {
		case "open_id":
			id.OpenId = mention.Id
		case "user_id":
			id.UserId = mention.Id
		case "union_id":
			id.UnionId = mention.Id
		}
		mentions = append(mentions, &larkim.MentionEvent{Key: mention.Key, Id: id, Name: mention.Name, TenantKey: mention.TenantKey})
	}
	return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{SenderId: user, SenderType: msg.Sender.SenderType, TenantKey: msg.Sender.TenantKey},
		Message: &larkim.EventMessage{MessageId: msg.MessageId, ChatId: msg.ChatId, ChatType: &chatType,
			MessageType: msg.MsgType, Content: msg.Body.Content, CreateTime: msg.CreateTime,
			RootId: msg.RootId, ParentId: msg.ParentId, ThreadId: msg.ThreadId, Mentions: mentions},
	}}
}

// bufferLiveMessage prevents a newer live message from advancing the engine's
// stale-message watermark ahead of history that is still being fetched.
func (s *p2pCatchup) bufferLiveMessage(event *larkim.P2MessageReceiveV1) bool {
	if event == nil || event.Event == nil || event.Event.Message == nil || stringValue(event.Event.Message.ChatType) != "p2p" {
		return false
	}
	s.bufferMu.Lock()
	defer s.bufferMu.Unlock()
	if !s.recovering {
		return false
	}
	// The SDK owns the incoming event. Retain an independent snapshot while the
	// history request is in flight, including nested sender and mention objects.
	data, err := json.Marshal(event)
	if err != nil {
		slog.Warn("feishu: buffer private message failed", "error", err)
		return false
	}
	var copy larkim.P2MessageReceiveV1
	if err := json.Unmarshal(data, &copy); err != nil {
		slog.Warn("feishu: decode buffered private message failed", "error", err)
		return false
	}
	s.pending = append(s.pending, &copy)
	return true
}

func (p *Platform) finishP2PCatchup(ctx context.Context, events []*larkim.P2MessageReceiveV1) {
	for {
		p.catchup.bufferMu.Lock()
		events = append(events, p.catchup.pending...)
		p.catchup.pending = nil
		if len(events) == 0 || ctx.Err() != nil {
			p.catchup.recovering = false
			p.catchup.bufferMu.Unlock()
			return
		}
		p.catchup.bufferMu.Unlock()
		sort.SliceStable(events, func(i, j int) bool {
			left, _ := strconv.ParseInt(stringValue(events[i].Event.Message.CreateTime), 10, 64)
			right, _ := strconv.ParseInt(stringValue(events[j].Event.Message.CreateTime), 10, 64)
			return left < right
		})
		for _, event := range events {
			if ctx.Err() != nil {
				break
			}
			if err := p.handleMessageEvent(ctx, event, true); err != nil {
				slog.Warn(p.tag()+": dispatch p2p catchup failed", "error", err)
			}
		}
		events = nil
	}
}
