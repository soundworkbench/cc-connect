package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Official native model capability fallback, verified 2026-09-22.
// Unknown/older providers retain conservative compatibility; no silent effort remapping.
func modelReasoningEfforts(model string) []string {
	levels := []string{"low", "medium", "high", "xhigh"}
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-6-astra", "gpt-6-sol":
		return append(levels, "max", "ultra")
	case "gpt-6-luna":
		return append(levels, "max")
	}
	return levels
}

func cachedReasoningEfforts(home, model string) []string {
	if home == "" || model == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, "models_cache.json"))
	if err != nil {
		return nil
	}
	var payload struct {
		Models []struct {
			Slug   string
			Levels []struct{ Effort string } `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	for _, m := range payload.Models {
		if m.Slug != model {
			continue
		}
		var result []string
		seen := map[string]bool{}
		for _, l := range m.Levels {
			if normalizeReasoningEffort(l.Effort) == l.Effort && l.Effort != "" && !seen[l.Effort] {
				result = append(result, l.Effort)
				seen[l.Effort] = true
			}
		}
		return result
	}
	return nil
}
