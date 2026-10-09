package codex

import (
	"encoding/json"
	"fmt"
	"github.com/chenhg5/cc-connect/core"
	"log/slog"
	"strings"
	"time"
)

// backgroundWakeup is session-local and guarded by stateMu. Restarting does not
// resurrect work. A genuine Send arms the bounded lease; synthetic sends do not.
type backgroundWakeupPolicy struct {
	enabled  bool
	maxTurns int
	lifetime time.Duration
}

func parseBackgroundWakeupPolicy(opts map[string]any) (backgroundWakeupPolicy, error) {
	p := backgroundWakeupPolicy{maxTurns: 10, lifetime: time.Hour}
	p.enabled, _ = opts["background_wakeup"].(bool)
	for key, limit := range map[string]int{"background_wakeup_max_turns": 100, "background_wakeup_timeout_mins": 1440} {
		raw, ok := opts[key]
		if !ok {
			continue
		}
		var n int
		switch v := raw.(type) {
		case int:
			n = v
		case int64:
			n = int(v)
		default:
			return p, fmt.Errorf("codex: %s must be an integer", key)
		}
		if n < 1 || n > limit {
			return p, fmt.Errorf("codex: %s must be between 1 and %d", key, limit)
		}
		if key == "background_wakeup_max_turns" {
			p.maxTurns = n
		} else {
			p.lifetime = time.Duration(n) * time.Minute
		}
	}
	return p, nil
}

type backgroundWakeup struct {
	gate     func() bool
	maxTurns int
	lifetime time.Duration
	enabled  bool
	armed    bool
	deadline time.Time
	used     int
	pending  map[string]bool
	seen     map[string]bool
	readyAt  time.Time
	inFlight bool
}

func (w *backgroundWakeup) turnLimit() int {
	if w.maxTurns == 0 {
		return 10
	}
	return w.maxTurns
}

func (w *backgroundWakeup) disable() { w.armed = false; w.pending = nil }

func (w *backgroundWakeup) userInput(prompt string, now time.Time) {
	if !w.enabled {
		return
	}
	// Fail closed on a stop request, including prompts wrapped with sender metadata.
	// /stop also closes the session and cancels the wakeup goroutine.
	lower := strings.ToLower(prompt)
	for _, stop := range []string{"停止", "暂停", "取消", "stop", "cancel", "pause"} {
		if strings.Contains(lower, stop) {
			w.disable()
			return
		}
	}
	w.armed = true
	lifetime := w.lifetime
	if lifetime == 0 {
		lifetime = time.Hour
	}
	w.deadline = now.Add(lifetime)
	w.used = 0
	if w.seen == nil {
		w.seen = make(map[string]bool)
	}
}

func (w *backgroundWakeup) observe(itemID, childID string, now time.Time) {
	if !w.enabled || !w.armed || itemID == "" || childID == "" || w.seen[itemID] {
		return
	}
	if !now.Before(w.deadline) || w.used >= w.turnLimit() || len(w.seen) >= 4096 {
		w.disable()
		return
	}
	w.seen[itemID] = true
	if w.pending == nil {
		w.pending = make(map[string]bool)
	}
	w.pending[childID] = true
	// A fixed debounce window coalesces a batch without starvation.
	if len(w.pending) == 1 {
		w.readyAt = now.Add(2 * time.Second)
	}
}

func (w *backgroundWakeup) take(now time.Time, busy bool) bool {
	if !w.armed {
		return false
	}
	if !now.Before(w.deadline) || w.used >= w.turnLimit() {
		w.disable()
		return false
	}
	if busy || len(w.pending) == 0 || now.Before(w.readyAt) {
		return false
	}
	w.pending = nil
	w.used++
	w.inFlight = true
	return true
}

