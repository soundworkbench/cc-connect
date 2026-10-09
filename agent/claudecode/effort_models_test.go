package claudecode

import "testing"

func TestOfficialEffortLevels_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		model string
		count int
	}{
		{model: "claude-fable-5-1", count: 5},
		{model: "claude-opus-5-5", count: 5},
		{model: "claude-sonnet-5", count: 5},
		{model: "claude-opus-4-6", count: 4},
		{model: "haiku", count: 0},
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
