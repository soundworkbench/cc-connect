package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCUJ_UsageInitializationErrorDoesNotBreakNextCommand(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	a := &stubUsageAgent{err: &UserMessageError{Key: MsgUsageOnboardingIncomplete}}
	e := NewEngine("test", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	defer e.Stop()
	for _, command := range []string{"/usage", "/help", "/usage"} {
		e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: "test", UserID: "user", Content: command, ReplyCtx: context.Background()})
	}
	sent := p.getSent()
	if len(sent) != 3 || !strings.Contains(sent[0], "首次初始化") || !strings.Contains(sent[2], "首次初始化") {
		t.Fatalf("unexpected replies: %v", sent)
	}
	for _, lang := range []Language{LangEnglish, LangChinese, LangTraditionalChinese, LangJapanese, LangSpanish} {
		if value := (&UserMessageError{Key: MsgUsageOnboardingIncomplete}).Localize(lang); value == "" || value == string(MsgUsageOnboardingIncomplete) {
			t.Fatalf("missing translation for %s", lang)
		}
	}
}

func TestTurnFailureMessages_AllLanguages(t *testing.T) {
	for _, key := range []MsgKey{MsgAgentTurnFailed, MsgAgentTurnInterrupted} {
		for _, lang := range []Language{LangEnglish, LangChinese, LangTraditionalChinese, LangJapanese, LangSpanish} {
			value := (&UserMessageError{Key: key, Args: []any{": UPSTREAM_ERROR"}}).Localize(lang)
			if !strings.Contains(value, "UPSTREAM_ERROR") || strings.Contains(value, string(key)) || strings.Contains(value, "%!") {
				t.Fatalf("missing or invalid translation for %s/%s", key, lang)
			}
		}
	}
}
