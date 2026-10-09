package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageProbeHonorsConfiguredWrapperAndArgs(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "configured-wrapper")
	script := `#!/bin/sh
test "$1" = wrapper-argument || exit 42
shift
test "$1" = --tools || exit 43
printf 'Current session\n12%% used\nResets 3pm (UTC)\nCurrent week\n34%% used\nResets Sep 12, 3pm (UTC)\n'
while read line; do :; done
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	// No default claude binary exists. The configured wrapper is authoritative.
	t.Setenv("PATH", dir)
	a := &Agent{cmd: bin, cliExtraArgs: []string{"wrapper-argument"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	report, err := a.GetUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Buckets[0].Windows[0].UsedPercent != 12 {
		t.Fatalf("wrong usage report: %+v", report)
	}
}

func TestUsageProbeOnboardingIsNotReportedAsTimeout(t *testing.T) {
	for _, screen := range []string{"Select login method:", "Choose the text style that looks best with your terminal", "Let's get started. Select a theme"} {
		err := detectClaudeUsageOutputError(screen, "")
		if err == nil || !strings.Contains(err.Error(), "onboarding") {
			t.Errorf("screen %q: got %v, want actionable onboarding error", screen, err)
		}
	}
}

func TestUsageProbeTrustDefaultExitSelectsKnownEmptyDirectory(t *testing.T) {
	screen := "Quick safety check\n❯ No, exit\n  Yes, I trust this folder\nEnter to confirm"
	if got := promptActionForScreen(screen); got != "\x1b[B\r" {
		t.Fatalf("got %q; Enter alone exits the CLI", got)
	}
	state := &claudeUsageProbeState{}
	now := time.Now()
	if got := nextClaudeUsageProbeAction(screen, state, now); got != "\x1b[B\r" {
		t.Fatalf("first action %q", got)
	}
	if got := nextClaudeUsageProbeAction(screen, state, now.Add(time.Second)); got != "" {
		t.Fatalf("repeated action in unchanged dialog: %q", got)
	}
}

func TestUsageTerminalAbsoluteColumnsAndSplitSequences(t *testing.T) {
	raw := "\x1b[2GCurrent\x1b[10Gsession\r\n\x1b[2G12%\x1b[6Gused\r\n\x1b[2GResets\x1b[9G3pm\x1b[13G(UTC)\r\n\x1b[2GCurrent\x1b[10Gweek\r\n\x1b[2G34%\x1b[6Gused\r\n\x1b[2GResets\x1b[9GSep 12, 3pm (UTC)\r\n"
	terminal := newClaudeUsageTerminal()
	for _, b := range []byte(raw) {
		terminal.Write([]byte{b})
	}
	text := normalizeClaudeUsageText(terminal.String())
	if !usageReady(text) {
		t.Fatalf("lost absolute-column word spacing: %q", text)
	}
	report, err := parseClaudeUsageReport(text, time.Now())
	if err != nil || report.Buckets[0].Windows[0].UsedPercent != 12 {
		t.Fatalf("report=%v error=%v", report, err)
	}
}

func TestUsageProbeReadinessRequiresBothWindows(t *testing.T) {
	partial := "Current session\n12% used\nResets 3pm (UTC)\nCurrent week\nLoading"
	if usageReady(partial) {
		t.Fatal("partial week panel must not be cached as a complete report")
	}
	if !usageReady(partial + "\n34% used\nResets Sep 12, 3pm (UTC)") {
		t.Fatal("complete panel was not recognized")
	}
}

func TestUsageLiveConfiguredCommand(t *testing.T) {
	bin := os.Getenv("CC_USAGE_LIVE_COMMAND")
	if bin == "" {
		t.Skip("explicit live acceptance only")
	}
	a := &Agent{cmd: bin}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	screen, err := a.runClaudeUsageProbe(ctx)
	if err != nil {
		for _, line := range strings.Split(screen, "\n") {
			if strings.Contains(line, "trust") || strings.Contains(line, "No,") || strings.Contains(line, "Yes,") || strings.Contains(line, "error:") {
				t.Logf("diagnostic confirmation: %q", line)
			}
		}
		t.Fatal(err)
	}
	report, err := parseClaudeUsageReport(screen, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Buckets) != 1 || len(report.Buckets[0].Windows) != 2 {
		t.Fatal("usage windows absent")
	}
	for _, window := range report.Buckets[0].Windows {
		t.Logf("verified quota name=%s used_percent=%d reset_unix=%d", window.Name, window.UsedPercent, window.ResetAtUnix)
	}
}

func TestUsageProbeEarlyExitDoesNotHangCleanup(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 9\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	a := &Agent{cmd: bin}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.GetUsage(ctx); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("exited CLI must report failure")
		}
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("usage cleanup hung after consuming reader completion")
	}
}
