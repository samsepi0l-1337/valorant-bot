//go:build unix

package authweb

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const captchaProcessTerminateTimeout = time.Second
const captchaProcessGroupPollInterval = 10 * time.Millisecond

func configureCaptchaProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateCaptchaProcess(process *os.Process, exited <-chan struct{}) error {
	if process == nil || waitForCaptchaOwnedProcessExit(process, exited, 0) {
		return nil
	}
	termErr := syscall.Kill(-process.Pid, syscall.SIGTERM)
	if errors.Is(termErr, syscall.ESRCH) {
		termErr = nil
	}
	if waitForCaptchaOwnedProcessExit(process, exited, captchaProcessTerminateTimeout) {
		return termErr
	}
	killErr := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(killErr, syscall.ESRCH) {
		killErr = nil
	}
	if !waitForCaptchaOwnedProcessExit(process, exited, captchaProcessTerminateTimeout) {
		killErr = errors.Join(killErr, fmt.Errorf("captcha Chrome process group did not exit"))
	}
	return errors.Join(termErr, killErr)
}

func waitForCaptchaOwnedProcessExit(process *os.Process, exited <-chan struct{}, timeout time.Duration) bool {
	return waitForCaptchaProcessGroupExitWithProbe(
		process,
		exited,
		timeout,
		captchaProcessGroupPollInterval,
		captchaProcessGroupExists,
	)
}

// waitForCaptchaProcessGroupExitWithProbe deliberately does not treat the
// leader's exited channel as proof that the owned process group is gone. Chrome
// wrappers may exit while a child continues using the owned profile.
func waitForCaptchaProcessGroupExitWithProbe(
	process *os.Process,
	_ <-chan struct{},
	timeout time.Duration,
	pollInterval time.Duration,
	groupExists func(int) (bool, error),
) bool {
	if process == nil {
		return true
	}
	if pollInterval <= 0 {
		pollInterval = captchaProcessGroupPollInterval
	}
	deadline := time.Now().Add(timeout)
	for {
		exists, err := groupExists(process.Pid)
		if err == nil && !exists {
			return true
		}
		if timeout <= 0 {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		if remaining < pollInterval {
			time.Sleep(remaining)
		} else {
			time.Sleep(pollInterval)
		}
	}
}

func captchaProcessGroupExists(pid int) (bool, error) {
	err := syscall.Kill(-pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		// Unknown probe errors are treated as possibly alive by callers.
		return true, err
	}
}
