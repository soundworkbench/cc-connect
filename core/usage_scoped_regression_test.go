package core

import (
	"strings"
	"testing"
)

func scopedUsageFixture() *UsageReport {
	return &UsageReport{Buckets: []UsageBucket{
		{Name: "Usage", Windows: []UsageWindow{{WindowSeconds: 18000, UsedPercent: 28}, {WindowSeconds: 604800, UsedPercent: 60}}},
		{Name: "Fable", Windows: []UsageWindow{{WindowSeconds: 604800, UsedPercent: 66}}},
	}}
}

func TestUsageScopedQuotaIsNotDropped(t *testing.T) {
	for _, lang := range []Language{LangEnglish, LangChinese, LangTraditionalChinese, LangJapanese, LangSpanish} {
		got := formatUsageReport(scopedUsageFixture(), lang)
		if !strings.Contains(got, "Fable") || !strings.Contains(got, "34%") || !strings.Contains(got, "40%") {
			t.Fatalf("%s drops independent model quota: %s", lang, got)
		}
	}
}

func TestUsageScopedQuotaCard(t *testing.T) {
	p := &stubCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	a := &stubUsageAgent{report: scopedUsageFixture()}
	e := NewEngine("test", a, []Platform{p}, "", LangChinese)
	defer e.Stop()
	e.handleCommand(p, &Message{SessionKey: "test:user", ReplyCtx: "ctx"}, "/usage")
	if len(p.repliedCards) != 1 {
		t.Fatal("usage card absent")
	}
	got := p.repliedCards[0].RenderText()
	if !strings.Contains(got, "Fable · 7日限额") || !strings.Contains(got, "剩余：34%") {
		t.Fatalf("model quota absent from card: %s", got)
	}
}
