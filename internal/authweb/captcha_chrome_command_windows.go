//go:build windows

package authweb

import (
	"os"
	"os/exec"
)

func platformCaptchaChromeCommand(bin string, args []string) (captchaChromePlatformCommand, error) {
	return captchaChromePlatformCommand{
		cmd:         exec.Command(bin, args...),
		goos:        "windows",
		environment: os.Environ(),
	}, nil
}
