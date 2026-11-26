package modelrouter

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// A thread is one conversation as the user sees it. Each shell keeps its own
// session of it; the thread links them, remembers which shell holds the
// conversation now, and when each shell was last left, so a shell coming back
// is told only what happened since.
type thread struct {
	ID      string               `json:"id"`
	Active  string               `json:"active,omitempty"`
	IDs     map[string]string    `json:"ids,omitempty"`  // shell -> its current session id
	Seen    map[string]string    `json:"seen,omitempty"` // every session id the thread had -> shell
	Left    map[string]time.Time `json:"left,omitempty"` // shell -> when the conversation moved away from it
	Updated time.Time            `json:"updated"`
}

// entry is one recorded line of a thread: a user message, a final reply, or
// a one-line tool call.
type entry struct {
	Time    time.Time `json:"t"`
	Shell   string    `json:"s"`
	Role    string    `json:"r"` // roleUser, roleAssistant, roleTool
	Content string    `json:"c"`
}

const (
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

var (
	// threadTTL drops threads nobody touched for this long when the store loads.
	threadTTL = 90 * 24 * time.Hour
	// transcriptMaxBytes bounds a thread's transcript file; past it the file
	// keeps its newer half. A handoff never needs more than the recent part.
	transcriptMaxBytes int64 = 1 << 20
	// memoryMaxEntries bounds a thread's transcript when the store has no
	// directory (tests, no data_dir).
	memoryMaxEntries = 2000
)

// store holds the threads of one project. Agents of the same project (the
// project agent, per-topic and per-workspace copies) share one store.
type store struct {
	dir string // "" keeps everything in memory

	mu      sync.Mutex
	threads map[string]*thread
	byID    map[string]*thread
	memory  map[string][]entry
}

var (
	storesMu sync.Mutex
	stores   = map[string]*store{}
)

// openStore returns the store for dir, loading it on first use.
func openStore(dir string) *store {
	if dir == "" {
		return newStore("")
	}
	storesMu.Lock()
	defer storesMu.Unlock()
	if s, ok := stores[dir]; ok {
		return s
	}
	s := newStore(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("model_router: cannot create state dir, keeping threads in memory", "dir", dir, "error", err)
		s.dir = ""
	} else {
		s.load()
	}
	stores[dir] = s
	return s
}

func newStore(dir string) *store {
	return &store{
		dir:     dir,
		threads: map[string]*thread{},
		byID:    map[string]*thread{},
		memory:  map[string][]entry{},
	}
}

func (s *store) indexPath() string { return filepath.Join(s.dir, "threads.json") }

func (s *store) transcriptPath(threadID string) string {
	return filepath.Join(s.dir, threadID+".jsonl")
}

func (s *store) load() {
	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("model_router: read threads", "error", err)
		}
		return
	}
	var list []*thread
	if err := json.Unmarshal(data, &list); err != nil {
		slog.Warn("model_router: parse threads", "error", err)
		return
	}
	cutoff := time.Now().Add(-threadTTL)
	for _, th := range list {
		if th == nil || th.ID == "" {
			continue
		}
		if th.Updated.Before(cutoff) {
			_ = os.Remove(s.transcriptPath(th.ID))
			continue
		}
		// omitempty drops empty maps on save, so they come back nil.
		if th.IDs == nil {
			th.IDs = map[string]string{}
		}
		if th.Seen == nil {
			th.Seen = map[string]string{}
		}
		if th.Left == nil {
			th.Left = map[string]time.Time{}
		}
		s.index(th)
	}
}

func (s *store) index(th *thread) {
	s.threads[th.ID] = th
	for id := range th.Seen {
		s.byID[id] = th
	}
}

