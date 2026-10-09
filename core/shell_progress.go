package core

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Entry points keep their existing shell selection and output policies.
func (e *Engine) executeTimerShell(p Platform, replyCtx any, job *TimerJob) error {
	return e.runScheduledShell(p, replyCtx, job.Exec, job.WorkDir, job.ExecutionTimeout(), true)
}
func (e *Engine) executeCronShell(p Platform, replyCtx any, job *CronJob) error {
	return e.runScheduledShell(p, replyCtx, job.Exec, job.WorkDir, job.ExecutionTimeout(), false)
}
func (e *Engine) runScheduledShell(p Platform, replyCtx any, command, workDir string, timeout time.Duration, timerShell bool) error {
	if workDir == "" {
		if wd, ok := e.agent.(interface{ GetWorkDir() string }); ok {
			workDir = wd.GetWorkDir()
		}
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	cmd := shellExecCommand(ctx, e.shell, e.shellFlag, e.shellProfile, command)
	// Timers historically use the system shell, unlike cron/user commands.
	if timerShell {
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", command)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "-c", command)
		}
	}
	cmd.Dir = workDir
	return e.runShellProcess(ctx, p, replyCtx, cmd, shellPresentation{label: truncateStr(command, 60), scheduled: true, timeout: timeout})
}
func (e *Engine) runShellWithProgress(p Platform, replyCtx any, command, workDir string, timeout time.Duration, maxOutput int) error {
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	cmd := shellExecCommand(ctx, e.shell, e.shellFlag, e.shellProfile, command)
	cmd.Dir = workDir
	return e.runShellProcess(ctx, p, replyCtx, cmd, shellPresentation{label: truncateStr(command, 60), maxOutput: maxOutput, timeout: timeout})
}

type shellPresentation struct {
	label     string
	scheduled bool
	maxOutput int
	timeout   time.Duration
}

func (s shellPresentation) progress(output string, timedOut bool) string {
	if s.scheduled {
		status := "⏳"
		if timedOut {
			status = "⚠️ timeout:"
		}
		msg := fmt.Sprintf("⏰ %s `%s`", status, s.label)
		if output != "" {
			msg += "\n\n" + truncateStr(output, 3000)
		}
		return msg
	}
	if timedOut {
		return fmt.Sprintf("⚠️ `%s` (timeout)\n```\n%s\n```", s.label, truncateRunes(output, s.maxOutput))
	}
	return fmt.Sprintf("⏳ `%s`\n```\n%s\n```", s.label, truncateRunes(output, s.maxOutput))
}
func (s shellPresentation) final(output string, exitCode int, waitErr error) (string, error) {
	display := strings.TrimSpace(output)
	if exitCode == 0 && display == "" {
		display = "(no output)"
	}
	if s.scheduled {
		if exitCode == 0 {
			return fmt.Sprintf("⏰ ✅ `%s`\n\n%s", s.label, truncateStr(display, 3000)), nil
		}
		msg := fmt.Sprintf("⏰ ❌ `%s`", s.label)
		if output != "" {
			msg += "\n\n" + truncateStr(output, 3000)
		}
		return fmt.Sprintf("%s\n\nerror: exit code %d", msg, exitCode), fmt.Errorf("shell: exit code %d", exitCode)
	}
	if exitCode == 0 {
		return fmt.Sprintf("✅ `%s`\n```\n%s\n```", s.label, truncateRunes(display, s.maxOutput)), nil
	}
	if display == "" && waitErr != nil {
		display = waitErr.Error()
	}
	return fmt.Sprintf("❌ `%s` (exit code %d)\n```\n%s\n```", s.label, exitCode, truncateRunes(display, s.maxOutput)), fmt.Errorf("exit code %d", exitCode)
}

// Both pipes drain before Wait; only the wait goroutine closes done.
func startShellOutput(cmd *exec.Cmd, stdout, stderr io.Reader) (snapshot func() string, done <-chan struct{}, waitError func() error) {
	var mu sync.Mutex
	var buf bytes.Buffer
	var waitErr error
	doneCh := make(chan struct{})
	readPipe := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
		for scanner.Scan() {
			mu.Lock()
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(scanner.Text())
			mu.Unlock()
		}
		if err := scanner.Err(); err != nil {
			slog.Warn("shell output read failed", "error", err)
		}
	}
	go func() {
		var readers sync.WaitGroup
		readers.Add(2)
		go func() { defer readers.Done(); readPipe(stdout) }()
		go func() { defer readers.Done(); readPipe(stderr) }()
		readers.Wait()
		waitErr = cmd.Wait()
		close(doneCh)
	}()
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}, doneCh, func() error { return waitErr } // Read only after receiving done.
}
func (e *Engine) runShellProcess(ctx context.Context, p Platform, replyCtx any, cmd *exec.Cmd, view shellPresentation) error {
	prefix := ""
	if view.scheduled {
		prefix = "shell: "
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%sstdout pipe: %w", prefix, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("%sstderr pipe: %w", prefix, err)
	}
	if err := cmd.Start(); err != nil {
		if view.scheduled {
			e.send(p, replyCtx, fmt.Sprintf("⏰ ❌ `%s`\nerror: failed to start: %v", view.label, err))
			return fmt.Errorf("shell: start: %w", err)
		}
		e.reply(p, replyCtx, fmt.Sprintf("❌ `%s`\n```\nfailed to start command: %v\n```", view.label, err))
		return err
	}
	output, done, waitError := startShellOutput(cmd, stdout, stderr)
	var handle any
	updater, canUpdate := p.(MessageUpdater)
	deliver := func(text string, final bool) {
		if handle != nil {
			if err := updater.UpdateMessage(e.ctx, handle, text); err != nil {
				slog.Warn("shell preview update failed", "error", err)
			}
		} else if final && !view.scheduled {
			e.reply(p, replyCtx, text)
		} else {
			e.send(p, replyCtx, text)
		}
	}
	finish := func(timedOut bool) error {
		if timedOut {
			killAndWait(cmd, done)
			deliver(view.progress(output(), true), false)
			if view.scheduled {
				return fmt.Errorf("shell command timed out")
			}
			return fmt.Errorf("command timed out after %s", view.timeout)
		}
		text, err := view.final(output(), cmd.ProcessState.ExitCode(), waitError())
		deliver(text, true)
		return err
	}
	quick := time.NewTimer(quickFinishTimeout)
	defer quick.Stop()
	select {
	case <-done:
		return finish(ctx.Err() != nil)
	case <-ctx.Done():
		return finish(true)
	case <-quick.C:
	}
	if starter, ok := p.(PreviewStarter); ok && canUpdate {
		preview, err := starter.SendPreviewStart(e.ctx, replyCtx, view.progress(output(), false))
		if err == nil {
			handle = preview
		}
	}
	if handle == nil {
		text := fmt.Sprintf("⏳ `%s`", view.label)
		if view.scheduled {
			text = "⏰ " + text
		}
		e.send(p, replyCtx, text)
	}
	// A single loop owns progress and final writes; no late updater can overwrite
	// the completed result.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return finish(ctx.Err() != nil)
		case <-ctx.Done():
			return finish(true)
		case <-ticker.C:
			if handle != nil {
				deliver(view.progress(output(), false), false)
			}
		}
	}
}
func killAndWait(cmd *exec.Cmd, done <-chan struct{}) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-done
}
