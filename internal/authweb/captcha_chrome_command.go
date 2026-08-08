package authweb

import (
	"fmt"
	"os/exec"
)

type captchaChromePlatformCommand struct {
	cmd                 *exec.Cmd
	goos                string
	environment         []string
	internalEnvironment []string
}

// chromeCommand is the single command-environment boundary on every platform.
// Platform files choose only the executable/wrapper and environment source;
// this function always replaces Cmd.Env with a new explicit allowlist before
// returning it to the launcher.
func chromeCommand(bin string, args []string) (*exec.Cmd, error) {
	platformCommand, err := platformCaptchaChromeCommand(bin, args)
	if err != nil {
		return nil, err
	}
	if platformCommand.cmd == nil {
		return nil, fmt.Errorf("Chrome command is unavailable")
	}
	platformCommand.cmd.Env = allowlistedCaptchaDesktopEnvironment(platformCommand.goos, platformCommand.environment)
	platformCommand.cmd.Env = append(platformCommand.cmd.Env, platformCommand.internalEnvironment...)
	return platformCommand.cmd, nil
}
