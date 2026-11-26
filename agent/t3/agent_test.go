package t3

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewDefaultsAndCredentialFile(t *testing.T) {
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credential")
	if err := os.WriteFile(credentialPath, []byte("opaque-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent, err := New(map[string]any{"credential_file": credentialPath, "cc_data_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	a := agent.(*Agent)
	if a.server != "127.0.0.1:6768" || a.provider != "antigravity" {
		t.Fatalf("defaults = %#v", a)
	}
	if a.credential != "opaque-token" {
		t.Fatalf("credential was not loaded")
	}
	if strings.Contains(a.server, a.credential) {
		t.Fatal("credential leaked into server address")
	}
}

func TestProjectIDForIsStableUUID(t *testing.T) {
	first, second := projectIDFor("/srv/xiaoman/butler"), projectIDFor("/srv/xiaoman/butler")
	if first != second || len(first) != 36 || strings.Count(first, "-") != 4 {
		t.Fatalf("project id = %q", first)
	}
}

func TestListSessionsMissingMapping(t *testing.T) {
	agent, err := New(map[string]any{"cc_data_dir": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := agent.ListSessions(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("sessions = %#v, err = %v", got, err)
	}
}
