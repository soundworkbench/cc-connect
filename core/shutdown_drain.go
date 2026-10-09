package core

import (
	"sync"
	"time"
)

// Admission and draining share the lifecycle lock. Child work reserves its
// ticket before launching, so there is no zero-count scheduling gap.
func (e *Engine) trackWork(continuation bool) (func(), bool) {
	e.platformLifecycleMu.Lock()
	defer e.platformLifecycleMu.Unlock()
	if e.ctx.Err() != nil || (e.stopping && (!continuation || e.activeWork == 0)) {
		return nil, false
	}
	if e.activeWork == 0 {
		e.workIdle = make(chan struct{})
	}
	e.activeWork++
	var once sync.Once
	return func() {
		once.Do(func() {
			e.platformLifecycleMu.Lock()
			defer e.platformLifecycleMu.Unlock()
			e.activeWork--
			if e.activeWork == 0 {
				close(e.workIdle)
			}
		})
	}, true
}

func (e *Engine) launchAcceptedWork(fn func()) bool {
	done, ok := e.trackWork(true)
	if !ok {
		return false
	}
	go func() { defer done(); fn() }()
	return true
}

func (e *Engine) waitForAcceptedWork(timeout time.Duration) bool {
	e.platformLifecycleMu.Lock()
	n, idle := e.activeWork, e.workIdle
	e.platformLifecycleMu.Unlock()
	if n == 0 {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

func (e *Engine) notifyShutdownInterrupted() {
	type target struct {
		platform Platform
		replyCtx any
	}
	var targets []target
	e.interactiveMu.Lock()
	for _, state := range e.interactiveStates {
		state.mu.Lock()
		if state.currentTurnUserMessageTimeMs > 0 || len(state.pendingMessages) > 0 {
			targets = append(targets, target{state.platform, state.replyCtx})
		}
		state.mu.Unlock()
	}
	e.interactiveMu.Unlock()
	for _, t := range targets {
		if t.platform != nil {
			e.reply(t.platform, t.replyCtx, e.i18n.T(MsgShutdownInterrupted))
		}
	}
}
