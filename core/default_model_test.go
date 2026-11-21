package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// defaultModelAgent mirrors the Claude Code agent's precedence: the active
// provider's model wins over the agent model when a session starts.
type defaultModelAgent struct {
	cujAgent
	modelMu   sync.Mutex
	model     string
	providers []ProviderConfig
	active    int
	delayMs   int
}

func (a *defaultModelAgent) Name() string { return "default-model-test" }

func (a *defaultModelAgent) SetModel(m string) { a.modelMu.Lock(); a.model = m; a.modelMu.Unlock() }

func (a *defaultModelAgent) GetModel() string {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	return GetProviderModel(a.providers, a.active, a.model)
}

func (a *defaultModelAgent) AvailableModels(context.Context) []ModelOption {
	return []ModelOption{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}}
}

func (a *defaultModelAgent) WorkspaceAgentOptions() map[string]any {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	return map[string]any{"model": a.model}
}

func (a *defaultModelAgent) SetProviders(p []ProviderConfig) {
	a.modelMu.Lock()
	a.providers = append([]ProviderConfig(nil), p...)
	a.modelMu.Unlock()
}

func (a *defaultModelAgent) ListProviders() []ProviderConfig {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	return append([]ProviderConfig(nil), a.providers...)
}

func (a *defaultModelAgent) GetActiveProvider() *ProviderConfig {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	if a.active < 0 || a.active >= len(a.providers) {
		return nil
	}
	p := a.providers[a.active]
	return &p
}

func (a *defaultModelAgent) SetActiveProvider(name string) bool {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	for i, p := range a.providers {
		if p.Name == name {
			a.active = i
			return true
		}
	}
	a.active = -1
	return name == ""
}

func (a *defaultModelAgent) StartSession(ctx context.Context, id string) (AgentSession, error) {
	s, err := a.cujAgent.StartSession(ctx, id)
	if err != nil {
		return nil, err
	}
	cs := s.(*cujAgentSession)
	a.modelMu.Lock()
	delay := a.delayMs
	a.modelMu.Unlock()
	cs.mu.Lock()
	cs.reply = "MODEL=" + a.GetModel()
	cs.delayMs = delay
	cs.mu.Unlock()
	return cs, nil
}

func init() {
	RegisterAgent("default-model-test", func(opts map[string]any) (Agent, error) {
		model, _ := opts["model"].(string)
		return &defaultModelAgent{model: model, active: -1}, nil
	})
}

func newDefaultModelAgent() *defaultModelAgent {
	return &defaultModelAgent{
		model:     "alpha",
		providers: []ProviderConfig{{Name: "wp", Model: "alpha"}},
		active:    0,
	}
}

func callModelAPI(t *testing.T, api *APIServer, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	rec := httptest.NewRecorder()
	api.handleModel(rec, httptest.NewRequest(method, target, reader))
	return rec
}

func decodeModel(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out["model"]
}

func TestModelAPI_SwitchFollowsDefaultAndKeepsTopicChoice(t *testing.T) {
	agent := newDefaultModelAgent()
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	defer e.Stop()
	var saved []string
	e.SetProviderModelSaveFunc(func(provider, model string) error {
		saved = append(saved, provider+"="+model)
		return nil
	})
	env := &cujEnv{t: t, engine: e, plat: p}
	send := func(key, content string, isolated bool) string {
		t.Helper()
		p.clearSent()
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", UserID: "user", Content: content, ReplyCtx: key, IsolateAgent: isolated})
		env.waitFor(content, 2*time.Second, func() bool { return len(p.getSent()) > 0 })
		out := strings.Join(p.getSent(), "\n")
		env.waitFor("idle "+key, 2*time.Second, func() bool { return !e.sessions.GetOrCreateActive(key).Busy() })
		return out
	}
	const topicA, topicB, chat = "test:chat:root:a", "test:chat:root:b", "test:chat:user"

	if got := send(topicA, "/model switch beta", true); !strings.Contains(got, "beta") {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		key, want string
		isolate   bool
	}{{topicA, "MODEL=beta", true}, {topicB, "MODEL=alpha", true}, {chat, "MODEL=alpha", false}} {
		if got := send(tc.key, "which model", tc.isolate); !strings.Contains(got, tc.want) {
			t.Fatalf("before switch %s: %s", tc.key, got)
		}
	}

	api := &APIServer{engines: map[string]*Engine{"test": e}}
	if got := decodeModel(t, callModelAPI(t, api, http.MethodPost, "/model", ModelRequest{Project: "test", Model: " gamma "})); got != "gamma" {
		t.Fatalf("POST model = %q", got)
	}
	if got := decodeModel(t, callModelAPI(t, api, http.MethodGet, "/model?project=test", nil)); got != "gamma" {
		t.Fatalf("GET model = %q", got)
	}
	if strings.Join(saved, ",") != "wp=gamma" {
		t.Fatalf("saved = %v", saved)
	}

	for _, tc := range []struct {
		key, want string
		isolate   bool
	}{{topicA, "MODEL=beta", true}, {topicB, "MODEL=gamma", true}, {chat, "MODEL=gamma", false}} {
		if got := send(tc.key, "which model", tc.isolate); !strings.Contains(got, tc.want) {
			t.Fatalf("after switch %s: %s", tc.key, got)
		}
	}
}

