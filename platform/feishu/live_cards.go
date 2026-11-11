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
	"sync"
	"time"
)

// liveCardEntry is a card that shows work in progress (a spinner): a turn's
// card or a background task's card. Entries leave the store when the card
// settles; whatever is left at startup was cut off by a restart.
type liveCardEntry struct {
	MessageID string    `json:"message_id"`
	CardID    string    `json:"card_id,omitempty"`
	Text      string    `json:"text"` // what the card says if a restart cuts it off
	Since     time.Time `json:"since"`
}

type liveCardStore struct {
	mu       sync.Mutex
	path     string
	entries  map[string]liveCardEntry
	leftover []liveCardEntry
}

// configureLiveCards keeps the store next to the p2p catchup state. Without a
// data directory nothing is recorded and restarts leave cards as they were.
func (p *Platform) configureLiveCards(opts map[string]any) {
	dataDir, _ := opts["cc_data_dir"].(string)
	if dataDir == "" {
		return
	}
	project, _ := opts["cc_project"].(string)
	key := sha256.Sum256([]byte(p.domain + "\x00" + p.appID + "\x00" + project))
	store, err := loadLiveCards(filepath.Join(dataDir, "feishu", fmt.Sprintf("live-cards-%x.json", key[:16])))
	if err != nil {
		slog.Warn(p.tag()+": cannot load live card state; interrupted cards will not be updated", "error", err)
		return
	}
	p.liveCards = store
}

func loadLiveCards(path string) (*liveCardStore, error) {
	s := &liveCardStore{path: path, entries: make(map[string]liveCardEntry)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read live card state: %w", err)
	}
	var entries []liveCardEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode live card state: %w", err)
	}
	for _, e := range entries {
		if e.MessageID != "" {
			s.entries[e.MessageID] = e
			s.leftover = append(s.leftover, e)
		}
	}
	return s, nil
}

func (s *liveCardStore) add(messageID, cardID, text string) {
	if s == nil || messageID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[messageID] = liveCardEntry{MessageID: messageID, CardID: cardID, Text: text, Since: time.Now()}
	s.saveLocked()
}

func (s *liveCardStore) remove(messageID string) {
	if s == nil || messageID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[messageID]; !ok {
		return
	}
	delete(s.entries, messageID)
	s.saveLocked()
}

func (s *liveCardStore) takeLeftover() []liveCardEntry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.leftover
	s.leftover = nil
	return out
}

func (s *liveCardStore) saveLocked() {
	entries := make([]liveCardEntry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	if err := writeFileAtomic(s.path, entries); err != nil {
		slog.Warn("feishu: save live card state failed", "error", err)
	}
}

func writeFileAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".live-cards-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err == nil {
		err = f.Close()
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	return err
}

// settleLeftoverCards rewrites the cards a restart cut off, so none keeps a
// spinner for work that is no longer running. Each card is tried once.
func (p *Platform) settleLeftoverCards(ctx context.Context) {
	for _, c := range p.liveCards.takeLeftover() {
		if err := p.settleLeftoverCard(ctx, c); err != nil {
			slog.Warn(p.tag()+": cannot mark interrupted card", "message_id", c.MessageID, "error", err)
		} else {
			slog.Info(p.tag()+": marked interrupted card", "message_id", c.MessageID)
		}
		p.liveCards.remove(c.MessageID)
	}
}

func (p *Platform) settleLeftoverCard(ctx context.Context, c liveCardEntry) error {
	card := greyLineCardJSON(c.Text)
	if c.CardID == "" {
		return p.patchCardMessage(ctx, c.MessageID, card)
	}
	// The previous process's sequence counter is gone. CardKit only needs
	// each sequence to exceed the card's last one; per-card counters start at
	// 1 and stay far below the current Unix time in seconds.
	h := &feishuPreviewHandle{messageID: c.MessageID, cardID: c.CardID, sequence: int(time.Now().Unix())}
	p.closeSegmentedStream(ctx, h)
	return p.updateCardEntity(ctx, h, card)
}

// greyLineCardJSON is a settled card holding one quiet grey line.
func greyLineCardJSON(text string) string {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "default", "summary": map[string]any{"content": text}},
		"body": map[string]any{"elements": []map[string]any{{
			"tag": "markdown", "content": "<font color='grey'>" + escapeCardText(text) + "</font>", "text_size": "notation",
		}}},
	}
	b, _ := json.Marshal(card)
	return string(b)
}
