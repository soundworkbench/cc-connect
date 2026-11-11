package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/config"
)

func TestQuickLineDefaultsOnWhenConfigured(t *testing.T) {
	key := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	full := config.QuickLineConfig{BaseURL: "http://gw", Model: "m", KeyFile: key}
	if !buildQuickLineCfg(full).Enabled {
		t.Fatal("a section with base_url, model and key_file should be on without enabled = true")
	}
	if buildQuickLineCfg(config.QuickLineConfig{}).Enabled {
		t.Fatal("no key configured should leave the quick line off")
	}
	off := false
	full.Enabled = &off
	if buildQuickLineCfg(full).Enabled {
		t.Fatal("enabled = false should turn it off")
	}
	missing := config.QuickLineConfig{BaseURL: "http://gw", Model: "m", KeyFile: filepath.Join(t.TempDir(), "none")}
	if buildQuickLineCfg(missing).Enabled {
		t.Fatal("an unreadable key file should leave the quick line off")
	}
}

func TestQuickLineFallsBackToContainerEnv(t *testing.T) {
	key := filepath.Join(t.TempDir(), "magpie.key")
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://magpie-co:3425/v1/")
	t.Setenv("PARTNER_MODEL_KEY_FILE", key)
	t.Setenv("QUICK_LINE_MODEL", "")
	if buildQuickLineCfg(config.QuickLineConfig{}).Enabled {
		t.Fatal("without QUICK_LINE_MODEL the quick line should stay off")
	}
	t.Setenv("QUICK_LINE_MODEL", "auto/gpt-6-luna")
	q := buildQuickLineCfg(config.QuickLineConfig{})
	if !q.Enabled || q.BaseURL != "http://magpie-co:3425" || q.Model != "auto/gpt-6-luna" || q.KeyFile != key {
		t.Fatalf("container env should turn it on, got %+v", q)
	}
	q = buildQuickLineCfg(config.QuickLineConfig{BaseURL: "http://127.0.0.1:3425", Model: "m"})
	if q.BaseURL != "http://127.0.0.1:3425" || q.Model != "m" {
		t.Fatalf("explicit config should win over env, got %+v", q)
	}
}
