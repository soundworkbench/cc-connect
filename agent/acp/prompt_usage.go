package acp

import "encoding/json"

// promptUsage is the token usage an ACP agent reports in the session/prompt
// result. Fields the agent leaves out stay 0.
type promptUsage struct {
	Input       int
	Output      int
	Thought     int
	Total       int
	CachedRead  int
	CachedWrite int
}

// parsePromptUsage reads result.usage (inputTokens, outputTokens,
// thoughtTokens, totalTokens, cachedReadTokens, cachedWriteTokens). ok is
// false when the result carries no usage or every count is 0.
func parsePromptUsage(res json.RawMessage) (promptUsage, bool) {
	var pr struct {
		Usage struct {
			InputTokens       int `json:"inputTokens"`
			OutputTokens      int `json:"outputTokens"`
			ThoughtTokens     int `json:"thoughtTokens"`
			TotalTokens       int `json:"totalTokens"`
			CachedReadTokens  int `json:"cachedReadTokens"`
			CachedWriteTokens int `json:"cachedWriteTokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(res, &pr) != nil {
		return promptUsage{}, false
	}
	u := promptUsage{
		Input:       pr.Usage.InputTokens,
		Output:      pr.Usage.OutputTokens,
		Thought:     pr.Usage.ThoughtTokens,
		Total:       pr.Usage.TotalTokens,
		CachedRead:  pr.Usage.CachedReadTokens,
		CachedWrite: pr.Usage.CachedWriteTokens,
	}
	if u == (promptUsage{}) {
		return promptUsage{}, false
	}
	return u, true
}
