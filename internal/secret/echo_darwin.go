//go:build darwin

package secret

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

func disableEcho(input *os.File) (func() error, error) {
	if input == nil {
		return nil, errors.New("nil terminal")
	}
	fd := input.Fd()
	var original syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&original))); errno != 0 {
		return nil, errno
	}

	disabled := original
	disabled.Lflag &^= syscall.ECHO
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSETA, uintptr(unsafe.Pointer(&disabled))); errno != 0 {
		return nil, errno
	}

	return func() error {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSETA, uintptr(unsafe.Pointer(&original)))
		if errno != 0 {
			return errno
		}
		return nil
	}, nil
}