func (s *appServerSession) observeBackgroundCompletion(method string, raw json.RawMessage) {
	if method != "item/completed" {
		return
	}
	var n struct {
		ThreadID string `json:"threadId"`
		Item     struct {
			Type          string `json:"type"`
			Kind          string `json:"kind"`
			ID            string `json:"id"`
			AgentThreadID string `json:"agentThreadId"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &n) != nil || n.ThreadID == "" || n.ThreadID != s.CurrentSessionID() || n.Item.Type != "subAgentActivity" || n.Item.Kind != "completed" {
		return
	}
	s.stateMu.Lock()
	s.wakeup.observe(n.Item.ID, n.Item.AgentThreadID, time.Now())
	s.stateMu.Unlock()
}

func (s *appServerSession) startBackgroundWakeup(policy backgroundWakeupPolicy) {
	s.stateMu.Lock()
	s.wakeup.enabled = policy.enabled
	s.wakeup.maxTurns = policy.maxTurns
	s.wakeup.lifetime = policy.lifetime
	s.stateMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case now := <-tick.C:
				s.runBackgroundWakeup(now)
			}
		}
	}()
}

func (s *appServerSession) runBackgroundWakeup(now time.Time) {
	// Serialize automatic and user turn/start requests. Notification handling must
	// never acquire sendMu: RPC replies and events share the reader goroutine.
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if !s.alive.Load() || s.ctx.Err() != nil {
		return
	}
	s.runtimeMu.RLock()
	exhausted := false
	if s.usage != nil {
		for _, b := range s.usage.Buckets {
			for _, w := range b.Windows {
				if w.UsedPercent >= 100 {
					exhausted = true
				}
			}
		}
	}
	s.runtimeMu.RUnlock()
	s.stateMu.Lock()
	if exhausted {
		s.wakeup.disable()
	}
	w := &s.wakeup
	eligible := w.armed && now.Before(w.deadline) && w.used < w.turnLimit() && len(w.pending) > 0 && !now.Before(w.readyAt) && s.currentTurn == ""
	gate := w.gate
	s.stateMu.Unlock()
	if !eligible {
		return
	}
	// Production attaches an engine lease so incoming user messages are queued
	// or steered, never mistaken for a new idle foreground turn.
	if gate != nil && !gate() {
		return
	}
	s.stateMu.Lock()
	ready := s.wakeup.take(now, s.currentTurn != "")
	s.stateMu.Unlock()
	if !ready {
		// Cancellation/native activity may race lease acquisition. Release via the
		// normal unsolicited result path without sending anything to the model.
		s.emit(core.Event{Type: core.EventResult, Done: true, Synthetic: true, Metadata: map[string]any{"background_cancelled": true}, SessionID: s.CurrentSessionID()})
		return
	}
	// No child output is inserted as instructions. Read actual task state using
	// native tools, and do not invent new work from a completion event alone.
	const prompt = "[cc-connect background completion event] A delegated agent has completed. Inspect native agent status/results. Report useful results and continue only the user's already-authorized unfinished task, if any. Respect the latest stop/cancel instruction, task deadline and quota. Do not restart finished or cancelled work, and do not create a new goal. If nothing remains, finish without dispatching more agents."
	if err := s.sendPrompt(prompt, "", nil, nil); err != nil {
		s.stateMu.Lock()
		s.wakeup.disable()
		s.wakeup.inFlight = false
		s.stateMu.Unlock()
		// Do not replay on uncertain transport failure: the server may have accepted.
		s.emit(core.Event{Type: core.EventError, Done: true, Error: &core.UserMessageError{Key: core.MsgAgentTurnFailed, Args: []any{""}}, SessionID: s.CurrentSessionID()})
		slog.Warn("codex background wakeup failed; disabled for this lease")
		return
	}
	slog.Info("codex background completion wakeup started")
}

// SetBackgroundTurnGate is the optional core capability; it has no platform knowledge.
func (s *appServerSession) SetBackgroundTurnGate(gate func() bool) {
	s.stateMu.Lock()
	s.wakeup.gate = gate
	s.stateMu.Unlock()
}
