//go:build !windows

package worker

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}
