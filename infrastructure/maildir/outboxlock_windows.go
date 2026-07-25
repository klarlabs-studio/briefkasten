//go:build windows

package maildir

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// errSharingViolation is what Windows reports when a handle is already
// open without sharing — the platform's way of saying "locked".
const errSharingViolation = syscall.Errno(32)

// lockPoll is how often a blocking acquisition retries. Windows offers no
// wait-for-open, so contention is polled rather than queued by the kernel.
const lockPoll = 25 * time.Millisecond

// acquireLockFile opens path with sharing disabled, which is Windows'
// equivalent of an advisory lock: no second handle can be opened until
// this one closes, and the kernel closes it when the process ends, crash
// included. ok is false only when block is false and the file is held.
func acquireLockFile(path string, block bool) (*os.File, bool, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	for {
		handle, err := syscall.CreateFile(
			name,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			0, // no sharing — this is the lock
			nil,
			syscall.OPEN_ALWAYS,
			syscall.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err == nil {
			return os.NewFile(uintptr(handle), path), true, nil
		}
		if !errors.Is(err, errSharingViolation) && !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return nil, false, err
		}
		if !block {
			return nil, false, nil
		}
		time.Sleep(lockPoll)
	}
}
