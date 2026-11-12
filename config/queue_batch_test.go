package config

import (
	"github.com/BurntSushi/toml"
	"reflect"
	"strings"
	"testing"
)

func TestQueueBatchConfig_DefaultsAndExplicitZero(t *testing.T) {
	want := ResolvedQueueBatch{Enabled: true, MaxMessages: 20, MaxChars: 20000, HoldSeconds: 3, DebounceMs: 1500, ImmediateSources: []string{"human"}}
	if got := (QueueBatchConfig{}).Resolve(); !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults=%+v", got)
	}
	var cfg Config
	if _, err := toml.Decode("[queue_batch]\nenabled=false\nmax_messages=3\nmax_chars=100\nhold_seconds=0\ndebounce_ms=0\nimmediate_sources=[\"relay\",\"timer\"]\n", &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.QueueBatch.validate(); err != nil {
		t.Fatal(err)
	}
	want = ResolvedQueueBatch{Enabled: false, MaxMessages: 3, MaxChars: 100, HoldSeconds: 0, DebounceMs: 0, ImmediateSources: []string{"relay", "timer"}}
	if got := cfg.QueueBatch.Resolve(); !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit=%+v", got)
	}
}
func TestQueueBatchConfig_InvalidLimits(t *testing.T) {
	for _, input := range []string{"max_messages=0", "max_messages=-1", "max_chars=0", "hold_seconds=-1", "debounce_ms=-1", "debounce_ms=60001", "hold_seconds=9223372037", "immediate_sources=[\"\"]"} {
		t.Run(input, func(t *testing.T) {
			var cfg Config
			if _, err := toml.Decode("[queue_batch]\n"+input, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Projects = []ProjectConfig{validProject("test")}
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "queue_batch") {
				t.Fatalf("validation=%v", err)
			}
		})
	}
}
