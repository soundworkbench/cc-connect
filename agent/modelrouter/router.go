// Package modelrouter runs one conversation on two agent CLIs ("shells") and
// picks the shell by model: Claude models run in Claude Code, every other
// model in an ACP agent (e.g. Antigravity, `agy acp`). Each shell keeps its
// own session; when the model moves the conversation to the other shell, the
// first user message there starts with what happened in the other shell
// (see handoff.go).
package modelrouter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const (
	shellClaude = "claudecode"
	shellACP    = "acp"
)

func init() {
	core.RegisterAgent("model_router", New)
}

// defaultClaudeModels are the model ids (glob, case-insensitive) that run in
// Claude Code. Everything else runs in the ACP agent.
var defaultClaudeModels = []string{"claude*", "opus*", "sonnet*", "haiku*", "fable*"}

// sharedKeys are options of the router that both shells get unless their own
// table sets them.
var sharedKeys = []string{"work_dir", "run_as_user", "run_as_env", "cc_data_dir", "cc_project", "language"}

// Agent routes sessions to the shell of the current model.
type Agent struct {
	shells     map[string]core.Agent
	names      map[string]string
	patterns   []string
	store      *store
	state      map[string]any // cc_data_dir, cc_project: where the store lives
	handoffMax int            // size limit of a handoff, in characters

	mu        sync.Mutex
	providers []core.ProviderConfig
	activeIdx int
	model     string
	lastModel map[string]string // shell -> the model it last ran
	applied   map[string]string // shell -> the model last handed to it
}

// New builds a model_router agent. Options:
//
//	model          initial model (optional)
//	claude_models  globs of model ids that run in Claude Code (optional)
//	handoff_max_chars  size limit of the handoff, in characters (default 20000)
//	[claudecode]   options of the Claude Code agent, as for type "claudecode"
//	               (reasoning_effort seeds --effort)
//	[acp]          options of the ACP agent, as for type "acp"
//	               (reasoning_effort seeds thought_level when the agent has one)
//
// work_dir, run_as_user and run_as_env apply to both shells.
func New(opts map[string]any) (core.Agent, error) {
	ccOpts, err := shellOpts(opts, shellClaude)
	if err != nil {
		return nil, err
	}
	acpOpts, err := shellOpts(opts, shellACP)
	if err != nil {
		return nil, err
	}
	if v, ok := opts["stream_partial_text"]; ok {
		if _, set := ccOpts["stream_partial_text"]; !set {
			ccOpts["stream_partial_text"] = v
		}
	}
	cc, err := core.CreateAgent(shellClaude, ccOpts)
	if err != nil {
		return nil, fmt.Errorf("model_router: claudecode: %w", err)
	}
	acp, err := core.CreateAgent(shellACP, acpOpts)
	if err != nil {
		return nil, fmt.Errorf("model_router: acp: %w", err)
	}
	dataDir, _ := opts["cc_data_dir"].(string)
	project, _ := opts["cc_project"].(string)
	dir := ""
	if dataDir != "" {
		dir = filepath.Join(dataDir, "model_router", safeName(project))
	}
	model, _ := opts["model"].(string)
	r := newRouter(cc, acp, stringList(opts["claude_models"]), strings.TrimSpace(model), openStore(dir))
	r.state = map[string]any{"cc_data_dir": dataDir, "cc_project": project}
	r.handoffMax = intOpt(opts["handoff_max_chars"], defaultHandoffChars)
	return r, nil
}

func newRouter(cc, acp core.Agent, patterns []string, model string, st *store) *Agent {
	if len(patterns) == 0 {
		patterns = defaultClaudeModels
	}
	for i, p := range patterns {
		patterns[i] = strings.ToLower(strings.TrimSpace(p))
	}
	r := &Agent{
		shells:     map[string]core.Agent{shellClaude: cc, shellACP: acp},
		names:      map[string]string{shellClaude: "Claude Code", shellACP: displayName(acp, "ACP")},
		patterns:   patterns,
		store:      st,
		activeIdx:  -1,
		handoffMax: defaultHandoffChars,
		lastModel:  map[string]string{},
		applied:    map[string]string{},
	}
	if model == "" {
		if ms, ok := cc.(core.ModelSwitcher); ok {
			model = ms.GetModel()
		}
	}
	if model != "" {
		r.model = model
		r.sync()
	}
	return r
}

