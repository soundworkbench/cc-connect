package feishu

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// topicMemory remembers the topic a session's latest message came from, for
// sessions whose key does not name the topic (thread_isolation off: one
// session per person in a topic group or across a group's topics). Messages
// built from the session key alone, such as a decision's answer, a turn
// resumed after a restart or a timer, then reply in that topic instead of
// posting to the chat's main stream.
type topicMemory struct {
	mu    sync.Mutex
	path  string            // empty: kept in memory only
	roots map[string]string // session key -> topic root message ID
}

// configureTopicMemory keeps the memory next to the other Feishu state so it
// survives restarts; without a data directory it lives in memory.
func (p *Platform) configureTopicMemory(opts map[string]any) {
	m := &topicMemory{roots: make(map[string]string)}
	if dataDir, _ := opts["cc_data_dir"].(string); dataDir != "" {
		project, _ := opts["cc_project"].(string)
		key := sha256.Sum256([]byte(p.domain + "\x00" + p.appID + "\x00" + project))
		m.path = filepath.Join(dataDir, "feishu", fmt.Sprintf("topics-%x.json", key[:16]))
		data, err := os.ReadFile(m.path)
		if err == nil {
			err = json.Unmarshal(data, &m.roots)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn(p.tag()+": cannot load topic state; starting empty", "error", err)
		}
		if m.roots == nil {
			m.roots = make(map[string]string)
		}
	}
	p.topics = m
}

// topicRootOf is the root of the topic msg belongs to, or "" when msg is not
// in a topic.
func topicRootOf(msg *larkim.EventMessage) string {
	if stringValue(msg.ThreadId) == "" {
		return ""
	}
	if root := stringValue(msg.RootId); root != "" {
		return root
	}
	return stringValue(msg.MessageId)
}

// note records where sessionKey's latest message came from; rootID "" means
// the main stream.
func (m *topicMemory) note(sessionKey, rootID string) {
	if m == nil || sessionKey == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.roots[sessionKey] == rootID {
		return
	}
	if rootID == "" {
		delete(m.roots, sessionKey)
	} else {
		m.roots[sessionKey] = rootID
	}
	if m.path == "" {
		return
	}
	if err := writeFileAtomic(m.path, m.roots); err != nil {
		slog.Warn("feishu: save topic state failed", "error", err)
	}
}

func (m *topicMemory) root(sessionKey string) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.roots[sessionKey]
}
