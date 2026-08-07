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

func terminateCaptchaProcess(process *os.Process, exited <-chan struct{}) error {
	if process == nil || waitForCaptchaProcessExit(exited, 0) {
		return nil
	}
	if err := process.Kill(); err != nil {
		if waitForCaptchaProcessExit(exited, 0) {
			return nil
		}
		return err
	}
	if !waitForCaptchaProcessExit(exited, captchaProcessTerminateTimeout) {
		return fmt.Errorf("captcha Chrome process did not exit")
	}
	return nil
}
