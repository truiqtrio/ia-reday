//go:build windows

package txn

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

type fileLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

// LockFileEx supplies the Windows equivalent of non-blocking flock. The first
// byte is held exclusively for the lifetime of the open lock-file handle.
func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &fileLock{file: f}
	r1, _, callErr := procLockFileEx.Call(
		f.Fd(),
		lockfileExclusiveLock|lockfileFailImmediately,
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	runtime.KeepAlive(lock)
	if r1 == 0 {
		_ = f.Close()
		if errors.Is(callErr, errorLockViolation) {
			return nil, ErrLocked
		}
		return nil, callErr
	}
	return lock, nil
}

func (l *fileLock) Close() error {
	r1, _, callErr := procUnlockFileEx.Call(
		l.file.Fd(),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	runtime.KeepAlive(l)
	closeErr := l.file.Close()
	if r1 == 0 {
		return callErr
	}
	return closeErr
}