func shellOpts(opts map[string]any, shell string) (map[string]any, error) {
	out := map[string]any{}
	for _, k := range sharedKeys {
		if v, ok := opts[k]; ok {
			out[k] = v
		}
	}
	raw, ok := opts[shell]
	if !ok {
		return nil, fmt.Errorf("model_router: options table [%s] is required", shell)
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("model_router: options.%s must be a table", shell)
	}
	for k, v := range table {
		out[k] = v
	}
	return out, nil
}

func displayName(a core.Agent, fallback string) string {
	if info, ok := a.(core.AgentDoctorInfo); ok {
		if n := strings.TrimSpace(info.CLIDisplayName()); n != "" {
			return n
		}
	}
	return fallback
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	if s == "" || s == "." || s == ".." {
		return "default"
	}
	return s
}

// intOpt reads a positive integer option (TOML gives int64, JSON float64).
func intOpt(v any, def int) int {
	var n int
	switch x := v.(type) {
	case int:
		n = x
	case int64:
		n = int(x)
	case float64:
		n = int(x)
	}
	if n <= 0 {
		return def
	}
	return n
}

func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(x) != "" {
			return []string{x}
		}
	}
	return nil
}

// shellFor names the shell that runs model.
func (r *Agent) shellFor(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return shellClaude
	}
	base := m
	if i := strings.LastIndex(m, "/"); i >= 0 {
		base = m[i+1:]
	}
	for _, p := range r.patterns {
		if ok, _ := path.Match(p, m); ok {
			return shellClaude
		}
		if ok, _ := path.Match(p, base); ok {
			return shellClaude
		}
	}
	return shellACP
}

func (r *Agent) other(shell string) string {
	if shell == shellClaude {
		return shellACP
	}
	return shellClaude
}

func (r *Agent) Name() string { return "model_router" }

// currentShell is the shell of the current model.
func (r *Agent) currentShell() string { return r.shellFor(r.GetModel()) }

func (r *Agent) current() core.Agent { return r.shells[r.currentShell()] }

func (r *Agent) claude() core.Agent { return r.shells[shellClaude] }

// ── StartSession ─────────────────────────────────────────────

// StartSession starts or resumes the conversation of sessionID on the shell
// of the current model. When the conversation was held by the other shell,
// the first user message gets the handoff; when the shell does not start,
// the conversation goes back to the shell that held it.
func (r *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	th := r.store.threadFor(sessionID, r.ownerOf(ctx, sessionID))
	target := r.currentShell()
	r.store.setActive(th, target)
	snap := r.store.snapshot(th)
	from := snap.Active

	if from == target {
		inner, err := r.shells[target].StartSession(ctx, snap.IDs[target])
		if err != nil {
			return nil, err
		}
		return r.wrap(inner, th, target, nil), nil
	}

	inner, resumed, err := r.startForHandoff(ctx, target, snap.IDs[target])
	if err == nil {
		h := r.handoffFor(ctx, th, from, target, resumed)
		slog.Info("model_router: conversation moves to another shell",
			"from", from, "to", target, "resumed", resumed, "handoff_chars", runeLen(h.text))
		return r.wrap(inner, th, target, h), nil
	}

	slog.Warn("model_router: shell did not start, staying on the previous one",
		"shell", target, "previous", from, "error", err)
	back, berr := r.shells[from].StartSession(ctx, snap.IDs[from])
	if berr != nil {
		return nil, fmt.Errorf("model_router: %s: %w; %s: %v", target, err, from, berr)
	}
	model := r.revertTo(from)
	notice := &core.Notice{Key: core.MsgShellSwitchFailed, Args: []any{r.names[target], r.names[from], model}}
	return r.wrap(back, th, from, &handoff{notice: notice}), nil
}

// startForHandoff resumes id on shell, or starts a new session when there is
// none or it cannot be resumed.
func (r *Agent) startForHandoff(ctx context.Context, shell, id string) (core.AgentSession, bool, error) {
	if id != "" {
		inner, err := r.shells[shell].StartSession(ctx, id)
		if err == nil {
			return inner, true, nil
		}
		slog.Warn("model_router: resume failed, starting a new session", "shell", shell, "session_id", id, "error", err)
	}
	inner, err := r.shells[shell].StartSession(ctx, "")
	return inner, false, err
}

// handoff is what a session owes the conversation on its first user message.
type handoff struct {
	from   string // shell the conversation leaves; "" for a notice only
	text   string
	notice *core.Notice
}

