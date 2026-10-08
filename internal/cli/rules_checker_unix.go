//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel runs the checker in its own process group and kills the
// whole group when the check gives up on it, so a child it started cannot
// keep it alive past its timeout.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
