//go:build windows

package authweb

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

const captchaProcessTerminateTimeout = 2 * time.Second

func configureCaptchaProcess(_ *exec.Cmd) {}

func newCaptchaProcessOwnership(process *os.Process, exited <-chan struct{}) *captchaProcessOwnership {
	return trackCaptchaProcessOwnership(
		func(timeout time.Duration) bool {
			return waitForCaptchaOwnedProcessExit(process, exited, timeout)
		},
		func(waitForExit func(time.Duration) bool) error {
			return terminateCaptchaProcessWithWait(process, waitForExit)
		},
	)
}

func waitForCaptchaOwnedProcessExit(_ *os.Process, exited <-chan struct{}, timeout time.Duration) bool {
	return waitForCaptchaProcessExit(exited, timeout)
}

func terminateCaptchaProcess(process *os.Process, exited <-chan struct{}) error {
	return newCaptchaProcessOwnership(process, exited).terminate()
}

func terminateCaptchaProcessWithWait(process *os.Process, waitForExit func(time.Duration) bool) error {
	if process == nil || waitForExit(0) {
		return nil
	}
	if err := process.Kill(); err != nil {
		if waitForExit(0) {
			return nil
		}
		return err
	}
	if !waitForExit(captchaProcessTerminateTimeout) {
		return fmt.Errorf("captcha Chrome process did not exit")
	}
	return nil
}