// handoffFor builds the handoff for a move from one shell to another. A
// resumed shell gets what happened in the other shell since it was left; a
// new session gets the thread's recent conversation from both shells.
func (r *Agent) handoffFor(ctx context.Context, th *thread, from, to string, resumed bool) *handoff {
	snap := r.store.snapshot(th)
	all := r.store.transcript(th)
	var entries []entry
	if resumed {
		entries = entriesSince(all, from, snap.Left[to])
	} else {
		entries = all
	}
	if !hasShell(all, from) {
		// Nothing recorded for the shell being left (a conversation from
		// before this router): read its own history.
		entries = append(r.historyOf(ctx, from, snap.IDs[from], snap.Left[to], resumed), entries...)
	}
	text := buildHandoff(r.names[from], entries, resumed, handoffBudget(r.windowOf(to), r.handoffMax))
	key := core.MsgShellSwitched
	if text != "" {
		key = core.MsgShellSwitchedCarried
	}
	return &handoff{from: from, text: text, notice: &core.Notice{Key: key, Args: []any{r.names[to]}}}
}

func hasShell(entries []entry, shell string) bool {
	for _, e := range entries {
		if e.Shell == shell {
			return true
		}
	}
	return false
}

// historyOf reads a shell's own history of sessionID as transcript entries.
func (r *Agent) historyOf(ctx context.Context, shell, sessionID string, since time.Time, resumed bool) []entry {
	hp, ok := r.shells[shell].(core.HistoryProvider)
	if !ok || sessionID == "" {
		return nil
	}
	hist, err := hp.GetSessionHistory(ctx, sessionID, 400)
	if err != nil {
		slog.Debug("model_router: no history from shell", "shell", shell, "error", err)
		return nil
	}
	var out []entry
	for _, h := range hist {
		if resumed && !since.IsZero() && h.Timestamp.Before(since) {
			continue
		}
		role := roleAssistant
		if h.Role == "user" {
			role = roleUser
		}
		out = append(out, entry{Time: h.Timestamp, Shell: shell, Role: role, Content: h.Content})
	}
	return out
}

// windowOf is the context window (tokens) of the current model on shell, or
// 0 when the shell does not say.
func (r *Agent) windowOf(shell string) int {
	budget, ok := r.shells[shell].(core.SwitchContextBudget)
	if !ok {
		return 0
	}
	// The switch line is three quarters of the window.
	return budget.SwitchCompactLine(r.GetModel()) * 4 / 3
}

// revertTo makes the model shell last ran the current model again and
// returns it.
func (r *Agent) revertTo(shell string) string {
	r.mu.Lock()
	model := r.lastModel[shell]
	r.mu.Unlock()
	if model == "" {
		if ms, ok := r.shells[shell].(core.ModelSwitcher); ok {
			model = ms.GetModel()
		}
	}
	r.mu.Lock()
	r.model = model
	if r.activeIdx >= 0 && r.activeIdx < len(r.providers) && r.providers[r.activeIdx].Model != "" {
		r.providers, _ = core.SetProviderModel(r.providers, r.providers[r.activeIdx].Name, model)
	}
	r.mu.Unlock()
	r.sync()
	return model
}

// ownerOf names the shell a session id belongs to.
func (r *Agent) ownerOf(ctx context.Context, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if s := r.store.shellOf(sessionID); s != "" {
		return s
	}
	if v, ok := r.claude().(core.SessionIDValidator); ok && v.ValidateSessionID(ctx, sessionID) {
		return shellClaude
	}
	return r.currentShell()
}

// ── models and providers ─────────────────────────────────────

func (r *Agent) GetModel() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return core.GetProviderModel(r.providers, r.activeIdx, r.model)
}

func (r *Agent) SetModel(model string) {
	r.mu.Lock()
	r.model = model
	r.mu.Unlock()
	r.sync()
}

// sync hands the current model to its shell and the provider list to Claude
// Code, with models of the other shell replaced by the Claude model it last
// ran, so Claude Code is never asked to run a model it cannot.
func (r *Agent) sync() {
	r.mu.Lock()
	model := core.GetProviderModel(r.providers, r.activeIdx, r.model)
	shell := ""
	if model != "" {
		shell = r.shellFor(model)
		r.lastModel[shell] = model
		if r.applied[shell] == model {
			shell = ""
		} else {
			r.applied[shell] = model
		}
	}
	var providers []core.ProviderConfig
	if r.providers != nil {
		providers = make([]core.ProviderConfig, len(r.providers))
		copy(providers, r.providers)
		for i := range providers {
			if providers[i].Model != "" && r.shellFor(providers[i].Model) != shellClaude {
				providers[i].Model = r.lastModel[shellClaude]
			}
		}
	}
	r.mu.Unlock()

	if ps, ok := r.claude().(core.ProviderSwitcher); ok && providers != nil {
		ps.SetProviders(providers)
	}
	if shell != "" {
		if ms, ok := r.shells[shell].(core.ModelSwitcher); ok {
			ms.SetModel(model)
		}
	}
}

