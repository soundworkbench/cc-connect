package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTimerCLIProcess(t *testing.T) {
	if os.Getenv("TIMERTZ_CLI_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			runTimer(os.Args[i+1:])
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func timerCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestTimerCLIProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "TIMERTZ_CLI_HELPER=1", "CC_PROJECT=test", "CC_SESSION_KEY=test:chat:user")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestTimerAdd_RejectsAtWithoutTimezoneBeforeIPC(t *testing.T) {
	for _, at := range []string{"", "2026-10-13T07:45", "2026-10-13T07:45:00", "2026-10-13 07:45", "2h"} {
		t.Run(at, func(t *testing.T) {
			out, err := timerCLI(t, "add", "--at", at, "--prompt", "check", "--data-dir", t.TempDir())
			want := "--at 必须带时区偏移，例如 2026-10-13T22:45+08:00（北京）或 +09:00（东京）；也可用 --delay"
			if err == nil || !strings.Contains(out, want) || strings.Contains(out, "socket not found") {
				t.Fatalf("exit=%v, output=%q; want timezone rejection before IPC", err, out)
			}
		})
	}
}

func TestTimerCLI_OffsetCreationAndBeijingList(t *testing.T) {
	dir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "tz-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", resolveSocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/timer/list" {
			_, _ = w.Write([]byte(`[{"id":"tz1","scheduled_at":"2099-10-13T20:45:00Z","prompt":"check"},{"id":"tz2","scheduled_at":"2099-10-13T00:15:00+09:00","prompt":"check"}]`))
			return
		}
		var body struct {
			Delay string `json:"delay"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad JSON", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "tz1", "scheduled_at": body.Delay, "prompt": "check"})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	for _, tc := range []struct{ at, beijing string }{
		{"2099-10-13T22:45+08:00", "10-13 22:45"},
		{"2099-10-13T22:45:00+08:00", "10-13 22:45"},
		{"2099-10-13T00:15+09:00", "10-12 23:15"},
		{"2099-10-13T00:15:00+09:00", "10-12 23:15"},
		{"2099-10-13T20:45Z", "10-14 04:45"},
		{"2099-10-13T20:45:00Z", "10-14 04:45"},
	} {
		out, err := timerCLI(t, "add", "--at", tc.at, "--prompt", "check", "--data-dir", dir)
		if err != nil || !strings.Contains(out, "Fires at: "+tc.at) || !strings.Contains(out, "北京时间: "+tc.beijing) {
			t.Errorf("at=%s exit=%v output=%q", tc.at, err, out)
		}
	}
	out, err := timerCLI(t, "list", "--data-dir", dir)
	if err != nil || !strings.Contains(out, "北京时间: 10-14 04:45") || !strings.Contains(out, "北京时间: 10-12 23:15") {
		t.Fatalf("list exit=%v output=%q", err, out)
	}
}
