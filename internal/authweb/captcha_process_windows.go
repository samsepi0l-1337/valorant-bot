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

func waitForCaptchaOwnedProcessExit(_ *os.Process, exited <-chan struct{}, timeout time.Duration) bool {
	return waitForCaptchaProcessExit(exited, timeout)
}

func terminateCaptchaProcess(process *os.Process, exited <-chan struct{}) error {
	if process == nil || waitForCaptchaOwnedProcessExit(process, exited, 0) {
		return nil
	}
	if err := process.Kill(); err != nil {
		if waitForCaptchaOwnedProcessExit(process, exited, 0) {
			return nil
		}
		return err
	}
	if !waitForCaptchaOwnedProcessExit(process, exited, captchaProcessTerminateTimeout) {
		return fmt.Errorf("captcha Chrome process did not exit")
	}
	return nil
}
