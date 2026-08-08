//go:build unix

package authweb

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const captchaProcessTerminateTimeout = time.Second
const captchaProcessGroupPollInterval = 10 * time.Millisecond
const captchaProcessGuardianArgument = "--valorant-internal-captcha-process-group-guardian"
const captchaProcessGuardianEnvironment = "VALORANT_INTERNAL_CAPTCHA_PROCESS_GROUP_GUARDIAN"

func init() {
	if len(os.Args) == 2 && os.Args[1] == captchaProcessGuardianArgument && os.Getenv(captchaProcessGuardianEnvironment) == "1" {
		signal.Ignore(syscall.SIGTERM)
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	if len(os.Args) >= 2 && os.Args[1] == captchaChromeExecArgument {
		if err := runCaptchaChromeExecHelper(os.Args[1:], os.Environ(), defaultCaptchaChromeExecSystem); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "captcha Chrome exec helper: %v\n", err)
		}
		os.Exit(126)
	}
}

func prepareCaptchaProcess(cmd *exec.Cmd) (*captchaProcessOwnership, error) {
	guardianRead, guardianWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create CAPTCHA process guardian pipe: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		_ = guardianRead.Close()
		_ = guardianWrite.Close()
		return nil, fmt.Errorf("resolve CAPTCHA process guardian executable: %w", err)
	}
	guardian := exec.Command(executable, captchaProcessGuardianArgument)
	guardian.Env = []string{captchaProcessGuardianEnvironment + "=1"}
	guardian.Stdin = guardianRead
	guardian.Stdout = io.Discard
	guardian.Stderr = io.Discard
	guardian.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := guardian.Start(); err != nil {
		_ = guardianRead.Close()
		_ = guardianWrite.Close()
		return nil, fmt.Errorf("start CAPTCHA process guardian: %w", err)
	}
	_ = guardianRead.Close()
	guardianExited := make(chan struct{})
	go func() {
		_ = guardian.Wait()
		close(guardianExited)
	}()

	owner := rawCaptchaProcessOwnership(guardian.Process, guardianExited)
	owner.stableGroup = true
	owner.singleUseTermination = true
	owner.releaseRaw = guardianWrite.Close
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: guardian.Process.Pid}
	if isCaptchaChromeExecCommand(cmd) {
		cmd.Env = setCaptchaEnvironment(cmd.Env, captchaChromeExecPGIDEnvironment, strconv.Itoa(guardian.Process.Pid))
	}
	return owner, nil
}

func completeCaptchaProcessOwnership(prepared *captchaProcessOwnership, _ *os.Process, _ <-chan struct{}) *captchaProcessOwnership {
	return prepared
}

func newCaptchaProcessOwnership(process *os.Process, exited <-chan struct{}) *captchaProcessOwnership {
	return rawCaptchaProcessOwnership(process, exited)
}

func rawCaptchaProcessOwnership(process *os.Process, exited <-chan struct{}) *captchaProcessOwnership {
	return trackCaptchaProcessOwnership(
		func(timeout time.Duration) bool {
			return waitForCaptchaOwnedProcessExit(process, exited, timeout)
		},
		func(waitForExit func(time.Duration) bool) error {
			return terminateCaptchaProcessWithWait(process, waitForExit)
		},
	)
}

func terminateCaptchaProcess(process *os.Process, exited <-chan struct{}) error {
	return rawCaptchaProcessOwnership(process, exited).terminate()
}

func terminateCaptchaProcessWithWait(process *os.Process, waitForExit func(time.Duration) bool) error {
	if process == nil || waitForExit(0) {
		return nil
	}
	termErr := syscall.Kill(-process.Pid, syscall.SIGTERM)
	if errors.Is(termErr, syscall.ESRCH) {
		termErr = nil
	}
	if waitForExit(captchaProcessTerminateTimeout) {
		return termErr
	}
	killErr := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(killErr, syscall.ESRCH) {
		killErr = nil
	}
	if !waitForExit(captchaProcessTerminateTimeout) {
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
