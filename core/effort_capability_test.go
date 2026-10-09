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
	if text := e.renderReasoningCard().RenderText(); !strings.Contains(text, e.i18n.T(MsgReasoningNotSupported)) {
		t.Fatalf("card=%s", text)
	}
}
