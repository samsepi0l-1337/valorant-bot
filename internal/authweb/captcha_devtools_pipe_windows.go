//go:build windows

package authweb

import (
	"errors"
	"os/exec"
)

func prepareChromeDevToolsPipe(_ *exec.Cmd) (*chromeDevToolsPipeSetup, error) {
	return nil, errors.New("private Chrome DevTools pipe is unavailable on Windows; use Riot Mobile QR")
}
