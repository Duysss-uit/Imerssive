//go:build windows

package worker

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// hideWindow prevents a console window from flashing up for the sidecar.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
