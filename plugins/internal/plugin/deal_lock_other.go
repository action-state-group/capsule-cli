//go:build !unix

package plugin

import "github.com/action-state-group/capsule-cli/cli"

// lockDealStore fails closed where no advisory file lock is implemented: deal
// steps must never be sealed without serializing writers.
func lockDealStore(string) (func() error, error) {
	return nil, cli.InputError("deal commands are supported on Linux and macOS only")
}
