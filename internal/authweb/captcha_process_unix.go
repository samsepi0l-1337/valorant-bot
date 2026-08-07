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

func configureCaptchaProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateCaptchaProcess(process *os.Process, exited <-chan struct{}) error {
	if process == nil || waitForCaptchaProcessExit(exited, 0) {
		return nil
	}
	termErr := syscall.Kill(-process.Pid, syscall.SIGTERM)
	if errors.Is(termErr, syscall.ESRCH) {
		termErr = nil
	}
	if waitForCaptchaProcessExit(exited, captchaProcessTerminateTimeout) {
		return termErr
	}
	killErr := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(killErr, syscall.ESRCH) {
		killErr = nil
	}
	if !waitForCaptchaProcessExit(exited, captchaProcessTerminateTimeout) {
		killErr = errors.Join(killErr, fmt.Errorf("captcha Chrome process group did not exit"))
	}
	return errors.Join(termErr, killErr)
}