// saveLocked writes the thread index. Threads that never got a session id
// are not worth keeping. Caller holds s.mu.
func (s *store) saveLocked() {
	if s.dir == "" {
		return
	}
	list := make([]*thread, 0, len(s.threads))
	for _, th := range s.threads {
		if len(th.Seen) > 0 {
			list = append(list, th)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		slog.Warn("model_router: encode threads", "error", err)
		return
	}
	if err := core.AtomicWriteFile(s.indexPath(), data, 0o600); err != nil {
		slog.Warn("model_router: save threads", "error", err)
	}
}

// lookup returns the thread that had sessionID, or nil.
func (s *store) lookup(sessionID string) *thread {
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[sessionID]
}

// threadFor returns the thread of sessionID, creating one when the id is new
// (shell names the shell that owns it) or empty.
func (s *store) threadFor(sessionID, shell string) *thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID != "" {
		if th := s.byID[sessionID]; th != nil {
			return th
		}
	}
	th := &thread{
		ID:      newThreadID(),
		IDs:     map[string]string{},
		Seen:    map[string]string{},
		Left:    map[string]time.Time{},
		Updated: time.Now(),
	}
	if sessionID != "" {
		th.Active = shell
		th.IDs[shell] = sessionID
		th.Seen[sessionID] = shell
		s.byID[sessionID] = th
	}
	s.threads[th.ID] = th
	if sessionID != "" {
		s.saveLocked()
	}
	return th
}

// snapshot returns a copy of th's state.
func (s *store) snapshot(th *thread) thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *th
	cp.IDs = copyMap(th.IDs)
	cp.Left = make(map[string]time.Time, len(th.Left))
	for k, v := range th.Left {
		cp.Left[k] = v
	}
	cp.Seen = nil
	return cp
}

// setActive marks shell as the thread's shell without a switch (new thread).
func (s *store) setActive(th *thread, shell string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if th.Active == "" {
		th.Active = shell
	}
}

// setID records shell's current session id for th.
func (s *store) setID(th *thread, shell, sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if th.IDs[shell] == sessionID && th.Seen[sessionID] == shell {
		return
	}
	th.IDs[shell] = sessionID
	th.Seen[sessionID] = shell
	if th.Active == "" {
		th.Active = shell
	}
	th.Updated = time.Now()
	s.byID[sessionID] = th
	s.threads[th.ID] = th
	s.saveLocked()
}

// moved records that the conversation went from one shell to another.
func (s *store) moved(th *thread, from, to string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	th.Active = to
	th.Left[from] = at
	th.Updated = at
	s.saveLocked()
}

// forget drops sessionID from its thread.
func (s *store) forget(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	th := s.byID[sessionID]
	if th == nil {
		return
	}
	delete(s.byID, sessionID)
	shell := th.Seen[sessionID]
	delete(th.Seen, sessionID)
	if th.IDs[shell] == sessionID {
		delete(th.IDs, shell)
	}
	s.saveLocked()
}

// shellOf reports which shell sessionID belongs to, "" when unknown.
func (s *store) shellOf(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if th := s.byID[sessionID]; th != nil {
		return th.Seen[sessionID]
	}
	return ""
}

// record appends e to th's transcript.
func (s *store) record(th *thread, e entry) {
	if e.Content == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		list := append(s.memory[th.ID], e)
		if len(list) > memoryMaxEntries {
			list = append([]entry(nil), list[len(list)-memoryMaxEntries/2:]...)
		}
		s.memory[th.ID] = list
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	path := s.transcriptPath(th.ID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		slog.Warn("model_router: record transcript", "error", err)
		return
	}
	_, werr := f.Write(append(line, '\n'))
	info, serr := f.Stat()
	_ = f.Close()
	if werr != nil {
		slog.Warn("model_router: record transcript", "error", werr)
		return
	}
	if serr == nil && info.Size() > transcriptMaxBytes {
		trimTranscript(path, transcriptMaxBytes/2)
	}
}

// trimTranscript keeps the newest whole lines of path that fit in keep bytes.
func trimTranscript(path string, keep int64) {
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) <= keep {
		return
	}
	tail := data[int64(len(data))-keep:]
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	if err := core.AtomicWriteFile(path, tail, 0o600); err != nil {
		slog.Warn("model_router: trim transcript", "error", err)
	}
}

// transcript returns th's recorded entries, oldest first. A user message is
// recorded once the shell took it, which can be after the first events of
// its answer, so entries are ordered by time.
func (s *store) transcript(th *thread) []entry {
	out := s.readTranscript(th)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func (s *store) readTranscript(th *thread) []entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return append([]entry(nil), s.memory[th.ID]...)
	}
	f, err := os.Open(s.transcriptPath(th.ID))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Content != "" {
			out = append(out, e)
		}
	}
	return out
}

func newThreadID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
