//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

// chromeProcessGroup starts Chrome in a process group of its own, so the
// test can stop every process it starts.
func chromeProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killChromeProcessGroup kills every process left in Chrome's group.
func killChromeProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
