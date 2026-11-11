package core

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bgWorkSession is an agent session whose CLI still runs a background task
// after the turn's result while bg is set.
type bgWorkSession struct {
	*controllableAgentSession
	bg atomic.Bool
}

func (s *bgWorkSession) HasBackgroundWork() bool { return s.bg.Load() }

func newBgWorkSession(id string) *bgWorkSession {
	s := &bgWorkSession{controllableAgentSession: newControllableSession(id)}
	s.bg.Store(true)
	return s
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestIdleReclaim_KeepsSessionWithBackgroundWork(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	as := newBgWorkSession("idle-bg")
	key := "test:idle-bg"
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx", agentSessionIdleToken: 7}
	e.interactiveStates[key] = state

	e.cleanupInteractiveStateForIdleToken(key, state, 7, time.Minute)
	if isClosed(as.closed) {
		t.Fatal("idle reclaim closed a session whose background task is still running")
	}
	if e.interactiveStates[key] != state {
		t.Fatal("idle reclaim dropped the state of a session with background work")
	}

	as.bg.Store(false)
	e.cleanupInteractiveStateForIdleToken(key, state, 7, time.Minute)
	select {
	case <-as.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("idle session without background work was not reclaimed")
	}
}

func TestAwaitBackgroundWork_WaitsForTasksThenStopsAtLimit(t *testing.T) {
	old := backgroundWorkPoll
	backgroundWorkPoll = 5 * time.Millisecond
	defer func() { backgroundWorkPoll = old }()

	e := NewEngine("test", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	defer e.Stop()
	as := newBgWorkSession("cron-bg")
	key := "test:cron#cron:s1"
	e.interactiveStates[key] = &interactiveState{agentSession: as}

	go func() {
		time.Sleep(100 * time.Millisecond)
		as.bg.Store(false)
	}()
	start := time.Now()
	e.awaitBackgroundWork(key, 5*time.Second)
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("awaitBackgroundWork returned after %v; want it to wait for the task to end", elapsed)
	}

	as.bg.Store(true)
	start = time.Now()
	e.awaitBackgroundWork(key, 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("awaitBackgroundWork ignored its limit (%v)", elapsed)
	}
}

func TestProcessInteractiveEvents_LostPermissionChannelResumesOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retried bool
		want    MsgKey
	}{
		{"first loss resumes", false, MsgPermissionChannelRetrying},
		{"second loss tells the user", true, MsgPermissionChannelLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
			defer e.Stop()
			as := &closedInputFailureSession{
				controllableAgentSession: newControllableSession("perm-channel"),
				failure:                  &InputWriteError{Err: ErrPermissionChannelClosed},
			}
			as.alive = false
			close(as.events)
			key := "test:perm-channel"
			state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx",
				turnInput: &Message{SessionKey: key, Platform: "test", UserID: "u1", Content: "整理周报", ReplyCtx: "ctx", channelRetry: tc.retried}}
			e.interactiveStates[key] = state
			sent := make(chan error, 1)
			sent <- nil
			e.processInteractiveEvents(state, e.sessions.GetOrCreateActive(key), e.sessions, key, "m1", time.Now(), nil, sent, "ctx", 0)

			all := strings.Join(p.getSent(), "\n")
			if !strings.Contains(all, e.i18n.T(tc.want)) {
				t.Fatalf("sent %q, want %q", all, e.i18n.T(tc.want))
			}
			if strings.Contains(all, "Stream closed") || strings.Contains(all, "permission channel") {
				t.Fatalf("raw English failure reached the user: %q", all)
			}
		})
	}
}
