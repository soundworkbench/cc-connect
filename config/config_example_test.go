package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestQueueBatch_ConfigExampleIsValidTOML(t *testing.T) {
	var cfg Config
	if _, err := toml.DecodeFile("../config.example.toml", &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.QueueBatch.Resolve().Enabled {
		t.Fatal("example must enable queue batching")
	}
}

func TestQueueBatch_ConfigExamplePreservesRootOptions(t *testing.T) {
	b, err := os.ReadFile("../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, option := range []string{
		`language = "en"`, `shell = "/bin/zsh"`,
		`shell_profile = "source ~/.zshrc"`, `data_dir = "/path/to/custom/dir"`,
		`attachment_send = "on"`, `max_attachment_size_mb = 50`,
	} {
		if !strings.Contains(text, "# "+option) {
			t.Fatalf("missing example option %s", option)
		}
		text = strings.Replace(text, "# "+option, option, 1)
	}
	// Restore only the known broken comment so misplaced tables fail separately.
	text = strings.Replace(text, "\n[[projects]] entry binds", "\n# [[projects]] entry binds", 1)
	path := filepath.Join(t.TempDir(), "example.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Language != "en" || cfg.DataDir != "/path/to/custom/dir" || cfg.Shell != "/bin/zsh" || cfg.ShellProfile != "source ~/.zshrc" || cfg.AttachmentSend != "on" || cfg.MaxAttachmentSizeMB != 50 {
		t.Fatalf("root options lost: language=%q data_dir=%q shell=%q shell_profile=%q attachment_send=%q max_attachment_size_mb=%d", cfg.Language, cfg.DataDir, cfg.Shell, cfg.ShellProfile, cfg.AttachmentSend, cfg.MaxAttachmentSizeMB)
	}
}
