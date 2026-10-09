package claudecode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/chenhg5/cc-connect/core"
	"github.com/creack/pty"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

func (a *Agent) GetUsage(ctx context.Context) (*core.UsageReport, error) {
	if report, err := a.fetchClaudeUsageAPI(ctx); err == nil {
		return report, nil
	} else {
		slog.Debug("claude usage API unavailable; trying configured CLI", "error", err)
	}
	screen, err := a.runClaudeUsageProbe(ctx)
	if err != nil {
		return nil, err
	}
	// The CLI can refresh an expired OAuth session. Retry with the newly
	// written credential before falling back to its reduced terminal panel.
	if report, err := a.fetchClaudeUsageAPI(ctx); err == nil {
		return report, nil
	} else {
		slog.Debug("claude usage API unavailable after CLI probe", "error", err)
	}
	return parseClaudeUsageReport(screen, time.Now())
}

func (a *Agent) runClaudeUsageProbe(ctx context.Context) (string, error) {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	workDir, err := os.MkdirTemp("", "cc-connect-claude-usage-*")
	if err != nil {
		return "", fmt.Errorf("claudecode: create usage temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	args := []string{
		"--tools", "",
		"--permission-mode", "plan",
		"--no-chrome",
		// /usage is built in. Starting unrelated MCP services slows the probe
		// and can execute user-configured startup code unnecessarily.
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
	}
	// Usage must use the same configured executable, wrapper arguments and
	// provider environment as normal requests (including per-process proxies).
	a.mu.Lock()
	bin := a.cmd
	allArgs := append([]string(nil), a.cliExtraArgs...)
	if a.cmdArgsFlag != "" {
		allArgs = append(allArgs, a.cmdArgsFlag, shellJoinArgs(args))
	} else {
		allArgs = append(allArgs, args...)
	}
	a.mu.Unlock()
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return "", &core.UserMessageError{Key: core.MsgUsageCLIUnavailable, Args: []any{bin}}
	}
	cmd := exec.CommandContext(probeCtx, bin, allArgs...)
	cmd.Dir = workDir

	env := filterEnv(os.Environ(), "CLAUDECODE")
	env = append(env, "DISABLE_TELEMETRY=true")
	env = append(env, "DISABLE_COST_WARNINGS=true")
	if extra := a.usageProbeEnv(); len(extra) > 0 {
		env = core.MergeEnv(env, extra)
	}
	cmd.Env = env

	var stderr claudeUsageStderr
	cmd.Stderr = &stderr

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		return "", fmt.Errorf("claudecode: start claude usage probe: %w", err)
	}

	var waitErr error
	processDone := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(processDone)
	}()

	terminal := newClaudeUsageTerminal()
	readDone := make(chan error, 1)
	readerFinished := make(chan struct{})
	go func() {
		defer close(readerFinished)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				terminal.Write(buf[:n])
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EIO) {
					readDone <- nil
				} else {
					readDone <- err
				}
				return
			}
		}
	}()

	defer func() {
		if err := ptmx.Close(); err != nil {
			slog.Warn("claudeSession: close ptmx", "error", err)
		}
		cancel()
		// Wait for reader goroutine to finish so it is never leaked.
		// The select below may already have consumed readDone. A separate closed
		// channel keeps early EOF / process failure from hanging cleanup forever.
		<-readerFinished
		select {
		case <-processDone:
		case <-time.After(2 * time.Second):
			if cmd.Process != nil {
				if err := cmd.Process.Kill(); err != nil {
					slog.Warn("claudeSession: kill process", "error", err)
				}
			}
			<-processDone
		}
	}()

	ticker := time.NewTicker(claudeUsagePollInterval)
	defer ticker.Stop()

	var (
		state       claudeUsageProbeState
		lastScreen  string
		lastChange  = time.Now()
		usageScreen string
	)

	for {
		select {
		case <-probeCtx.Done():
			if usageScreen != "" {
				return usageScreen, nil
			}
			if screenErr := detectClaudeUsageOutputError(lastScreen, stderr.String()); screenErr != nil {
				return "", screenErr
			}
			return "", fmt.Errorf("claudecode: timed out waiting for Claude Code /usage panel: %w", probeCtx.Err())
		case err := <-readDone:
			readDone = nil
			if err != nil {
				return "", fmt.Errorf("claudecode: read Claude Code /usage output: %w", err)
			}
		case <-processDone:
			lastScreen = normalizeClaudeUsageText(terminal.String())
			if usageScreen != "" {
				return usageScreen, nil
			}
			if screenErr := detectClaudeUsageOutputError(lastScreen, stderr.String()); screenErr != nil {
				return "", screenErr
			}
			if waitErr != nil {
				return lastScreen, fmt.Errorf("claudecode: Claude Code exited before /usage rendered: %w", waitErr)
			}
			return lastScreen, fmt.Errorf("claudecode: Claude Code exited before /usage rendered")
		case <-ticker.C:
			screen := normalizeClaudeUsageText(terminal.String())
			if screen != lastScreen {
				lastScreen = screen
				lastChange = time.Now()
			}
			if usageReady(lastScreen) {
				usageScreen = lastScreen
			}
			if screenErr := detectClaudeUsageOutputError(lastScreen, stderr.String()); screenErr != nil {
				return "", screenErr
			}
			if usageScreen != "" && time.Since(lastChange) >= claudeUsageStableFor {
				return usageScreen, nil
			}
			// A partial terminal frame can briefly omit the selected default.
			// Never press Enter until the dialog has finished rendering.
			if time.Since(lastChange) < claudeUsageStableFor {
				continue
			}
			if action := nextClaudeUsageProbeAction(lastScreen, &state, time.Now()); action != "" {
				if _, err := io.WriteString(ptmx, action); err != nil {
					return "", fmt.Errorf("claudecode: write Claude Code /usage probe input: %w", err)
				}
			}
		}
	}
}

// os/exec copies stderr concurrently with the panel polling loop.
type claudeUsageStderr struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *claudeUsageStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *claudeUsageStderr) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (a *Agent) usageProbeEnv() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runtimeEnvLocked()
}
