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
	"syscall"
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

// Mutation caught: re-probing a PGID after this controller already observed
// its group gone can signal an unrelated process group that reused the number.
func TestCaptchaBrowserCloseNeverRevivesObservedGoneProcessGroup(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("7", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var waitCalls atomic.Int32
	var terminateCalls atomic.Int32
	var removeCalls atomic.Int32
	exited := make(chan struct{})
	close(exited)
	controller := &chromeBrowserController{
		cmd:         &exec.Cmd{Process: &os.Process{Pid: 4242}},
		profileRoot: root,
		profileDir:  profileDir,
		exited:      exited,
		closeDevTools: func(context.Context, string) error {
			return errors.New("DevTools unavailable")
		},
		// Both probes in the first Close observe the owned group gone. Any
		// later true result represents a reused, unrelated PGID.
		waitProcessExit: func(*os.Process, <-chan struct{}, time.Duration) bool {
			return waitCalls.Add(1) <= 2
		},
		terminateProcess: func(*os.Process, <-chan struct{}) error {
			terminateCalls.Add(1)
			return errors.New("would signal reused process group")
		},
		removeProfile: func(profileRoot, profileDir string) error {
			if removeCalls.Add(1) == 1 {
				return errors.New("temporary profile lock")
			}
			return removeCaptchaChromeProfile(profileRoot, profileDir)
		},
	}

	firstErr := controller.Close()
	if firstErr == nil || captchaBrowserMayBeRunning(firstErr) {
		t.Fatalf("first Close error=%v, want gone-group profile failure", firstErr)
	}
	if err := controller.Close(); err != nil {
		t.Fatalf("retry Close after PGID reuse: %v", err)
	}
	if got := terminateCalls.Load(); got != 0 {
		t.Fatalf("signaled reused process group %d time(s)", got)
	}
	if got := waitCalls.Load(); got > 2 {
		t.Fatalf("re-probed observed-gone PGID %d time(s)", got)
	}
	if got := removeCalls.Load(); got != 2 {
		t.Fatalf("profile cleanup calls=%d, want 2", got)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("profile remains after retry: %v", err)
	}
}

// Mutation caught: wiring Chrome itself as the process-group leader allows its
// numeric PGID to disappear and be reused before a delayed first Close.
func TestDefaultCaptchaControllerUsesDurableUnixGroupGuardian(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("4", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	controllerValue, err := startChromeLogged(cmd, root, profileDir)
	if err != nil {
		t.Fatalf("start production-wired controller: %v", err)
	}
	controller := controllerValue.(*chromeBrowserController)
	controller.closeDevTools = func(context.Context, string) error { return errors.New("close through owned group") }
	chromePID := cmd.Process.Pid
	groupPID, err := syscall.Getpgid(chromePID)
	if err != nil {
		t.Fatalf("get Chrome process group: %v", err)
	}
	t.Cleanup(func() {
		_ = controller.Close()
		_ = syscall.Kill(-groupPID, syscall.SIGKILL)
	})

	if groupPID == chromePID {
		t.Fatalf("Chrome PID %d is still the reusable process-group leader", chromePID)
	}
	guardianGroup, err := syscall.Getpgid(groupPID)
	if err != nil {
		t.Fatalf("stable guardian PID %d is not alive: %v", groupPID, err)
	}
	if guardianGroup != groupPID {
		t.Fatalf("guardian PID %d belongs to group %d", groupPID, guardianGroup)
	}

	unrelated := exec.Command("sleep", "30")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatalf("start unrelated process group: %v", err)
	}
	unrelatedPID := unrelated.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-unrelatedPID, syscall.SIGKILL)
		_, _ = unrelated.Process.Wait()
	})
	if err := controller.Close(); err != nil {
		t.Fatalf("delayed first Close: %v", err)
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("delayed Close signaled unrelated group: %v", err)
	}
}

// Mutation caught: starting the guardian but failing to attach it to the
// controller's failed-launch cleanup leaks a stable process group indefinitely.
func TestDefaultCaptchaLaunchFailureReclaimsUnixGroupGuardian(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("3", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "missing-chrome"))
	controller, err := startChromeLogged(cmd, root, profileDir)
	if err == nil {
		t.Fatal("missing Chrome command unexpectedly started")
	}
	if controller != nil {
		t.Fatalf("failed launch retained controller %T", controller)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Pgid <= 0 {
		t.Fatal("failed launch was not prepared with a stable guardian")
	}
	if alive, probeErr := captchaProcessGroupExists(cmd.SysProcAttr.Pgid); probeErr != nil || alive {
		t.Fatalf("failed launch retained guardian group: alive=%v err=%v", alive, probeErr)
	}
	if _, statErr := os.Stat(profileDir); !os.IsNotExist(statErr) {
		t.Fatalf("failed launch retained owned profile: %v", statErr)
	}
}

// Mutation caught: allowing a stable owner to repeat its terminal group signal
// can target a replacement after the guardian and original group are gone.
func TestStableUnixGroupOwnerNeverSignalsTwiceAfterFailedTerminalWait(t *testing.T) {
	var signalCalls atomic.Int32
	owner := trackCaptchaProcessOwnership(
		func(time.Duration) bool { return false },
		func(func(time.Duration) bool) error {
			signalCalls.Add(1)
			return errors.New("group did not disappear after terminal signal")
		},
	)
	owner.singleUseTermination = true
	if err := owner.terminate(); err == nil {
		t.Fatal("first terminal signal unexpectedly reported exit")
	}
	if err := owner.terminate(); err == nil {
		t.Fatal("retry unexpectedly reported exit")
	}
	if got := signalCalls.Load(); got != 1 {
		t.Fatalf("terminal group signals=%d, want exactly 1", got)
	}
}

// Mutation caught: reverting the default production owner to the leader exit
// channel leaves a real child alive in the launched process group and removes
// the profile out from under it.
func TestDefaultCaptchaControllerOwnsWholeUnixProcessGroup(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("6", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sleep 30 >/dev/null 2>&1 & exec sleep 0.4")
	controllerValue, err := startChromeLogged(cmd, root, profileDir)
	if err != nil {
		t.Fatalf("start production-wired controller: %v", err)
	}
	controller := controllerValue.(*chromeBrowserController)
	pid := cmd.Process.Pid
	groupPID, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("get owned process group: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-groupPID, syscall.SIGKILL)
		select {
		case <-controller.exited:
		case <-time.After(2 * time.Second):
		}
		_ = os.RemoveAll(root)
	})

	select {
	case <-controller.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("process-group leader did not exit")
	}
	if alive, probeErr := captchaProcessGroupExists(groupPID); probeErr != nil || !alive {
		t.Fatalf("child process group not alive after leader exit: alive=%v err=%v", alive, probeErr)
	}
	controller.closeDevTools = func(context.Context, string) error { return errors.New("leader already exited") }

	if err := controller.Close(); err != nil {
		t.Fatalf("close production-wired process group: %v", err)
	}
	if alive, probeErr := captchaProcessGroupExists(groupPID); probeErr != nil || alive {
		t.Fatalf("owned child process group remains after Close: alive=%v err=%v", alive, probeErr)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("owned profile remains after group exit: %v", err)
	}
}
