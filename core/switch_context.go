package core

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// A conversation resumed on another model carries everything the previous
// model accumulated. When that is more than the new model should start with,
// it is compacted before the user's turn runs, while the card says why the
// turn is waiting:
//
//   - on the previous model, while its process is still alive (warm cache, and
//     the conversation is known to fit its window);
//   - otherwise on the new model, before the user's message is sent.
//
// Anything left over is the agent's own auto-compaction, whose progress the
// card also shows (EventCompacting).

// switchCompactTimeout bounds one switch compaction. Measured compactions of
// 370k-token conversations took 81-212s.
var switchCompactTimeout = 15 * time.Minute

// retireAfterModelSwitch ends the live process's run on the previous model
// after a /model switch. The process is kept until its current turn (if any)
// ends; the next turn, queued or new, restarts it on the new model with
// --resume (getOrCreateInteractiveStateWith, drainPendingMessages) after
// compactBeforeModelRecycle had the chance to compact on the previous model.
// A process with background work, or none at all, is closed now.
func (e *Engine) retireAfterModelSwitch(iKey string) {
	e.interactiveMu.Lock()
	state := e.interactiveStates[iKey]
	e.interactiveMu.Unlock()
	if state != nil {
		state.mu.Lock()
		as := state.agentSession
		keep := as != nil && as.Alive() && !state.stopped && !hasBackgroundWork(as)
		if keep {
			state.modelStale = true
		}
		state.mu.Unlock()
		if keep {
			return
		}
	}
	e.cleanupInteractiveState(iKey)
}

// compactBeforeModelRecycle compacts the conversation on the previous model
// when the live process is about to be recycled onto a new one and carries
// more than the new model's switch line. It runs before the process is
// chosen for the turn; failures are logged and left to
// compactCarriedContext, which retries on the new model.
func (e *Engine) compactBeforeModelRecycle(iKey string, agent Agent, session *Session, sp *streamPreview, p Platform, replyCtx any, notify bool) {
	budget, ok := agent.(SwitchContextBudget)
	if !ok {
		return
	}
	e.interactiveMu.Lock()
	state := e.interactiveStates[iKey]
	e.interactiveMu.Unlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	as := state.agentSession
	ready := state.modelStale && as != nil && as.Alive() && !state.stopped && state.pending == nil && !hasBackgroundWork(as)
	state.mu.Unlock()
	if !ready {
		return
	}
	reporter, ok := as.(CarriedContextReporter)
	if !ok {
		return
	}
	target := agentModel(agent)
	tokens, from := reporter.CarriedContext()
	if from != "" && !modelsDiffer(from, target) {
		return
	}
	line := budget.SwitchCompactLine(target)
	if tokens <= 0 || line <= 0 || tokens < line {
		return
	}

	slog.Info("model switch: compacting on the previous model",
		"session_key", iKey, "from", from, "to", target, "tokens", tokens, "line", line)
	e.showSwitchNotice(sp, p, replyCtx, notify)
	e.stopUnsolicitedReader(state)
	drainEvents(as.Events())
	started := time.Now()
	err := e.runSwitchCompact(agent, state, as, session)
	after, _ := reporter.CarriedContext()
	if err != nil {
		slog.Warn("model switch: compaction on the previous model failed",
			"session_key", iKey, "error", err, "elapsed", time.Since(started))
		return
	}
	slog.Info("model switch: compacted on the previous model",
		"session_key", iKey, "tokens_before", tokens, "tokens_after", after, "elapsed", time.Since(started))
}

