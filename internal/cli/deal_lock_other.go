//go:build !unix

package cli

// lockDealStore fails closed where no advisory file lock is implemented: deal
// steps must never be sealed without serializing writers.
func lockDealStore(string) (func() error, error) {
	return nil, inputError("deal commands are supported on Linux and macOS only")
}

func lockDealStoreShared(string) (func() error, error) {
	return nil, inputError("deal commands are supported on Linux and macOS only")
}
