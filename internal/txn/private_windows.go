//go:build windows

package txn

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	sddlRevision1                   = 1
	seFileObject                    = 1
	daclSecurityInformation         = 0x00000004
	protectedDACLInformation        = 0x80000000
	unprotectedDACLInformation      = 0x20000000
	seDACLProtected                 = 0x1000
	privateSecurityInformationFlags = daclSecurityInformation | protectedDACLInformation
)

var (
	advapi32                              = syscall.NewLazyDLL("advapi32.dll")
	procConvertStringSecurityDescriptor   = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procConvertSecurityDescriptorToString = advapi32.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	procGetNamedSecurityInfo              = advapi32.NewProc("GetNamedSecurityInfoW")
	procGetSecurityDescriptorControl      = advapi32.NewProc("GetSecurityDescriptorControl")
	procSetFileSecurity                   = advapi32.NewProc("SetFileSecurityW")
)

func openExclusivePrivate(path string) (*os.File, error) {
	descriptor, err := privateCurrentUserDACL()
	if err != nil {
		return nil, err
	}
	sd, free, err := securityDescriptorFromString(descriptor)
	if err != nil {
		return nil, err
	}
	defer free()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa := syscall.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	handle, err := syscall.CreateFile(
		name,
		syscall.GENERIC_WRITE,
		0,
		&sa,
		syscall.CREATE_NEW,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	runtime.KeepAlive(sa)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func applyPrivateFileSecurity(path string) error {
	descriptor, err := privateCurrentUserDACL()
	if err != nil {
		return err
	}
	return applySecurityDescriptor(path, descriptor, privateSecurityInformationFlags)
}

func applyPrivateDirSecurity(path string) error {
	descriptor, err := privateCurrentUserDACL()
	if err != nil {
		return err
	}
	return applySecurityDescriptor(path, descriptor, privateSecurityInformationFlags)
}

func captureFileSecurity(path string) (string, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var sd uintptr
	result, _, _ := procGetNamedSecurityInfo.Call(
		uintptr(unsafe.Pointer(name)),
		seFileObject,
		daclSecurityInformation,
		0,
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&sd)),
	)
	if result != 0 {
		return "", syscall.Errno(result)
	}
	defer localFree(sd)
	return securityDescriptorString(sd)
}

func restoreFileSecurity(path, descriptor string, _ os.FileMode) error {
	sd, free, err := securityDescriptorFromString(descriptor)
	if err != nil {
		return err
	}
	defer free()
	protected, err := daclIsProtected(sd)
	if err != nil {
		return err
	}
	flags := uintptr(daclSecurityInformation | unprotectedDACLInformation)
	if protected {
		flags = daclSecurityInformation | protectedDACLInformation
	}
	return setFileSecurityDescriptor(path, sd, flags)
}

func privateFileSecurityValid(path string) (bool, error) {
	got, err := captureFileSecurity(path)
	if err != nil {
		return false, err
	}
	descriptor, err := privateCurrentUserDACL()
	if err != nil {
		return false, err
	}
	wantSD, free, err := securityDescriptorFromString(descriptor)
	if err != nil {
		return false, err
	}
	defer free()
	want, err := securityDescriptorString(wantSD)
	if err != nil {
		return false, err
	}
	return got == want, nil
}

func privateFileSnapshot() (os.FileMode, string, error) {
	descriptor, err := privateCurrentUserDACL()
	if err != nil {
		return 0, "", err
	}
	sd, free, err := securityDescriptorFromString(descriptor)
	if err != nil {
		return 0, "", err
	}
	defer free()
	canonical, err := securityDescriptorString(sd)
	return 0o600, canonical, err
}

// Windows access control is represented by Security, not FileMode.Perm.
func fileModeMatches(os.FileMode, os.FileMode) bool { return true }

func privateCurrentUserDACL() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("txn: determine current Windows user: %w", err)
	}
	if current.Uid == "" {
		return "", errors.New("txn: current Windows user SID is empty")
	}
	return "D:P(A;;FA;;;" + current.Uid + ")", nil
}

func applySecurityDescriptor(path, descriptor string, flags uintptr) error {
	sd, free, err := securityDescriptorFromString(descriptor)
	if err != nil {
		return err
	}
	defer free()
	return setFileSecurityDescriptor(path, sd, flags)
}

func setFileSecurityDescriptor(path string, sd, flags uintptr) error {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	r1, _, callErr := procSetFileSecurity.Call(
		uintptr(unsafe.Pointer(name)),
		flags,
		sd,
	)
	runtime.KeepAlive(name)
	if r1 == 0 {
		return nonzeroWindowsError(callErr)
	}
	return nil
}

func daclIsProtected(sd uintptr) (bool, error) {
	var control uint16
	var revision uint32
	r1, _, callErr := procGetSecurityDescriptorControl.Call(
		sd,
		uintptr(unsafe.Pointer(&control)),
		uintptr(unsafe.Pointer(&revision)),
	)
	if r1 == 0 {
		return false, nonzeroWindowsError(callErr)
	}
	return control&seDACLProtected != 0, nil
}

func securityDescriptorFromString(descriptor string) (uintptr, func(), error) {
	text, err := syscall.UTF16PtrFromString(descriptor)
	if err != nil {
		return 0, nil, err
	}
	var sd uintptr
	r1, _, callErr := procConvertStringSecurityDescriptor.Call(
		uintptr(unsafe.Pointer(text)),
		sddlRevision1,
		uintptr(unsafe.Pointer(&sd)),
		0,
	)
	runtime.KeepAlive(text)
	if r1 == 0 {
		return 0, nil, nonzeroWindowsError(callErr)
	}
	return sd, func() { localFree(sd) }, nil
}

func securityDescriptorString(sd uintptr) (string, error) {
	var text *uint16
	var length uint32
	r1, _, callErr := procConvertSecurityDescriptorToString.Call(
		sd,
		sddlRevision1,
		daclSecurityInformation,
		uintptr(unsafe.Pointer(&text)),
		uintptr(unsafe.Pointer(&length)),
	)
	if r1 == 0 {
		return "", nonzeroWindowsError(callErr)
	}
	defer localFree(uintptr(unsafe.Pointer(text)))
	return syscall.UTF16ToString(unsafe.Slice(text, length)), nil
}

func localFree(pointer uintptr) {
	if pointer != 0 {
		_, _ = syscall.LocalFree(syscall.Handle(pointer))
	}
}

func nonzeroWindowsError(err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return errors.New("txn: Windows security API failed")
	}
	return err
}

// Windows does not expose the POSIX directory-fsync primitive. File contents
// are flushed before atomic replacement; MoveFileEx supplies the rename step.
func syncDir(string) error { return nil }
