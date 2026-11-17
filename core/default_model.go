package core

import "errors"

// ErrModelSwitchUnsupported is returned when the project's agent cannot switch models.
var ErrModelSwitchUnsupported = errors.New("agent does not support model switching")

// DefaultModel returns the project's default model: the one sessions without
// their own /model choice start with.
func (e *Engine) DefaultModel() (string, error) {
	switcher, ok := e.agent.(ModelSwitcher)
	if !ok {
		return "", ErrModelSwitchUnsupported
	}
	return switcher.GetModel(), nil
}

// SetDefaultModel switches the project's default model from outside a chat and
// persists it the same way /model does. Sessions that follow the default use
// the new model from their next turn: a live agent process is recycled when
// that turn starts and resumes the same conversation, so a running turn is never
// interrupted. Topics that picked their own model with /model keep it.
func (e *Engine) SetDefaultModel(target string) (string, error) {
	if _, ok := e.agent.(ModelSwitcher); !ok {
		return "", ErrModelSwitchUnsupported
	}
	model, err := e.switchModel(target)
	if err != nil {
		return "", err
	}

	followers := []Agent{e.agent}
	e.sessionAgentsMu.Lock()
	for _, entry := range e.sessionAgents {
		if pinned, _ := entry.sessions.isolatedModel(entry.key); pinned != "" {
			continue
		}
		e.followDefaultModel(entry.agent, model)
		followers = append(followers, entry.agent)
	}
	e.sessionAgentsMu.Unlock()

	e.interactiveMu.Lock()
	for _, state := range e.interactiveStates {
		state.mu.Lock()
		for _, agent := range followers {
			if state.agent == agent {
				state.modelStale = true
				break
			}
		}
		state.mu.Unlock()
	}
	e.interactiveMu.Unlock()
	return model, nil
}

// followDefaultModel copies the shared agent's model and providers onto a topic
// agent that has no model of its own.
func (e *Engine) followDefaultModel(agent Agent, model string) {
	if dst, ok := agent.(ProviderSwitcher); ok {
		if src, ok := e.agent.(ProviderSwitcher); ok {
			dst.SetProviders(src.ListProviders())
			// Re-selecting an unchanged provider would close its proxy under a
			// running turn.
			if active := src.GetActiveProvider(); active != nil {
				if cur := dst.GetActiveProvider(); cur == nil || cur.Name != active.Name {
					dst.SetActiveProvider(active.Name)
				}
			}
		}
	}
	if switcher, ok := agent.(ModelSwitcher); ok {
		switcher.SetModel(model)
	}
}
