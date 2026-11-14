//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup runs cmd in its own process group, so a timeout stops
// termux-call-log and the termux-api helper it starts, not just the script.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
