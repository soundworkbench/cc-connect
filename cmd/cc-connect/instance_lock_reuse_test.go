//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestKillExistingInstanceUnheldStalePIDDoesNotKill(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	if err := os.WriteFile(filepath.Join(dir, ".config.toml.lock"), []byte(fmt.Sprintf("%d\n", child.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	if KillExistingInstance(cfg) {
		t.Fatal("stale unlocked PID file killed an unrelated live child")
	}
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("child was killed despite no live instance lock")
	}
	lock, err := AcquireInstanceLock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
}

func TestKillExistingInstanceNeverKillsSelf(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.toml")
	lock, err := AcquireInstanceLock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if KillExistingInstance(cfg) {
		t.Fatal("must not target the current process")
	}
}

func TestKillExistingInstanceLiveHolderStillWorks(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.toml")
	child := exec.Command(os.Args[0], "-test.run=^TestInstanceLockHolderHelper$")
	child.Env = append(os.Environ(), "CC_LOCK_HELPER_CONFIG="+cfg)
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "LOCK_READY\n" {
		t.Fatalf("helper not ready: %q %v", line, err)
	}
	if !KillExistingInstance(cfg) {
		t.Fatal("live lock holder was not stopped")
	}
}

func TestInstanceLockHolderHelper(t *testing.T) {
	cfg := os.Getenv("CC_LOCK_HELPER_CONFIG")
	if cfg == "" {
		t.Skip("helper process only")
	}
	lock, err := AcquireInstanceLock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	fmt.Println("LOCK_READY")
	time.Sleep(30 * time.Second)
}
