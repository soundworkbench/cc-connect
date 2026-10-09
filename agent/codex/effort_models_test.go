package codex

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOfficialEffortLevels_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		model string
		count int
	}{
		{model: "gpt-6-astra", count: 6},
		{model: "gpt-6-sol", count: 6},
		{model: "gpt-6-luna", count: 5},
		{model: "gpt-5.5", count: 4},
	} {
		a := &Agent{model: tc.model}
		levels := a.AvailableReasoningEfforts()
		if len(levels) != tc.count {
			t.Fatalf("%s levels=%v", tc.model, levels)
		}
		for _, level := range levels {
			a.SetReasoningEffort(level)
			if a.GetReasoningEffort() != level {
				t.Fatalf("%s lost %s", tc.model, level)
			}
		}
	}
}

func TestOfficialEffortLevels_CacheTakesPriority(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-6-astra","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]}]}`), 0600)
	a := &Agent{model: "gpt-6-astra", codexHome: home}
	if got := a.AvailableReasoningEfforts(); len(got) != 3 {
		t.Fatalf("cache ignored: %v", got)
	}
}