// AvailableModels lists the configured models. Without a configured list it
// falls back to Claude Code's models followed by the ACP agent's that run there.
func (r *Agent) AvailableModels(ctx context.Context) []core.ModelOption {
	r.mu.Lock()
	models := append([]core.ModelOption(nil), core.GetProviderModels(r.providers, r.activeIdx)...)
	r.mu.Unlock()
	if len(models) > 0 {
		return models
	}
	if ms, ok := r.claude().(core.ModelSwitcher); ok {
		models = append(models, ms.AvailableModels(ctx)...)
	}
	if ms, ok := r.shells[shellACP].(core.ModelSwitcher); ok {
		models = r.appendACPModels(models, ms.AvailableModels(ctx))
	}
	return models
}

// ConfiguredModels is the model list known without a network lookup, for
// display names.
func (r *Agent) ConfiguredModels() []core.ModelOption {
	r.mu.Lock()
	models := append([]core.ModelOption(nil), core.GetProviderModels(r.providers, r.activeIdx)...)
	r.mu.Unlock()
	if len(models) > 0 {
		return models
	}
	if ms, ok := r.shells[shellACP].(core.ModelSwitcher); ok {
		// The ACP agent only reports what its last handshake listed.
		models = r.appendACPModels(models, ms.AvailableModels(context.Background()))
	}
	return models
}

func (r *Agent) appendACPModels(models, acp []core.ModelOption) []core.ModelOption {
	for _, m := range acp {
		if r.shellFor(m.Name) != shellACP || hasModel(models, m.Name) {
			continue
		}
		models = append(models, m)
	}
	return models
}

func hasModel(models []core.ModelOption, name string) bool {
	for _, m := range models {
		if strings.EqualFold(m.Name, name) {
			return true
		}
	}
	return false
}

func (r *Agent) SetProviders(providers []core.ProviderConfig) {
	r.mu.Lock()
	r.providers = append([]core.ProviderConfig(nil), providers...)
	if r.activeIdx >= len(r.providers) {
		r.activeIdx = -1
	}
	r.mu.Unlock()
	r.sync()
}

func (r *Agent) SetActiveProvider(name string) bool {
	r.mu.Lock()
	idx := -1
	for i, p := range r.providers {
		if p.Name == name {
			idx = i
		}
	}
	if name != "" && idx < 0 {
		r.mu.Unlock()
		return false
	}
	r.activeIdx = idx
	r.mu.Unlock()
	if ps, ok := r.claude().(core.ProviderSwitcher); ok {
		ps.SetActiveProvider(name)
	}
	r.sync()
	return true
}

func (r *Agent) GetActiveProvider() *core.ProviderConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeIdx < 0 || r.activeIdx >= len(r.providers) {
		return nil
	}
	p := r.providers[r.activeIdx]
	return &p
}

func (r *Agent) ListProviders() []core.ProviderConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.ProviderConfig(nil), r.providers...)
}

// SwitchCompactLine is the switch line of model's shell: a conversation only
// carries over within a shell, so only a same-shell switch compacts.
func (r *Agent) SwitchCompactLine(model string) int {
	if b, ok := r.shells[r.shellFor(model)].(core.SwitchContextBudget); ok {
		return b.SwitchCompactLine(model)
	}
	return 0
}

// ── reasoning effort, mode, compaction: the current shell's ──

// Reasoning effort belongs to the shell the conversation runs in: Claude
// Code takes it as --effort at the next process start, the ACP agent through
// its thought_level option (set live), and only when the agent publishes one.
// Each shell keeps its own value across model switches.

func (r *Agent) effortShell() (core.ReasoningEffortSwitcher, bool) {
	s, ok := r.current().(core.ReasoningEffortSwitcher)
	return s, ok
}

func (r *Agent) SetReasoningEffort(effort string) {
	if s, ok := r.effortShell(); ok {
		s.SetReasoningEffort(effort)
	}
}

func (r *Agent) GetReasoningEffort() string {
	if s, ok := r.effortShell(); ok {
		return s.GetReasoningEffort()
	}
	return ""
}

