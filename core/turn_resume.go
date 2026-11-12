package core

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
	"strings"
	"sync"
	"time"
)

// Resume after restart: the bridge keeps a small record of the turns running
// now and of the background tasks with an open card, per session. A restart
// (a deploy, an OOM kill, a crash) leaves that record behind. On the next
// start each session in it gets one internal turn that names what was cut off
// and asks the agent to check its state, carry on, and tell the person in one
// sentence. Work cut off more than resumeMaxAge ago, or a resume turn that was
// itself cut off, only gets one line telling the person it stopped. Messages
// sent while the bridge was down are recovered by the platform (Feishu
// p2p_catchup) first; the resume turn waits resumeStartDelay and then queues
// behind them like any scheduled prompt.

const (
	resumeMaxAge         = 6 * time.Hour
	resumeStartDelay     = 20 * time.Second
	inflightBeat         = time.Minute
	inflightRequestRunes = 60
	inflightMaxTasks     = 5
	// resumeMarker opens every resume prompt, so a resume turn that is itself
	// cut off is recognised and not resumed again.
	resumeMarker = "【重启续做】"
)

type inflightTurn struct {
	Request string    `json:"request,omitempty"`
	Started time.Time `json:"started"`
	Resume  bool      `json:"resume,omitempty"`
}

type inflightSession struct {
	Turn  *inflightTurn     `json:"turn,omitempty"`
	Tasks map[string]string `json:"tasks,omitempty"` // task ID → task name
}

func (s *inflightSession) empty() bool { return s.Turn == nil && len(s.Tasks) == 0 }

type inflightFile struct {
	AliveAt  time.Time                   `json:"alive_at"` // the last write; a restart cut the work off after it
	Sessions map[string]*inflightSession `json:"sessions,omitempty"`
}

// inflightStore persists the work in progress. Every method is a no-op on a
// nil store and once ctx is done, so work cut off by shutdown stays recorded.
type inflightStore struct {
	path string
	ctx  context.Context
	mu   sync.Mutex
	data inflightFile
}

func inflightPath(dataDir, project string) string {
	return filepath.Join(dataDir, "inflight", fmt.Sprintf("%x.json", sha256.Sum256([]byte(project))))
}

// openInflightStore reads what the previous process left, then starts an
// empty record so each leftover is handled once.
func openInflightStore(ctx context.Context, path string) (*inflightStore, inflightFile) {
	var left inflightFile
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &left); err != nil {
			slog.Warn("resume: unreadable record, ignored", "path", path, "error", err)
			left = inflightFile{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("resume: record not read", "path", path, "error", err)
	}
	s := &inflightStore{path: path, ctx: ctx, data: inflightFile{Sessions: map[string]*inflightSession{}}}
	s.mu.Lock()
	s.saveLocked()
	s.mu.Unlock()
	go s.beat()
	return s, left
}

func (s *inflightStore) live() bool { return s != nil && s.ctx.Err() == nil }

func (s *inflightStore) saveLocked() {
	for k, v := range s.data.Sessions {
		if v.empty() {
			delete(s.data.Sessions, k)
		}
	}
	s.data.AliveAt = time.Now()
	data, err := json.Marshal(s.data)
	if err == nil {
		err = AtomicWriteFile(s.path, data, 0o600)
	}
	if err != nil {
		slog.Warn("resume: record not saved", "path", s.path, "error", err)
	}
}

// beat keeps AliveAt within inflightBeat of a crash while work is recorded.
func (s *inflightStore) beat() {
	t := time.NewTicker(inflightBeat)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			if len(s.data.Sessions) > 0 && s.ctx.Err() == nil {
				s.saveLocked()
			}
			s.mu.Unlock()
		}
	}
}

func (s *inflightStore) update(key string, fn func(*inflightSession)) {
	if !s.live() || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	rec := s.data.Sessions[key]
	if rec == nil {
		rec = &inflightSession{}
		s.data.Sessions[key] = rec
	}
	fn(rec)
	s.saveLocked()
}

func (s *inflightStore) turnStart(key, request string) {
	s.update(key, func(r *inflightSession) {
		r.Turn = &inflightTurn{Request: clipRunes(request, inflightRequestRunes), Started: time.Now(), Resume: strings.Contains(request, resumeMarker)}
	})
}

func (s *inflightStore) turnEnd(key string) {
	s.update(key, func(r *inflightSession) { r.Turn = nil })
}

func (s *inflightStore) taskOpen(key, id, name string) {
	s.update(key, func(r *inflightSession) {
		if r.Tasks == nil {
			r.Tasks = map[string]string{}
		}
		r.Tasks[id] = name
	})
}

func (s *inflightStore) taskEnd(key, id string) {
	s.update(key, func(r *inflightSession) { delete(r.Tasks, id) })
}

// noteTurnStart records the turn processTurnEvents is about to run, named by
// the latest user entry of the session's history.
func (e *Engine) noteTurnStart(sessionKey string, session *Session) {
	if e.inflight == nil || strings.Contains(sessionKey, "#timer:") {
		return // a per-run timer session is gone after a restart
	}
	request := ""
	h := session.GetHistory(4)
	for k := len(h) - 1; k >= 0; k-- {
		if h[k].Role == "user" {
			request = h[k].Content
			break
		}
	}
	e.inflight.turnStart(sessionKey, request)
}

