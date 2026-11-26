package core

import (
	"strings"
	"testing"
)

type noEffortAgent struct{ stubModelModeAgent }

func (*noEffortAgent) AvailableReasoningEfforts() []string { return nil }
func TestReasoningUnsupportedModel_NoEmptyPicker(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	a := &noEffortAgent{}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	e.cmdReasoning(p, &Message{SessionKey: "test:effort", ReplyCtx: "ctx"}, nil)
	if len(p.sent) != 1 || p.sent[0] != e.i18n.T(MsgReasoningNotSupported) {
		t.Fatalf("reply=%v", p.sent)
	}
	if text := e.renderReasoningCard(e.agent).RenderText(); !strings.Contains(text, e.i18n.T(MsgReasoningNotSupported)) {
		t.Fatalf("card=%s", text)
	}
}

type effortAgent struct {
	stubModelModeAgent
	effort string
	keep   bool
}

func (a *effortAgent) AvailableReasoningEfforts() []string { return []string{"low", "high"} }
func (a *effortAgent) GetReasoningEffort() string          { return a.effort }
func (a *effortAgent) SetReasoningEffort(v string)         { a.effort = v }

type keepingEffortAgent struct{ effortAgent }

func (*keepingEffortAgent) KeepsSessionOnReasoningChange() bool { return true }

func reasoningChangeLeavesAgentSession(t *testing.T, a Agent) string {
	t.Helper()
	p := &stubPlatformEngine{n: "plain"}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	key := "test:effort-keep"
	s := e.sessions.GetOrCreateActive(key)
	s.SetAgentSessionID("agent-1", "test")
	e.cmdReasoning(p, &Message{SessionKey: key, ReplyCtx: "ctx"}, []string{"low"})
	return e.sessions.GetOrCreateActive(key).GetAgentSessionID()
}

func TestReasoningChange_DefaultStartsFreshConversation(t *testing.T) {
	if got := reasoningChangeLeavesAgentSession(t, &effortAgent{}); got != "" {
		t.Fatalf("agent session = %q, want reset", got)
	}
}

func TestReasoningChange_KeepingAgentKeepsConversation(t *testing.T) {
	a := &keepingEffortAgent{}
	if got := reasoningChangeLeavesAgentSession(t, a); got != "agent-1" {
		t.Fatalf("agent session = %q, want kept", got)
	}
	if a.effort != "low" {
		t.Fatalf("effort = %q, want low", a.effort)
	}
}
