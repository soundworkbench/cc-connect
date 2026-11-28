package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
		RevealCps: 90, RevealCatchupMs: 4000, RevealLeadMs: 250, BodyInflight: 6, DrainMaxMs: 8000,
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
reveal_lead_ms = 0
body_inflight = 1
`)
	if got.RevealLeadMs != 0 || got.BodyInflight != 1 {
		t.Fatalf("explicit pipeline values ignored: %+v", got)
	}
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

func TestStreamPreviewSpinnerImage(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.gif")
	gifBytes := []byte("GIF89a-test")
	if err := os.WriteFile(good, gifBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.gif")
	if err := os.WriteFile(bad, []byte("not a gif"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := decodeStreamPreview(t, "[stream_preview]\nspinner_image = '"+good+"'\n").SpinnerGIF; string(got) != string(gifBytes) {
		t.Fatalf("configured file: %q", got)
	}
	for name, path := range map[string]string{"unset": "", "missing": filepath.Join(dir, "none.gif"), "not a gif": bad} {
		if got := decodeStreamPreview(t, "[stream_preview]\nspinner_image = '"+path+"'\n").SpinnerGIF; got != nil {
			t.Fatalf("%s: want built-in spinner, got %q", name, got)
		}
	}
}

func TestProductDefaultsOutOfTheBox(t *testing.T) {
	// Empty config enables quiet Feishu experience, step-list streaming card, and decision cards.
	var cfg config.Config
	mode, tm, tool, _, _, showCtx, replyFooter, hideAgentFooter := config.EffectiveDisplay(&cfg, &config.ProjectConfig{})
	if mode != config.DisplayModeQuiet || !tm || tool || showCtx || replyFooter || !hideAgentFooter {
		t.Fatalf("display defaults mismatch: mode=%q tm=%v tool=%v showCtx=%v replyFooter=%v hideAgentFooter=%v",
			mode, tm, tool, showCtx, replyFooter, hideAgentFooter)
	}
	sp := buildStreamPreviewCfg(cfg.StreamPreview)
	if !sp.Enabled || !sp.Segmented || !sp.StepList || sp.EndMode != "settle" || sp.RevealCps != 90 || sp.PrintStrategy != "delay" || sp.NotifyAfterMs != 60000 {
		t.Fatalf("stream_preview defaults mismatch: %+v", sp)
	}
	if !cfg.Decision.EffectiveEnabled() || cfg.Decision.EffectiveDefaultHours() != 12 {
		t.Fatalf("decision defaults mismatch: enabled=%v hours=%d", cfg.Decision.EffectiveEnabled(), cfg.Decision.EffectiveDefaultHours())
	}

	// bootstrapConfig writes valid TOML pointing to soundworkbench/cc-connect and listing orca.
	outPath := filepath.Join(t.TempDir(), "config.toml")
	if err := bootstrapConfig(outPath); err != nil {
		t.Fatalf("bootstrapConfig failed: %v", err)
	}
	loaded, err := config.Load(outPath)
	if err != nil {
		t.Fatalf("bootstrapConfig output invalid: %v", err)
	}
	if !loaded.Decision.EffectiveEnabled() {
		t.Fatal("bootstrapConfig should keep decision cards enabled by default")
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"soundworkbench/cc-connect", `"orca"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("bootstrapConfig missing %q", want)
		}
	}
}