func (r *Agent) AvailableReasoningEfforts() []string {
	if s, ok := r.effortShell(); ok {
		return s.AvailableReasoningEfforts()
	}
	return nil
}

// KeepsSessionOnReasoningChange tells the engine the conversation survives a
// reasoning change: Claude Code resumes it with the new --effort, the ACP
// agent reloads it with the new thought level.
func (r *Agent) KeepsSessionOnReasoningChange() bool { return true }

func (r *Agent) SetMode(mode string) {
	if s, ok := r.current().(core.ModeSwitcher); ok {
		s.SetMode(mode)
	}
}

func (r *Agent) GetMode() string {
	if s, ok := r.current().(core.ModeSwitcher); ok {
		return s.GetMode()
	}
	return ""
}

func (r *Agent) PermissionModes() []core.PermissionModeInfo {
	if s, ok := r.current().(core.ModeSwitcher); ok {
		return s.PermissionModes()
	}
	return nil
}

func (r *Agent) CompressCommand() string {
	if c, ok := r.current().(core.ContextCompressor); ok {
		return c.CompressCommand()
	}
	return ""
}

// ── both shells ──────────────────────────────────────────────

func (r *Agent) SetWorkDir(dir string) {
	for _, a := range r.shells {
		if s, ok := a.(core.WorkDirSwitcher); ok {
			s.SetWorkDir(dir)
		}
	}
}

func (r *Agent) GetWorkDir() string {
	if s, ok := r.claude().(core.WorkDirSwitcher); ok {
		return s.GetWorkDir()
	}
	return ""
}

func (r *Agent) SetSessionEnv(env []string) {
	for _, a := range r.shells {
		if s, ok := a.(core.SessionEnvInjector); ok {
			s.SetSessionEnv(env)
		}
	}
}