// compactCarriedContext compacts, on the turn's own (new) model, a
// conversation that a different model left above this model's switch line.
// It runs after the process is chosen and before the user's message is sent:
// the process was just started with --resume, or compaction on the previous
// model was not possible. One attempt per carried state, so a compaction that
// cannot succeed does not repeat on every turn.
func (e *Engine) compactCarriedContext(iKey string, agent Agent, state *interactiveState, as AgentSession, session *Session, sp *streamPreview, p Platform, replyCtx any, notify bool) {
	budget, ok := agent.(SwitchContextBudget)
	if !ok || as == nil {
		return
	}
	reporter, ok := as.(CarriedContextReporter)
	if !ok {
		return
	}
	target := agentModel(agent)
	tokens, from := reporter.CarriedContext()
	if from == "" || !modelsDiffer(from, target) {
		return
	}
	line := budget.SwitchCompactLine(target)
	if tokens <= 0 || line <= 0 || tokens < line {
		return
	}
	attempt := fmt.Sprintf("%s|%s|%d", from, target, tokens)
	if prev, _ := e.switchCompactTried.Load(iKey); prev == attempt {
		return
	}
	e.switchCompactTried.Store(iKey, attempt)

	slog.Info("model switch: compacting on the new model",
		"session_key", iKey, "from", from, "to", target, "tokens", tokens, "line", line)
	e.showSwitchNotice(sp, p, replyCtx, notify)
	started := time.Now()
	err := e.runSwitchCompact(agent, state, as, session)
	if err != nil {
		slog.Warn("model switch: compaction on the new model failed",
			"session_key", iKey, "error", err, "elapsed", time.Since(started))
		state.mu.Lock()
		state.eventsNeedResync = true
		state.mu.Unlock()
		e.clearSwitchNotice(sp)
		return
	}
	after, _ := reporter.CarriedContext()
	slog.Info("model switch: compacted on the new model",
		"session_key", iKey, "tokens_before", tokens, "tokens_after", after, "elapsed", time.Since(started))
	e.clearSwitchNotice(sp)
}

// runSwitchCompact sends the agent's compact command to as and waits for the
// turn it starts to end. The caller owns as's event channel.
func (e *Engine) runSwitchCompact(agent Agent, state *interactiveState, as AgentSession, session *Session) error {
	compressor, ok := agent.(ContextCompressor)
	if !ok || compressor.CompressCommand() == "" {
		return errors.New("agent has no compact command")
	}
	cmd := compressor.CompressCommand()
	if preserve, ok := as.(TurnShellPreserver); ok {
		preserve.SetTurnShellCurrent()
	}
	if err := as.Send(cmd, "", nil, nil); err != nil {
		return fmt.Errorf("send %s: %w", cmd, err)
	}
	timer := time.NewTimer(switchCompactTimeout)
	defer timer.Stop()
	events := as.Events()
	stop := state.stopSignal()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return errors.New("agent process exited during compaction")
			}
			if ev.SessionID != "" && session != nil && session.GetAgentSessionID() != ev.SessionID {
				session.SetAgentSessionID(ev.SessionID, agent.Name())
			}
			switch ev.Type {
			case EventResult:
				return ev.Error
			case EventError:
				if ev.Error != nil {
					return ev.Error
				}
				return errors.New("compaction failed")
			case EventPermissionRequest:
				_ = as.RespondPermission(ev.RequestID, PermissionResult{Behavior: "allow", UpdatedInput: ev.ToolInputRaw})
			}
		case <-stop:
			return errors.New("stopped during compaction")
		case <-timer.C:
			return fmt.Errorf("compaction timed out after %s", switchCompactTimeout)
		case <-e.ctx.Done():
			return e.ctx.Err()
		}
	}
}

// showSwitchNotice tells the user why the turn waits: on the live card when
// there is one, as a message otherwise.
func (e *Engine) showSwitchNotice(sp *streamPreview, p Platform, replyCtx any, notify bool) {
	if !notify {
		return
	}
	text := e.i18n.T(MsgSwitchCompacting)
	if sp != nil {
		sp.setNotice(text)
		sp.mu.Lock()
		shown := sp.notice == text
		sp.mu.Unlock()
		if shown {
			return
		}
	}
	if p != nil {
		e.send(p, replyCtx, text)
	}
}

func (e *Engine) clearSwitchNotice(sp *streamPreview) {
	if sp != nil {
		sp.setNotice("")
	}
}

// modelStaleLocked reports whether state's process should hand its next turn
// to a process on the new model. Caller holds state.mu.
func modelStaleLocked(state *interactiveState, as AgentSession) bool {
	return state.modelStale && as != nil && as.Alive() && !hasBackgroundWork(as)
}

func agentModel(agent Agent) string {
	if ms, ok := agent.(ModelSwitcher); ok {
		return ms.GetModel()
	}
	return ""
}

// modelsDiffer reports whether two model ids name different models. The
// configured id and the one an API reports back often differ only in a
// gateway prefix ("claude/claude-opus-5-5"), a context suffix ("[1m]") or an
// alias ("opus" for "claude-opus-5-5"), so those count as the same model.
func modelsDiffer(a, b string) bool {
	na, nb := normalizeModelID(a), normalizeModelID(b)
	if na == "" || nb == "" {
		return false
	}
	return na != nb && !strings.Contains(na, nb) && !strings.Contains(nb, na)
}

func normalizeModelID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.TrimSuffix(id, "[1m]")
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	return id
}
