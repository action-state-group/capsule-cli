//go:build unix

package cli

import (
	"os"
	"syscall"
	"time"
)

// acquire takes the key's lock: an flock on the lock file, which the system
// releases when the run ends, however it ends, so a killed run leaves nothing
// behind and two runs never hold it together.
func (p *commitmentPins) acquire() error {
	f, err := os.OpenFile(p.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	for waited := time.Duration(0); ; waited += 100 * time.Millisecond {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			p.lockFile = f
			return nil
		} else if err != syscall.EWOULDBLOCK {
			f.Close()
			return err
		}
		if waited >= 5*time.Second {
			f.Close()
			return InputError("another run holds " + p.lock + " for longer than 5 seconds")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (p *commitmentPins) unlock() {
	_ = syscall.Flock(int(p.lockFile.Fd()), syscall.LOCK_UN)
	_ = p.lockFile.Close()
}
