//go:build !unix

package cli

import "os/exec"

// killGroupOnCancel kills only the checker itself where process groups are
// not available; WaitDelay still bounds the wait for its output.
func killGroupOnCancel(cmd *exec.Cmd) {}
