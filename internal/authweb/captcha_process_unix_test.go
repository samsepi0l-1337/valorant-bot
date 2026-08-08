//go:build unix

package authweb

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Mutation caught: consulting the leader's closed exited channel before the
// process-group probe makes this return without ever observing a live child.
func TestWaitForCaptchaProcessGroupExitIgnoresLeaderExitWhileGroupLives(t *testing.T) {
	leaderExited := make(chan struct{})
	close(leaderExited)
	var probes atomic.Int32
	process := &os.Process{Pid: 4242}

	exited := waitForCaptchaProcessGroupExitWithProbe(
		process,
		leaderExited,
		100*time.Millisecond,
		time.Millisecond,
		func(int) (bool, error) {
			return probes.Add(1) < 3, nil
		},
	)
	if !exited {
		t.Fatal("process group disappearance was not observed")
	}
	if got := probes.Load(); got < 3 {
		t.Fatalf("process-group probes=%d, want at least 3 despite exited leader", got)
	}
}

// Mutation caught: replacing the controller's owned-group wait seam with only
// waitForCaptchaProcessExit removes the profile while a child remains alive.
func TestCaptchaBrowserCloseRetriesAfterLeaderExitWhileGroupLives(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("9", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(profileDir, "child-still-owns-profile")
	if err := os.WriteFile(marker, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	leaderExited := make(chan struct{})
	close(leaderExited)
	groupAlive := atomic.Bool{}
	groupAlive.Store(true)
	var terminateCalls atomic.Int32
	var removeCalls atomic.Int32
	controller := &chromeBrowserController{
		cmd:         &exec.Cmd{Process: &os.Process{Pid: 4242}},
		profileRoot: root,
		profileDir:  profileDir,
		exited:      leaderExited,
		closeDevTools: func(context.Context, string) error {
			return errors.New("leader exited before DevTools close")
		},
		terminateProcess: func(*os.Process, <-chan struct{}) error {
			terminateCalls.Add(1)
			return errors.New("process group still running")
		},
		waitProcessExit: func(*os.Process, <-chan struct{}, time.Duration) bool {
			return !groupAlive.Load()
		},
		removeProfile: func(profileRoot, profileDir string) error {
			removeCalls.Add(1)
			return removeCaptchaChromeProfile(profileRoot, profileDir)
		},
	}

	firstErr := controller.Close()
	if firstErr == nil || !captchaBrowserMayBeRunning(firstErr) {
		t.Fatalf("first Close error=%v, want retryable live-group failure", firstErr)
	}
	if terminateCalls.Load() != 1 {
		t.Fatalf("terminate calls=%d, want 1 while group remains", terminateCalls.Load())
	}
	if removeCalls.Load() != 0 {
		t.Fatalf("profile removal calls=%d while group alive, want 0", removeCalls.Load())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("owned profile changed while child remained alive: %v", err)
	}

	groupAlive.Store(false)
	if err := controller.Close(); err != nil {
		t.Fatalf("retry Close after group exit: %v", err)
	}
	if removeCalls.Load() != 1 {
		t.Fatalf("profile removal calls=%d after group exit, want 1", removeCalls.Load())
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("owned profile remains after complete group exit: %v", err)
	}
}
