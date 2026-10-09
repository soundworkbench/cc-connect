package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type steerReactionPlatform struct {
	stubPlatformEngine
	reactions chan string
}

func (p *steerReactionPlatform) StartSteerReaction(_ context.Context, rc any) func(bool) {
	p.reactions <- "received"
	return func(done bool) {
		if done {
			p.reactions <- "done"
		} else {
			p.reactions <- "removed"
		}
	}
}

type asyncSteerTestSession struct {
	*steeringJourneySession
	ack      chan struct{}
	end      chan struct{}
	writeErr error
}

func (s *asyncSteerTestSession) SteerAsync(prompt, id string, images []ImageAttachment, files []FileAttachment) (*SteerReceipt, error) {
	s.steered <- prompt
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	return &SteerReceipt{Ack: s.ack, End: s.end}, nil
}
func TestSteerReactionLateAckAndCodexCompatibility(t *testing.T) {
	for _, async := range []bool{true, false} {
		t.Run(map[bool]string{true: "async", false: "codex-sync"}[async], func(t *testing.T) {
			p := &steerReactionPlatform{reactions: make(chan string, 8)}
			s := &steeringJourneySession{cujAgentSession: newCUJAgentSession(), steered: make(chan string, 8), accept: true}
			a := &asyncSteerTestSession{steeringJourneySession: s, ack: make(chan struct{}), end: make(chan struct{})}
			e := NewEngine("steering", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
			defer e.cancel()
			state := &interactiveState{agentSession: s}
			if async {
				state.agentSession = a
			}
			session := e.sessions.GetOrCreateActive("key")
			msg := &Message{MessageID: "m2", Content: "supplement"}
			if !e.trySteerMessageLocked(state, p, msg, session, e.sessions) {
				t.Fatal("not admitted")
			}
			if !e.trySteerMessageLocked(state, p, msg, session, e.sessions) || len(s.steered) != 1 {
				t.Fatal("duplicate input")
			}
			take := func(want string) {
				t.Helper()
				select {
				case got := <-p.reactions:
					if got != want {
						t.Fatalf("%s != %s", got, want)
					}
				case <-time.After(time.Second):
					t.Fatal("reaction missing")
				}
			}
			take("received")
			if async {
				select {
				case got := <-p.reactions:
					t.Fatalf("premature %s", got)
				case <-time.After(3200 * time.Millisecond):
				}
				close(a.ack)
			}
			take("done")
			if len(p.getSent()) != 0 {
				t.Fatalf("text feedback: %v", p.getSent())
			}
		})
	}
}
func TestSteerReactionWriteFailureQueuesOnce(t *testing.T) {
	p := &steerReactionPlatform{reactions: make(chan string, 8)}
	s := &asyncSteerTestSession{steeringJourneySession: &steeringJourneySession{cujAgentSession: newCUJAgentSession(), steered: make(chan string, 8)}, writeErr: ErrSteerNotWritten}
	e := NewEngine("steering", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	defer e.cancel()
	state := &interactiveState{agentSession: s}
	e.interactiveStates["key"] = state
	session := e.sessions.GetOrCreateActive("key")
	msg := &Message{MessageID: "m2", Content: "supplement"}
	for i := 0; i < 2; i++ {
		if !e.deliverMessageForBusySession(p, msg, "key", session, e.sessions) {
			t.Fatal("queue failed")
		}
	}
	if len(state.pendingMessages) != 1 || len(s.steered) != 1 {
		t.Fatal("duplicate queue/write")
	}
	sent := p.getSent()
	if len(sent) != 1 || sent[0] != e.i18n.T(MsgMessageSteerWriteFailed) {
		t.Fatalf("notice=%v", sent)
	}
}
func TestSteerReactionProcessExitWithoutAck(t *testing.T) {
	p := &steerReactionPlatform{reactions: make(chan string, 8)}
	e := &Engine{ctx: context.Background()}
	end := make(chan struct{})
	e.steerReaction(p, nil, &SteerReceipt{Ack: make(chan struct{}), End: end})
	<-p.reactions
	close(end)
	select {
	case got := <-p.reactions:
		if got != "removed" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("reaction leaked")
	}
}
