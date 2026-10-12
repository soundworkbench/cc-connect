package core

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type closedInputFailureSession struct {
	*controllableAgentSession
	failure *InputWriteError
}

func (s *closedInputFailureSession) InputFailure() *InputWriteError { return s.failure }
func (*closedInputFailureSession) Close() error                     { return nil }

func TestProcessInteractiveEvents_ClosedStreamReportsControlWriteFailure(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	defer e.Stop()
	as := &closedInputFailureSession{
		controllableAgentSession: newControllableSession("input-control-error"),
		failure:                  &InputWriteError{WrittenBytes: 7, TotalBytes: 100, Err: errors.New("broken pipe")},
	}
	as.alive = false
	close(as.events)
	key := "test:input-control-error"
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
	e.interactiveStates[key] = state
	sent := make(chan error, 1)
	sent <- nil
	e.processInteractiveEvents(state, e.sessions.GetOrCreateActive(key), e.sessions, key, "m1", time.Now(), nil, sent, "ctx")
	if !strings.Contains(strings.Join(p.getSent(), "\n"), "请重发上一条消息") {
		t.Fatalf("closed control stream must tell user to resend: %v", p.getSent())
	}
}
