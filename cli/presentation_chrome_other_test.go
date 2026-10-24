//go:build !unix

package cli

import "os/exec"

// chromeProcessGroup: no process groups here; Chrome's own process is all
// the test stops.
func chromeProcessGroup(*exec.Cmd) {}

func killChromeProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