// noteTurnEnd forgets the turn unless the engine is shutting down: a turn
// cut off by shutdown stays recorded for the next start.
func (e *Engine) noteTurnEnd(sessionKey string) {
	e.inflight.turnEnd(sessionKey)
}

// openInflight starts the record for this process and keeps what the previous
// one left for resumeInterrupted.
func (e *Engine) openInflight() {
	if e.dataDir == "" {
		return
	}
	store, left := openInflightStore(e.ctx, inflightPath(e.dataDir, e.name))
	e.inflight = store
	e.taskCards.inflight = store
	e.inflightLeft = left
}

// scheduleResume runs the resume pass once, resumeStartDelay after the first
// platform is ready.
func (e *Engine) scheduleResume() {
	e.resumeOnce.Do(func() {
		if len(e.inflightLeft.Sessions) == 0 {
			return
		}
		go func() {
			if !sleepCtx(e.ctx, e.resumeDelay) {
				return
			}
			e.resumeInterrupted(time.Now())
		}()
	})
}

// resumeInterrupted handles every session the previous process left with work
// in progress.
func (e *Engine) resumeInterrupted(now time.Time) {
	left := e.inflightLeft
	e.inflightLeft = inflightFile{}
	at := left.AliveAt
	clock := at.In(decisionZone).Format("15:04")
	keys := make([]string, 0, len(left.Sessions))
	for k := range left.Sessions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rec := left.Sessions[key]
		if rec == nil || rec.empty() {
			continue
		}
		items := e.interruptedItems(rec)
		switch {
		case at.IsZero() || now.Sub(at) > resumeMaxAge:
			e.notifyInterrupted(key, clock, "too_old")
		case rec.Turn != nil && rec.Turn.Resume:
			e.notifyInterrupted(key, clock, "resume_cut_off")
		default:
			e.promptResume(key, clock, items, now)
		}
	}
}

// interruptedItems names what was cut off: the request of the running turn
// and the background tasks.
func (e *Engine) interruptedItems(rec *inflightSession) []string {
	var items []string
	if rec.Turn != nil {
		req := strings.TrimSpace(rec.Turn.Request)
		if req == "" {
			items = append(items, e.i18n.T(MsgResumeItemTurn))
		} else {
			items = append(items, e.i18n.Tf(MsgResumeItemRequest, req))
		}
	}
	names := make([]string, 0, len(rec.Tasks))
	for _, name := range rec.Tasks {
		if strings.TrimSpace(name) == "" {
			name = e.i18n.T(MsgTaskUntitled)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > inflightMaxTasks {
		names = names[:inflightMaxTasks]
	}
	for _, name := range names {
		items = append(items, e.i18n.Tf(MsgResumeItemTask, name))
	}
	return items
}

func (e *Engine) promptResume(key, clock string, items []string, now time.Time) {
	silent := true
	job := &TimerJob{
		ID:          "resume-" + fmt.Sprint(now.UnixNano()),
		Project:     e.name,
		SessionKey:  key,
		ScheduledAt: now,
		Prompt:      resumeMarker + e.i18n.Tf(MsgResumePrompt, clock, strings.Join(items, e.i18n.T(MsgResumeItemSep))),
		Description: "resume after restart",
		SessionMode: "reuse",
		Silent:      &silent,
		Urgent:      true,
	}
	slog.Info("resume: prompting", "session", key, "items", len(items))
	// The job runs the whole turn, so each session resumes on its own.
	go func() {
		if err := e.ExecuteTimerJob(job); err != nil {
			slog.Warn("resume: prompt not delivered", "session", key, "error", err)
			e.notifyInterrupted(key, clock, "prompt_failed")
		}
	}()
}

// notifyInterrupted tells the person, in one line, that the work stopped.
func (e *Engine) notifyInterrupted(key, clock, reason string) {
	p, replyCtx, err := e.proactiveTarget(key)
	if err == nil {
		err = e.sendWithError(p, replyCtx, e.i18n.Tf(MsgResumeTooOld, clock))
	}
	if err != nil {
		slog.Warn("resume: notice not sent", "session", key, "reason", reason, "error", err)
		return
	}
	slog.Info("resume: notified", "session", key, "reason", reason)
}

// proactiveTarget finds the platform and reply context of a session with no
// live state, the way a timer does.
func (e *Engine) proactiveTarget(sessionKey string) (Platform, any, error) {
	name, _, _ := strings.Cut(sessionKey, ":")
	var target Platform
	for _, p := range e.platforms {
		if p.Name() == name {
			target = p
			break
		}
	}
	if target == nil { // multi-workspace: "<workspace>:<platform>:..."
		for _, p := range e.platforms {
			if idx := strings.Index(sessionKey, ":"+p.Name()+":"); idx >= 0 {
				target, sessionKey = p, sessionKey[idx+1:]
				break
			}
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("platform %q not found", name)
	}
	rc, ok := target.(ReplyContextReconstructor)
	if !ok {
		return nil, nil, fmt.Errorf("platform %s: %w", target.Name(), ErrNotSupported)
	}
	replyCtx, err := rc.ReconstructReplyCtx(sessionKey)
	if err != nil {
		return nil, nil, err
	}
	return target, replyCtx, nil
}
