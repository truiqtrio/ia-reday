//go:build windows

package secret

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const enableEchoInput = 0x0004

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode = kernel32.NewProc("GetConsoleMode")
	setConsoleMode = kernel32.NewProc("SetConsoleMode")
)

func disableEcho(input *os.File) (func() error, error) {
	if input == nil {
		return nil, errors.New("nil terminal")
	}
	fd := input.Fd()
	var original uint32
	if result, _, err := getConsoleMode.Call(fd, uintptr(unsafe.Pointer(&original))); result == 0 {
		return nil, fmt.Errorf("GetConsoleMode: %w", err)
	}
	if result, _, err := setConsoleMode.Call(fd, uintptr(original&^enableEchoInput)); result == 0 {
		return nil, fmt.Errorf("SetConsoleMode: %w", err)
	}

	return func() error {
		if result, _, err := setConsoleMode.Call(fd, uintptr(original)); result == 0 {
			return fmt.Errorf("SetConsoleMode restore: %w", err)
		}
		return nil
	}, nil
}
