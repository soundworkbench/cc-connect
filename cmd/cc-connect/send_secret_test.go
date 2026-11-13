package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestParseSecretArgs(t *testing.T) {
	t.Setenv("CC_PROJECT", "proj")
	t.Setenv("CC_SESSION_KEY", "feishu:oc_dm:ou_owner")
	req, dataDir, err := parseSecretArgs([]string{"--secret", "hubspot-token", "--hint", "用来读取商机数据", "--url", "https://example.invalid/s", "--data-dir", "/d"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Name != "hubspot-token" || req.Hint != "用来读取商机数据" || req.URL != "https://example.invalid/s" ||
		req.Project != "proj" || req.SessionKey != "feishu:oc_dm:ou_owner" || dataDir != "/d" {
		t.Fatalf("req=%+v dataDir=%q", req, dataDir)
	}
	for _, args := range [][]string{
		{"--secret", "HubSpot"},
		{"--secret"},
		{"--secret", "hubspot-token", "用来读取", "商机"}, // unquoted hint
	} {
		if _, _, err := parseSecretArgs(args); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}

func TestSecretListAndDeleteNeverPrintValues(t *testing.T) {
	store := core.NewSecretStore(filepath.Join(t.TempDir(), ".secrets"))
	if got := secretListText(nil); got != "No saved secrets.\n" {
		t.Fatalf("empty list = %q", got)
	}
	if _, err := store.Save("hubspot-token", "pat-na1-FAKE0000-value"); err != nil {
		t.Fatal(err)
	}
	list, _ := store.List()
	out := secretListText(list)
	if !strings.HasPrefix(out, "hubspot-token\tsaved "+time.Now().Format("2006-01-02")) || strings.Contains(out, "FAKE0000") {
		t.Fatalf("list = %q", out)
	}
	if err := deleteSecret(store, "hubspot-token"); err != nil {
		t.Fatal(err)
	}
	if err := deleteSecret(store, "hubspot-token"); err == nil || !strings.Contains(err.Error(), "--secret-list") {
		t.Fatalf("second delete: %v", err)
	}
	if err := deleteSecret(store, ""); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestSecretSentLine(t *testing.T) {
	if got := secretSentLine("hubspot-token", []byte(`{"status":"ok","replaces":true}`)); !strings.Contains(got, "Secret card sent: hubspot-token") || !strings.Contains(got, "replaces it") {
		t.Fatalf("line = %q", got)
	}
	if got := secretSentLine("hubspot-token", []byte(`{"status":"ok"}`)); strings.Contains(got, "replaces it") {
		t.Fatalf("line = %q", got)
	}
}
