package core

import (
	"strings"
	"testing"
	"time"
)

func TestResolveModelAlias_CaseInsensitive(t *testing.T) {
	models := []ModelOption{{Name: "gpt-5.3-codex", Alias: "Codex"}}

	got := resolveModelAlias(models, "codex")
	if got != "gpt-5.3-codex" {
		t.Fatalf("resolveModelAlias() = %q, want %q", got, "gpt-5.3-codex")
	}
}

func TestResolveModelAlias_NoMatchFallsBackToInput(t *testing.T) {
	models := []ModelOption{{Name: "gpt-5.3-codex", Alias: "codex"}}

	got := resolveModelAlias(models, "gpt-5.4")
	if got != "gpt-5.4" {
		t.Fatalf("resolveModelAlias() = %q, want original input", got)
	}
}

func TestParseModelSwitchArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
		ok   bool
	}{
		{name: "legacy syntax", args: []string{"gpt"}, want: "gpt", ok: true},
		{name: "switch syntax", args: []string{"switch", "gpt"}, want: "gpt", ok: true},
		{name: "missing switch target", args: []string{"switch"}, ok: false},
		{name: "unknown subcommand", args: []string{"list", "gpt"}, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseModelSwitchArgs(tt.args)
			if ok != tt.ok {
				t.Fatalf("parseModelSwitchArgs() ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("parseModelSwitchArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func claudeAliasAgent() *stubStrictModelAgent {
	return &stubStrictModelAgent{
		stubModelModeAgent: stubModelModeAgent{model: "claude-opus-5-5"},
		models: []ModelOption{
			{Name: "claude-opus-5-5", Alias: "Claude Opus 5.5"},
			{Name: "claude-fable-5-2", Alias: "Claude Fable 5.2"},
			{Name: "haiku"},
		},
	}
}

func assertAliasText(t *testing.T, what, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Fatalf("%s missing %q:\n%s", what, w, text)
		}
	}
	for _, id := range []string{"claude-opus-5-5", "claude-fable-5-2"} {
		if strings.Contains(text, id) {
			t.Fatalf("%s shows raw model id %q:\n%s", what, id, text)
		}
	}
}

func TestModelCard_ShowsAliasNotModelID(t *testing.T) {
	agent := claudeAliasAgent()
	e := NewEngine("test", agent, []Platform{&stubPlatformEngine{n: "feishu"}}, "", LangEnglish)

	card := e.renderModelCard("")
	assertAliasText(t, "model card", card.RenderText(),
		"Current model: Claude Opus 5.5", "Claude Fable 5.2", "haiku")

	var values []string
	for _, el := range card.Elements {
		if sel, ok := el.(CardSelect); ok {
			for _, o := range sel.Options {
				values = append(values, o.Value)
			}
		}
	}
	if len(values) != 3 || values[1] != "act:/model switch 2" {
		t.Fatalf("select values = %v, want index-based switch values", values)
	}
}

func TestModelCardSwitch_ResultShowsAliasAndPassesModelID(t *testing.T) {
	agent := claudeAliasAgent()
	e := NewEngine("test", agent, []Platform{&stubPlatformEngine{n: "plain"}}, "", LangEnglish)

	card := e.handleCardNav("act:/model switch 2", "plain:channel1:user1")
	if card == nil {
		t.Fatal("expected result card")
	}
	assertAliasText(t, "switch result card", card.RenderText(), "Model switched to `Claude Fable 5.2`.")
	if agent.model != "claude-fable-5-2" {
		t.Fatalf("agent model = %q, want claude-fable-5-2", agent.model)
	}

	assertAliasText(t, "switching card", e.renderModelSwitchingCard(modelDisplayName(agent.models, "claude-opus-5-5")).RenderText(),
		"Switching model to `Claude Opus 5.5`")
}

func TestCmdModel_TextListShowsAlias(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	agent := claudeAliasAgent()
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	e.cmdModel(p, &Message{SessionKey: "plain:user1", ReplyCtx: "ctx"}, nil)
	e.cmdModel(p, &Message{SessionKey: "plain:user1", ReplyCtx: "ctx"}, []string{"switch", "Claude Fable 5.2"})

	assertAliasText(t, "/model replies", strings.Join(p.getSent(), "\n---\n"),
		"Current model: Claude Opus 5.5", "> 1. Claude Opus 5.5", "2. Claude Fable 5.2", "Model switched to `Claude Fable 5.2`")
	if agent.model != "claude-fable-5-2" {
		t.Fatalf("agent model = %q, want claude-fable-5-2", agent.model)
	}
}

func TestModelDisplayName_FallsBackToName(t *testing.T) {
	models := []ModelOption{{Name: "claude-opus-5-5", Alias: "Claude Opus 5.5"}, {Name: "haiku"}}
	for in, want := range map[string]string{"claude-opus-5-5": "Claude Opus 5.5", "haiku": "haiku", "sonnet": "sonnet"} {
		if got := modelDisplayName(models, in); got != want {
			t.Fatalf("modelDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

// aliasFooterAgent is an agent whose configured model list carries aliases.
type aliasFooterAgent struct {
	stubFooterAgent
	models []ModelOption
}

func (a *aliasFooterAgent) ConfiguredModels() []ModelOption { return a.models }

func newAliasFooterAgent() *aliasFooterAgent {
	return &aliasFooterAgent{
		stubFooterAgent: stubFooterAgent{model: "claude-opus-5-5", effort: "xhigh", workDir: "/tmp/ws"},
		models:          claudeAliasAgent().models,
	}
}

func TestReplyFooter_ShowsAliasNotModelID(t *testing.T) {
	e := newLegacyFooterEngine()
	e.i18n = NewI18n(LangEnglish)
	got := e.buildReplyFooter(newAliasFooterAgent(), nil, "/tmp/ws", "100% left")
	assertAliasText(t, "reply footer", got, "Claude Opus 5.5 · xhigh")
}

func TestStatusLineFooter_ShowsAliasNotModelID(t *testing.T) {
	session := &controllableAgentSession{
		model:   "claude-fable-5-2",
		workDir: "/tmp/ws",
		contextUsage: &ContextUsage{
			InputTokens: 1, OutputTokens: 168, CacheCreationInputTokens: 971,
			CachedInputTokens: 40800, ContextWindow: 1_000_000, UsedTokens: 41772,
		},
	}
	e := newClaudeFooterEngine()
	e.i18n = NewI18n(LangEnglish)
	agent := newAliasFooterAgent()
	assertAliasText(t, "status line", e.buildClaudeStatusLineFooter(agent, session, "/tmp/ws"), "Claude Fable 5.2 · ")
	assertAliasText(t, "rich card footer", e.composeRichStatusFooter(false, time.Now(), agent, session, "/tmp/ws"), "Claude Fable 5.2 · ")
}

func TestFooter_UnaliasedModelKeepsID(t *testing.T) {
	e := newLegacyFooterEngine()
	e.i18n = NewI18n(LangEnglish)
	agent := newAliasFooterAgent()
	agent.model = "haiku"
	if got := e.buildReplyFooter(agent, nil, "/tmp/ws", "100% left"); !strings.Contains(got, "haiku · xhigh") {
		t.Fatalf("footer = %q, want the id for a model without an alias", got)
	}
}

func TestModelSwitchNotice_ShowsAliasKeepsCommandID(t *testing.T) {
	agent := newAliasFooterAgent()
	e := NewEngine("test", agent, nil, "", LangEnglish)
	ms := &ModelSwitch{From: "claude-opus-5-5", To: "claude-fable-5-2", Category: "cyber", SessionWide: true}
	got := e.modelSwitchNotice(ms)
	for _, w := range []string{"`Claude Opus 5.5`'s safeguards", "`Claude Fable 5.2` is answering", "`/model claude-opus-5-5`"} {
		if !strings.Contains(got, w) {
			t.Fatalf("notice missing %q:\n%s", w, got)
		}
	}
	ms.SessionWide = false
	got = e.modelSwitchNotice(ms)
	if strings.Contains(got, "claude-") || strings.Contains(got, "EXTRA") {
		t.Fatalf("per-turn notice shows a raw id or stray argument:\n%s", got)
	}
}
