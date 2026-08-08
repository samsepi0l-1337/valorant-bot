//go:build unix

package authweb

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

func prepareChromeDevToolsPipe(cmd *exec.Cmd) (*chromeDevToolsPipeSetup, error) {
	if cmd == nil {
		return nil, errors.New("Chrome command is unavailable")
	}
	if len(cmd.ExtraFiles) != 0 {
		return nil, errors.New("private Chrome DevTools requires unused file descriptors 3 and 4")
	}
	childRead, hostWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create Chrome DevTools command pipe: %w", err)
	}
	hostRead, childWrite, err := os.Pipe()
	if err != nil {
		_ = childRead.Close()
		_ = hostWrite.Close()
		return nil, fmt.Errorf("create Chrome DevTools response pipe: %w", err)
	}
	cmd.ExtraFiles = []*os.File{childRead, childWrite}
	return &chromeDevToolsPipeSetup{
		host:       newChromeDevToolsPipe(hostRead, hostWrite),
		childRead:  childRead,
		childWrite: childWrite,
	}, nil
}
