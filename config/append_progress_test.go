package config

import (
	"github.com/BurntSushi/toml"
	"testing"
)

func TestAppendProgressConfigInheritanceValidation(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode("[display]\nmode='quiet'\nprogress_history='append'\nkeep_progress_card=true", &cfg); err != nil {
		t.Fatal(err)
	}
	if err := validateDisplayConfig("display", &cfg.Display); err != nil {
		t.Fatal(err)
	}
	if EffectiveProgressHistory(&cfg, nil) != "append" || !EffectiveKeepProgressCard(&cfg, nil) {
		t.Fatal("global config lost")
	}
	recent, keep := "recent", false
	proj := ProjectConfig{Display: &DisplayConfig{ProgressHistory: &recent, KeepProgressCard: &keep}}
	if EffectiveProgressHistory(&cfg, &proj) != "recent" || EffectiveKeepProgressCard(&cfg, &proj) {
		t.Fatal("project overrides lost")
	}
	if EffectiveProgressHistory(&Config{}, nil) != "" || EffectiveKeepProgressCard(&Config{}, nil) {
		t.Fatal("defaults changed")
	}
	invalid := "replace"
	if validateDisplayConfig("projects.x.display", &DisplayConfig{ProgressHistory: &invalid}) == nil {
		t.Fatal("invalid history accepted")
	}
}
