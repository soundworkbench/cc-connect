package claudecode

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Shape verified against the real subscription endpoint; percentages are fixture data.
const scopedAPIUsage = `{"five_hour":{"utilization":28,"resets_at":"2026-09-11T18:20:00Z"},"seven_day":{"utilization":60,"resets_at":"2026-09-12T21:00:00Z"},"limits":[{"kind":"weekly_all","percent":60},{"kind":"weekly_scoped","percent":66,"resets_at":"2026-09-12T21:00:00Z","scope":{"model":{"id":null,"display_name":"Fable"}}}]}`

func TestUsageAPIParsesIndependentModelLimit(t *testing.T) {
	now := time.Date(2026, 9, 11, 17, 0, 0, 0, time.UTC)
	report, err := parseClaudeUsageAPI([]byte(scopedAPIUsage), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Buckets) != 2 || len(report.Buckets[0].Windows) != 2 || report.Buckets[1].Name != "Fable" {
		t.Fatalf("wrong quota groups: %+v", report)
	}
	window := report.Buckets[1].Windows[0]
	if window.UsedPercent != 66 || window.WindowSeconds != 604800 || window.ResetAfterSeconds != 100800 {
		t.Fatalf("model quota was confused with general quota: %+v", window)
	}
}

func TestUsageAPIOptionalAndInvalidWindows(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
		buckets    int
	}{
		{"weekly only", `{"seven_day":{"utilization":0}}`, true, 1},
		{"scoped only", `{"limits":[{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"Fable"}}}]}`, true, 1},
		{"missing scope not invented", `{"seven_day":{"utilization":12},"limits":[{"kind":"weekly_scoped","percent":42}]}`, true, 1},
		{"no fabricated model", `{"five_hour":{"utilization":12},"limits":[]}`, true, 1},
		{"null percent", `{"seven_day":{"utilization":null}}`, false, 0},
		{"empty payload", `{}`, false, 0},
		{"malformed", `{`, false, 0},
		{"negative", `{"five_hour":{"utilization":-1}}`, false, 0},
		{"over 100", `{"five_hour":{"utilization":101}}`, false, 0},
		{"invalid reset", `{"five_hour":{"utilization":1,"resets_at":"bad"}}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := parseClaudeUsageAPI([]byte(tc.body), time.Now())
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected validity: %v", err)
			}
			if err == nil && len(report.Buckets) != tc.buckets {
				t.Fatalf("unexpected buckets: %+v", report)
			}
		})
	}
}

func TestUsageAPITokenSelectionDoesNotCrossProviders(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"fixture-file-token"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CLAUDE_CONFIG_DIR": directory}
	token, err := claudeUsageToken(env)
	if err != nil || token != "fixture-file-token" {
		t.Fatal("configured credential directory ignored")
	}
	env["CLAUDE_CODE_OAUTH_TOKEN"] = "fixture-env-token"
	token, err = claudeUsageToken(env)
	if err != nil || token != "fixture-env-token" {
		t.Fatal("explicit OAuth token ignored")
	}
	env["ANTHROPIC_AUTH_TOKEN"] = "fixture-other-provider"
	if _, err := claudeUsageToken(env); err == nil {
		t.Fatal("custom provider must not use local subscription")
	}
}

func TestUsageAPIHTTPAndRedactedErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("anthropic-beta") == "" || r.Method != "GET" {
			t.Error("incorrect usage request")
		}
		if r.URL.Path == "/denied" {
			w.WriteHeader(401)
			fmt.Fprint(w, "fixture-secret-response")
			return
		}
		fmt.Fprint(w, scopedAPIUsage)
	}))
	defer server.Close()
	report, err := requestClaudeUsage(context.Background(), server.Client(), server.URL, "fixture-token")
	if err != nil || len(report.Buckets) != 2 {
		t.Fatalf("request failed: %v", err)
	}
	_, err = requestClaudeUsage(context.Background(), server.Client(), server.URL+"/denied", "fixture-token")
	if err == nil || strings.Contains(err.Error(), "fixture") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("response body or credentials leaked: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := requestClaudeUsage(ctx, server.Client(), server.URL, "fixture-token"); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}
