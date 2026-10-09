package core

import (
	"context"
	"fmt"
	"log/slog"
)

// Isolated agent options belong to a routing key, surviving /new and restart.
// The platform opts in via Message.IsolateAgent; core treats the key as opaque.
func (sm *SessionManager) isolatedModel(key string) (string, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	model, ok := sm.isolatedModels[key]
	return model, ok
}
func (sm *SessionManager) setIsolatedModel(key, model string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.isolatedModels == nil {
		sm.isolatedModels = make(map[string]string)
	}
	sm.isolatedModels[key] = model
	sm.saveLocked()
}

type sessionAgentKey struct {
	sessions *SessionManager
	key      string
}

type sessionAgent struct {
	agent    Agent
	sessions *SessionManager
	key      string
}

func (e *Engine) isolateAgent(base Agent, sessions *SessionManager, key string, optIn bool) (Agent, error) {
	model, enabled := sessions.isolatedModel(key)
	if !enabled && !optIn {
		return base, nil
	}
	e.sessionAgentsMu.Lock()
	defer e.sessionAgentsMu.Unlock()
	identity := sessionAgentKey{sessions, key}
	if entry, ok := e.sessionAgents[identity]; ok {
		return entry.agent, nil
	}
	snapshotter, ok := base.(WorkspaceAgentOptionSnapshotter)
	if !ok {
		return nil, fmt.Errorf("agent %s cannot isolate session options", base.Name())
	}
	opts := snapshotter.WorkspaceAgentOptions()
	if opts == nil {
		opts = make(map[string]any)
	}
	if wd, ok := base.(interface{ GetWorkDir() string }); ok {
		opts["work_dir"] = wd.GetWorkDir()
	}
	if _, ok := opts["run_as_user"]; !ok {
		if user := e.runAsUser(); user != "" {
			opts["run_as_user"] = user
		}
	}
	if _, ok := opts["run_as_env"]; !ok {
		if source, ok := base.(interface{ GetRunAsEnv() []string }); ok {
			opts["run_as_env"] = source.GetRunAsEnv()
		}
	}
	if model != "" {
		opts["model"] = model
	}
	agent, err := CreateAgent(base.Name(), opts)
	if err != nil {
		return nil, fmt.Errorf("create isolated session agent: %w", err)
	}
	if src, ok := base.(ProviderSwitcher); ok {
		if dst, ok := agent.(ProviderSwitcher); ok {
			dst.SetProviders(src.ListProviders())
			if active := src.GetActiveProvider(); active != nil {
				dst.SetActiveProvider(active.Name)
			}
		}
	}
	if model != "" {
		if switcher, ok := agent.(ModelSwitcher); ok {
			switcher.SetModel(model)
		}
	}
	if e.sessionAgents == nil {
		e.sessionAgents = make(map[sessionAgentKey]sessionAgent)
	}
	e.sessionAgents[identity] = sessionAgent{agent, sessions, key}
	if !enabled {
		sessions.setIsolatedModel(key, model)
	}
	return agent, nil
}

func (e *Engine) persistIsolatedModel(agent Agent, model string) bool {
	e.sessionAgentsMu.Lock()
	defer e.sessionAgentsMu.Unlock()
	for _, entry := range e.sessionAgents {
		if entry.agent == agent {
			entry.sessions.setIsolatedModel(entry.key, model)
			return true
		}
	}
	return false
}

func (e *Engine) commandContextWithWorkspace(p Platform, msg *Message) (Agent, *SessionManager, string, string, error) {
	agent, sessions, key, dir, err := e.baseCommandContextWithWorkspace(p, msg)
	if err != nil {
		return nil, nil, "", "", err
	}
	agent, err = e.isolateAgent(agent, sessions, msg.SessionKey, msg.IsolateAgent)
	return agent, sessions, key, dir, err
}

func (e *Engine) sessionContextForKey(key string) (Agent, *SessionManager) {
	agent, sessions := e.baseSessionContextForKey(key)
	agent, err := e.isolateAgent(agent, sessions, key, false)
	if err != nil {
		// Card callbacks cannot return resolution errors. Fail closed: never let a
		// failed thread clone mutate the shared agent's model or provider instead.
		slog.Error("isolated session agent unavailable", "session", key, "error", err)
		return unavailableSessionAgent{err}, sessions
	}
	return agent, sessions
}

type unavailableSessionAgent struct{ err error }

func (a unavailableSessionAgent) Name() string { return "unavailable" }
func (a unavailableSessionAgent) StartSession(context.Context, string) (AgentSession, error) {
	return nil, a.err
}
func (a unavailableSessionAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return nil, a.err
}
func (a unavailableSessionAgent) Stop() error { return nil }