func TestModelAPI_RunningTurnIsNotInterrupted(t *testing.T) {
	agent := newDefaultModelAgent()
	agent.delayMs = 300
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	defer e.Stop()
	env := &cujEnv{t: t, engine: e, plat: p}
	const key = "test:chat:user"

	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", UserID: "user", Content: "long task", ReplyCtx: key})
	env.waitFor("turn started", 2*time.Second, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return len(agent.sessions) == 1
	})
	if _, err := e.SetDefaultModel("gamma"); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	running := agent.sessions[0]
	agent.mu.Unlock()
	agent.modelMu.Lock()
	agent.delayMs = 0
	agent.modelMu.Unlock()

	env.waitFor("running turn reply", 2*time.Second, func() bool { return strings.Contains(strings.Join(p.getSent(), "\n"), "MODEL=alpha") })
	if running.closed.Get() {
		t.Fatal("running turn's agent session was closed by the model switch")
	}
	env.waitFor("idle", 2*time.Second, func() bool { return !e.sessions.GetOrCreateActive(key).Busy() })

	p.clearSent()
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", UserID: "user", Content: "next", ReplyCtx: key})
	env.waitFor("next reply", 2*time.Second, func() bool { return len(p.getSent()) > 0 })
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "MODEL=gamma") {
		t.Fatalf("next turn: %s", got)
	}
	if !running.closed.Get() {
		t.Fatal("old agent process should be recycled at the next turn")
	}
}

func TestModelAPI_PersistsAgentModelWithoutProvider(t *testing.T) {
	agent := &stubModelModeAgent{model: "alpha"}
	e := NewEngine("test", agent, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	var saved []string
	e.SetModelSaveFunc(func(model string) error {
		saved = append(saved, model)
		return nil
	})
	api := &APIServer{engines: map[string]*Engine{"test": e}}

	if got := decodeModel(t, callModelAPI(t, api, http.MethodPost, "/model", ModelRequest{Model: "codex/gpt-6.1-sol"})); got != "codex/gpt-6.1-sol" {
		t.Fatalf("POST model = %q", got)
	}
	if agent.model != "codex/gpt-6.1-sol" || strings.Join(saved, ",") != "codex/gpt-6.1-sol" {
		t.Fatalf("agent model = %q, saved = %v", agent.model, saved)
	}
}

func TestModelAPI_SaveFailureKeepsModel(t *testing.T) {
	agent := newDefaultModelAgent()
	e := NewEngine("test", agent, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	e.SetProviderModelSaveFunc(func(string, string) error { return errors.New("disk full") })
	api := &APIServer{engines: map[string]*Engine{"test": e}}

	rec := callModelAPI(t, api, http.MethodPost, "/model", ModelRequest{Project: "test", Model: "gamma"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := agent.GetModel(); got != "alpha" {
		t.Fatalf("model after failed save = %q", got)
	}
}

func TestModelAPI_Errors(t *testing.T) {
	modelEngine := NewEngine("a", &stubModelModeAgent{model: "alpha"}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	plainEngine := NewEngine("plain", &stubAgent{}, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
	api := &APIServer{engines: map[string]*Engine{"a": modelEngine, "plain": plainEngine}}

	for _, tc := range []struct {
		name, method, target string
		body                 any
		want                 int
	}{
		{"unknown project", http.MethodPost, "/model", ModelRequest{Project: "nope", Model: "x"}, http.StatusNotFound},
		{"unknown project GET", http.MethodGet, "/model?project=nope", nil, http.StatusNotFound},
		{"empty model", http.MethodPost, "/model", ModelRequest{Project: "a"}, http.StatusBadRequest},
		{"blank model", http.MethodPost, "/model", ModelRequest{Project: "a", Model: "  "}, http.StatusBadRequest},
		{"bad JSON", http.MethodPost, "/model", "{", http.StatusBadRequest},
		{"project required", http.MethodPost, "/model", ModelRequest{Model: "x"}, http.StatusBadRequest},
		{"no model switching", http.MethodPost, "/model", ModelRequest{Project: "plain", Model: "x"}, http.StatusBadRequest},
		{"method", http.MethodPut, "/model", nil, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := callModelAPI(t, api, tc.method, tc.target, tc.body); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if modelEngine.agent.(*stubModelModeAgent).model != "alpha" {
		t.Fatal("rejected requests must not change the model")
	}
}

type modelPagePlatform struct {
	stubPlatformEngine
	link string
}

func (p *modelPagePlatform) ModelPageLink() string { return p.link }

func modelCardButtons(card *Card) []CardButton {
	var out []CardButton
	for _, el := range card.Elements {
		if a, ok := el.(CardActions); ok {
			out = append(out, a.Buttons...)
		}
	}
	return out
}

func TestModelCard_ModelPageButton(t *testing.T) {
	const link = "https://applink.example.test/open?url=x"
	for _, tc := range []struct {
		name     string
		platform Platform
		want     string
	}{
		{"configured", &modelPagePlatform{stubPlatformEngine{n: "feishu"}, link}, link},
		{"not configured", &modelPagePlatform{stubPlatformEngine{n: "feishu"}, ""}, ""},
		{"platform without page", &stubPlatformEngine{n: "feishu"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine("test", &stubModelModeAgent{model: "gpt-4.1"}, []Platform{tc.platform}, "", LangChinese)
			buttons := modelCardButtons(e.renderModelCard("feishu:oc_1:ou_1"))
			var urls []string
			for _, b := range buttons {
				if b.URL != "" {
					urls = append(urls, b.URL)
				}
			}
			if tc.want == "" {
				if len(urls) != 0 {
					t.Fatalf("link buttons = %v, want none", urls)
				}
				return
			}
			if len(buttons) != 2 || buttons[0].URL != tc.want || buttons[0].Text != "模型" || buttons[0].Value != "" || buttons[1].Value != "nav:/help" {
				t.Fatalf("buttons = %+v", buttons)
			}
		})
	}
}
