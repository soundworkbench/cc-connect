package config

import (
	"github.com/BurntSushi/toml"
	"testing"
)

func TestToolProgressConfigResolutionAndValidation(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode("[display]\nmode='quiet'\ntool_progress_lines=2", &cfg); err != nil {
		t.Fatal(err)
	}
	if p := EffectiveToolProgressLines(&cfg, nil); p == nil || *p != 2 {
		t.Fatal(p)
	}
	zero := 0
	proj := ProjectConfig{Display: &DisplayConfig{ToolProgressLines: &zero}}
	if p := EffectiveToolProgressLines(&cfg, &proj); p == nil || *p != 0 {
		t.Fatal(p)
	}
	if p := EffectiveToolProgressLines(&Config{}, nil); p != nil {
		t.Fatal("default must follow runtime mode")
	}
	for _, n := range []int{-1, 0, 1, 2, 3, 4} {
		err := validateDisplayConfig("display", &DisplayConfig{ToolProgressLines: &n})
		if (err != nil) != (n < 0 || n > 3) {
			t.Fatalf("%d: %v", n, err)
		}
	}
}
