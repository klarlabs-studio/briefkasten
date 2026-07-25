//go:build !windows

package maildir

import (
	"errors"
	"os"
	"syscall"
)

// acquireLockFile takes an exclusive advisory lock on path, creating the
// file if needed. ok is false only when block is false and another process
// holds the lock. Releasing is closing the returned file: flock is bound
// to the open file description, so the kernel drops the lock when the
// process exits, crash included.
func acquireLockFile(path string, block bool) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- path is the store's own lock file
	if err != nil {
		return nil, false, err
	}
	how := syscall.LOCK_EX
	if !block {
		how |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		// A signal interrupting the wait is not contention; keep waiting.
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err == nil {
			return f, true, nil
		}
		_ = f.Close()
		if !block && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EACCES)) {
			return nil, false, nil
		}
		return nil, false, err
	}
}
