//go:build !windows

package txn

import (
	"errors"
	"os"
	"syscall"
)

type fileLock struct{ file *os.File }

// Non-blocking flock provides one relay-install transaction per host user;
// the kernel releases it automatically if the process exits or crashes.
func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &fileLock{file: f}, nil
}

func (l *fileLock) Close() error {
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}
