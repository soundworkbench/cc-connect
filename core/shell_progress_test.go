package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func runShellEntry(e *Engine, p Platform, mode, command, workDir string) error {
	switch mode {
	case "timer":
		return e.executeTimerShell(p, "ctx", &TimerJob{Exec: command, WorkDir: workDir})
	case "cron":
		return e.executeCronShell(p, "ctx", &CronJob{Exec: command, WorkDir: workDir})
	default:
		return e.runShellWithProgress(p, "ctx", command, workDir, 5*time.Second, 4000)
	}
}

type shellRoutePlatform struct {
	stubPlatformEngine
	routeMu sync.Mutex
	routes  []string
}

func (p *shellRoutePlatform) Send(ctx context.Context, replyCtx any, text string) error {
	p.routeMu.Lock()
	p.routes = append(p.routes, "send")
	p.routeMu.Unlock()
	return p.stubPlatformEngine.Send(ctx, replyCtx, text)
}
func (p *shellRoutePlatform) Reply(ctx context.Context, replyCtx any, text string) error {
	p.routeMu.Lock()
	p.routes = append(p.routes, "reply")
	p.routeMu.Unlock()
	return p.stubPlatformEngine.Reply(ctx, replyCtx, text)
}

func TestShellEntries_PreserveOutputAndRouting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	for _, mode := range []string{"normal", "cron", "timer"} {
		for _, tc := range []struct {
			name, command, want string
			failed, progress    bool
		}{
			{"stdout_stderr", "printf 'first\\n'; printf 'second\\n' >&2", "second", false, false},
			{"empty", "true", "(no output)", false, false},
			{"failed", "exit 7", "exit code 7", true, false},
			{"long", "printf 'before\\n'; sleep 0.7; printf 'after\\n'", "after", false, true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				p := &shellRoutePlatform{stubPlatformEngine: stubPlatformEngine{n: "shell-test"}}
				e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
				defer e.Stop()
				err := runShellEntry(e, p, mode, tc.command, t.TempDir())
				if (err != nil) != tc.failed {
					t.Fatalf("err=%v, failed=%t", err, tc.failed)
				}
				sent := p.getSent()
				wantCount := 1
				if tc.progress {
					wantCount = 2
				}
				if len(sent) != wantCount {
					t.Fatalf("sent=%v", sent)
				}
				final := sent[len(sent)-1]
				if !strings.Contains(final, tc.want) {
					t.Fatalf("final=%q", final)
				}
				if (mode != "normal") != strings.HasPrefix(final, "⏰ ") {
					t.Fatalf("scheduled marker: %q", final)
				}
				if (mode == "normal") != strings.Contains(final, "```") {
					t.Fatalf("code fence policy: %q", final)
				}
				wantRoute := "send"
				if mode == "normal" {
					wantRoute = "reply"
				}
				if p.routes[len(p.routes)-1] != wantRoute {
					t.Fatalf("routes=%v, want final %s", p.routes, wantRoute)
				}
			})
		}
	}
}

type shellWorkDirAgent struct {
	stubAgent
	dir string
}

func (a *shellWorkDirAgent) GetWorkDir() string { return a.dir }

func TestShellEntries_PreserveShellSelectionAndWorkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	for _, mode := range []string{"normal", "cron", "timer"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			p := &stubPlatformEngine{n: "shell-test"}
			e := NewEngine("test", &shellWorkDirAgent{dir: dir}, []Platform{p}, "", LangEnglish)
			defer e.Stop()
			workDir := ""
			if mode == "normal" {
				workDir = dir
			}
			if err := runShellEntry(e, p, mode, "pwd", workDir); err != nil {
				t.Fatal(err)
			}
			sent := p.getSent()
			if !strings.Contains(sent[len(sent)-1], dir) {
				t.Fatalf("workdir output=%v", sent)
			}
			e.shell = filepath.Join(dir, "nonexistent-shell")
			err := runShellEntry(e, p, mode, "true", workDir)
			if mode == "timer" && err != nil {
				t.Fatalf("timer must use system shell: %v", err)
			}
			if mode != "timer" && err == nil {
				t.Fatal("configured shell must be used")
			}
		})
	}
}

func TestShellEntries_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	for _, mode := range []string{"normal", "cron", "timer"} {
		t.Run(mode, func(t *testing.T) {
			p := &stubPlatformEngine{n: "shell-test"}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
			defer e.Stop()
			ctx, cancel := context.WithTimeout(e.ctx, 100*time.Millisecond)
			defer cancel()
			e.ctx = ctx
			err := runShellEntry(e, p, mode, "exec sleep 3", t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("err=%v", err)
			}
			sent := p.getSent()
			if len(sent) != 1 || !strings.Contains(sent[0], "timeout") {
				t.Fatalf("sent=%v", sent)
			}
		})
	}
}

type blockedShellUpdater struct {
	mockUpdaterPlatform
	marker           string
	entered, release chan struct{}
	once             sync.Once
}

func (p *blockedShellUpdater) UpdateMessage(ctx context.Context, handle any, text string) error {
	if strings.Contains(text, "⏳") {
		p.once.Do(func() {
			_ = os.WriteFile(p.marker, []byte("finish"), 0600)
			close(p.entered)
			<-p.release
		})
	}
	return p.mockUpdaterPlatform.UpdateMessage(ctx, handle, text)
}

func TestShellEntries_FinalCannotBeOverwrittenByLateProgress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	for _, mode := range []string{"normal", "cron", "timer"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			p := &blockedShellUpdater{marker: filepath.Join(dir, "finish"), entered: make(chan struct{}), release: make(chan struct{})}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
			defer e.Stop()
			done := make(chan error, 1)
			go func() {
				done <- runShellEntry(e, p, mode, "while [ ! -f finish ]; do sleep 0.01; done; printf done", dir)
			}()
			select {
			case <-p.entered:
			case <-time.After(4 * time.Second):
				close(p.release)
				t.Fatal("periodic preview did not start")
			}
			var early bool
			var err error
			select {
			case err = <-done:
				early = true
			case <-time.After(200 * time.Millisecond):
			}
			close(p.release)
			if !early {
				select {
				case err = <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("shell did not finish")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if early {
				t.Error("final sent while an older progress update was still in flight")
			}
			messages := p.getMessages()
			if len(messages) < 3 || !strings.Contains(messages[len(messages)-1], "✅") {
				t.Fatalf("last write must be final: %v", messages)
			}
		})
	}
}
