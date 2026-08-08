//go:build windows

package authweb

import "os/exec"

func chromeCommand(bin string, args []string) (*exec.Cmd, error) {
	return exec.Command(bin, args...), nil
}
