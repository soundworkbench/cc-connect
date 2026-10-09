package feishu

import "testing"

func TestPreviewLifecycleCompactPreference(t *testing.T) {
	for _, tc := range []struct {
		cards bool
		style string
		want  bool
	}{
		{true, "compact", true}, {true, "rich", false}, {true, "legacy", false}, {false, "compact", false},
	} {
		p := &Platform{useInteractiveCard: tc.cards, progressStyle: tc.style}
		if p.UseThinkingProcessPreview() != tc.want {
			t.Errorf("cards=%t style=%s", tc.cards, tc.style)
		}
		if p.KeepPreviewOnFinish() {
			t.Error("temporary card must be recalled")
		}
		if p.StartPreviewImmediately() != tc.cards {
			t.Error("immediate card preference differs")
		}
	}
}
