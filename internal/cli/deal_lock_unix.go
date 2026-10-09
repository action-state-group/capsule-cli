//go:build unix

package cli

import (
	"errors"
	"os"
	"syscall"
)

// lockDealStore takes an exclusive advisory lock beside the profile's SQLite
// file, so concurrent writers (sub-agents included) seal one deal step at a
// time and each step chains to the one before it.
func lockDealStore(dbPath string) (func() error, error) {
	f, e := os.OpenFile(dbPath+".deal.lock", os.O_RDWR|os.O_CREATE, 0o600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		return nil, errors.Join(e, f.Close())
	}
	return func() error {
		return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
	}, nil
}

// lockDealStoreShared takes a shared advisory lock on the store's lock file
// for a read, so it never sees a step half sealed, without creating the file:
// a store no writer has locked yet is read without one.
func lockDealStoreShared(dbPath string) (func() error, error) {
	f, e := os.Open(dbPath + ".deal.lock")
	if errors.Is(e, os.ErrNotExist) {
		return func() error { return nil }, nil
	}
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_SH); e != nil {
		return nil, errors.Join(e, f.Close())
	}
	return func() error {
		return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
	}, nil
}
