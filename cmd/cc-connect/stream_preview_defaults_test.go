package main

import (
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

func decodeStreamPreview(t *testing.T, src string) core.StreamPreviewCfg {
	t.Helper()
	var cfg config.Config
	if _, err := toml.Decode(src, &cfg); err != nil {
		t.Fatal(err)
	}
	return buildStreamPreviewCfg(cfg.StreamPreview)
}

func TestStreamPreviewDefaultsOn(t *testing.T) {
	want := core.StreamPreviewCfg{
		Enabled: true, Segmented: true, StepList: true,
		PushIntervalMs: 150, TypingIntervalMs: 40, TypingStep: 4,
		RevealCps: 90, RevealCatchupMs: 4000, DrainMaxMs: 8000,
		EndMode: "settle", NotifyAfterMs: 60000, DotsPauseMs: 2000,
		FinishCatchupMs: 3000, PrintStrategy: "delay",
		IntervalMs: 1500, MinDeltaChars: 30, MaxChars: 2000,
		SegIntervalMs: 700, SegMinDeltaChars: 12, SegMaxChars: 4000, SegTypingMarker: "...",
	}
	for _, src := range []string{"", "[stream_preview]\nenabled = true\n"} {
		if got := decodeStreamPreview(t, src); !reflect.DeepEqual(got, want) {
			t.Fatalf("defaults mismatch for %q:\n got %+v\nwant %+v", src, got, want)
		}
	}
}

func TestStreamPreviewExplicitValuesWin(t *testing.T) {
	got := decodeStreamPreview(t, `[stream_preview]
segmented = false
step_list = false
reveal_cps = 0
finish_catchup_ms = -1
notify_after_ms = 0
end_mode = "recall"
print_strategy = "fast"
interval_ms = 2000
typing_marker = ""
`)
	if got.Segmented || got.StepList {
		t.Fatalf("explicit false ignored: %+v", got)
	}
	if got.RevealCps != 0 || got.FinishCatchupMs != -1 || got.NotifyAfterMs != 0 {
		t.Fatalf("explicit numeric values ignored: %+v", got)
	}
	if got.EndMode != "recall" || got.PrintStrategy != "fast" {
		t.Fatalf("explicit strings ignored: %+v", got)
	}
	if got.IntervalMs != 2000 || got.SegIntervalMs != 0 || got.TypingMarker != "" || got.SegTypingMarker != "" {
		t.Fatalf("explicit shared values must apply to all platforms: %+v", got)
	}
}

func TestBuildAgentOptionsStreamPartialTextDefault(t *testing.T) {
	feishu := config.ProjectConfig{
		Agent:     config.AgentConfig{Type: "claudecode"},
		Platforms: []config.PlatformConfig{{Type: "feishu"}},
	}
	if v, _ := buildAgentOptions("/d", feishu)["stream_partial_text"].(bool); !v {
		t.Fatal("claudecode on feishu should default stream_partial_text on")
	}
	feishu.Agent.Options = map[string]any{"stream_partial_text": false}
	if v, _ := buildAgentOptions("/d", feishu)["stream_partial_text"].(bool); v {
		t.Fatal("explicit false must stay off")
	}
	other := config.ProjectConfig{
		Agent:     config.AgentConfig{Type: "claudecode"},
		Platforms: []config.PlatformConfig{{Type: "telegram"}},
	}
	if _, set := buildAgentOptions("/d", other)["stream_partial_text"]; set {
		t.Fatal("non-feishu projects must not change")
	}
	codex := config.ProjectConfig{
		Agent:     config.AgentConfig{Type: "codex"},
		Platforms: []config.PlatformConfig{{Type: "feishu"}},
	}
	if _, set := buildAgentOptions("/d", codex)["stream_partial_text"]; set {
		t.Fatal("only claudecode is affected")
	}
}