func (r *Agent) Stop() error {
	var errs []error
	for name, a := range r.shells {
		if err := a.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// WorkspaceAgentOptions rebuilds the router's options for a per-topic or
// per-workspace copy.
func (r *Agent) WorkspaceAgentOptions() map[string]any {
	opts := map[string]any{"claude_models": append([]string(nil), r.patterns...)}
	for k, v := range r.state {
		opts[k] = v
	}
	for name, a := range r.shells {
		table := map[string]any{}
		if s, ok := a.(core.WorkspaceAgentOptionSnapshotter); ok {
			for k, v := range s.WorkspaceAgentOptions() {
				table[k] = v
			}
		}
		opts[name] = table
	}
	if m := r.GetModel(); m != "" {
		opts["model"] = m
	}
	opts["handoff_max_chars"] = r.handoffMax
	return opts
}

// ── Claude Code's: prompt, memory, skills, tools, usage ──────

// SetPlatformPrompt reaches Claude Code only; the ACP agent gets no system
// prompt from the bridge.
func (r *Agent) SetPlatformPrompt(prompt string) {
	if s, ok := r.claude().(core.PlatformPromptInjector); ok {
		s.SetPlatformPrompt(prompt)
	}
}

func (r *Agent) HasSystemPromptSupport() bool {
	s, ok := r.claude().(core.SystemPromptSupporter)
	return ok && s.HasSystemPromptSupport()
}

func (r *Agent) ProjectMemoryFile() string {
	if m, ok := r.claude().(core.MemoryFileProvider); ok {
		return m.ProjectMemoryFile()
	}
	return ""
}

func (r *Agent) GlobalMemoryFile() string {
	if m, ok := r.claude().(core.MemoryFileProvider); ok {
		return m.GlobalMemoryFile()
	}
	return ""
}

func (r *Agent) SkillDirs() []string {
	if s, ok := r.claude().(core.SkillProvider); ok {
		return s.SkillDirs()
	}
	return nil
}

func (r *Agent) CommandDirs() []string {
	if s, ok := r.claude().(core.CommandProvider); ok {
		return s.CommandDirs()
	}
	return nil
}

func (r *Agent) AddAllowedTools(tools ...string) error {
	if s, ok := r.claude().(core.ToolAuthorizer); ok {
		return s.AddAllowedTools(tools...)
	}
	return nil
}

func (r *Agent) GetAllowedTools() []string {
	if s, ok := r.claude().(core.ToolAuthorizer); ok {
		return s.GetAllowedTools()
	}
	return nil
}

func (r *Agent) GetUsage(ctx context.Context) (*core.UsageReport, error) {
	if s, ok := r.claude().(core.UsageReporter); ok {
		return s.GetUsage(ctx)
	}
	return nil, errors.New("model_router: usage not available")
}

func (r *Agent) CLIBinaryName() string {
	if s, ok := r.current().(core.AgentDoctorInfo); ok {
		return s.CLIBinaryName()
	}
	return ""
}

func (r *Agent) CLIDisplayName() string { return r.names[r.currentShell()] }

func (r *Agent) GetRunAsUser() string {
	if s, ok := r.claude().(interface{ GetRunAsUser() string }); ok {
		return s.GetRunAsUser()
	}
	return ""
}

func (r *Agent) GetRunAsEnv() []string {
	if s, ok := r.claude().(interface{ GetRunAsEnv() []string }); ok {
		return s.GetRunAsEnv()
	}
	return nil
}

// ── sessions ─────────────────────────────────────────────────

func (r *Agent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	var all []core.AgentSessionInfo
	var errs []error
	for _, name := range []string{shellClaude, shellACP} {
		list, err := r.shells[name].ListSessions(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		all = append(all, list...)
	}
	if len(all) == 0 && len(errs) == len(r.shells) {
		return nil, errors.Join(errs...)
	}
	for _, err := range errs {
		slog.Debug("model_router: list sessions", "error", err)
	}
	return all, nil
}

// GetSessionHistory returns the thread's recorded conversation across both
// shells, else the owning shell's own history.
func (r *Agent) GetSessionHistory(ctx context.Context, sessionID string, limit int) ([]core.HistoryEntry, error) {
	if th := r.store.lookup(sessionID); th != nil {
		var out []core.HistoryEntry
		for _, e := range r.store.transcript(th) {
			if e.Role == roleTool {
				continue
			}
			out = append(out, core.HistoryEntry{Role: e.Role, Content: e.Content, Timestamp: e.Time})
		}
		if len(out) > 0 {
			if limit > 0 && len(out) > limit {
				out = out[len(out)-limit:]
			}
			return out, nil
		}
	}
	if hp, ok := r.shells[r.ownerOf(ctx, sessionID)].(core.HistoryProvider); ok {
		return hp.GetSessionHistory(ctx, sessionID, limit)
	}
	return nil, fmt.Errorf("model_router: no history for session %s", sessionID)
}

func (r *Agent) DeleteSession(ctx context.Context, sessionID string) error {
	owner := r.ownerOf(ctx, sessionID)
	defer r.store.forget(sessionID)
	if d, ok := r.shells[owner].(core.SessionDeleter); ok {
		return d.DeleteSession(ctx, sessionID)
	}
	return nil
}

// ValidateSessionID accepts ids of this router's threads and ids Claude Code
// knows for this project.
func (r *Agent) ValidateSessionID(ctx context.Context, sessionID string) bool {
	if r.store.shellOf(sessionID) != "" {
		return true
	}
	if v, ok := r.claude().(core.SessionIDValidator); ok {
		return v.ValidateSessionID(ctx, sessionID)
	}
	return true
}

var (
	_ core.Agent                           = (*Agent)(nil)
	_ core.ModelSwitcher                   = (*Agent)(nil)
	_ core.ProviderSwitcher                = (*Agent)(nil)
	_ core.SwitchContextBudget             = (*Agent)(nil)
	_ core.ReasoningEffortSwitcher         = (*Agent)(nil)
	_ core.ModeSwitcher                    = (*Agent)(nil)
	_ core.ContextCompressor               = (*Agent)(nil)
	_ core.WorkDirSwitcher                 = (*Agent)(nil)
	_ core.SessionEnvInjector              = (*Agent)(nil)
	_ core.WorkspaceAgentOptionSnapshotter = (*Agent)(nil)
	_ core.PlatformPromptInjector          = (*Agent)(nil)
	_ core.SystemPromptSupporter           = (*Agent)(nil)
	_ core.MemoryFileProvider              = (*Agent)(nil)
	_ core.SkillProvider                   = (*Agent)(nil)
	_ core.CommandProvider                 = (*Agent)(nil)
	_ core.ToolAuthorizer                  = (*Agent)(nil)
	_ core.UsageReporter                   = (*Agent)(nil)
	_ core.AgentDoctorInfo                 = (*Agent)(nil)
	_ core.HistoryProvider                 = (*Agent)(nil)
	_ core.SessionDeleter                  = (*Agent)(nil)
	_ core.SessionIDValidator              = (*Agent)(nil)
)
