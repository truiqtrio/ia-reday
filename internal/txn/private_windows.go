//go:build windows

package txn

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const privateSecurityInformationFlags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION

func openExclusivePrivate(path string) (*os.File, error) {
	sd, err := privateCurrentUserSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		0,
		&sa,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func applyPrivateFileSecurity(path string) error { return applyPrivateSecurity(path) }

func applyPrivateDirSecurity(path string) error { return applyPrivateSecurity(path) }

func applyPrivateSecurity(path string) error {
	sd, err := privateCurrentUserSecurityDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, privateSecurityInformationFlags, nil, nil, dacl, nil)
}

func captureFileSecurity(path string) (string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	descriptor := sd.String()
	if descriptor == "" {
		return "", errors.New("txn: convert Windows DACL to SDDL")
	}
	return descriptor, nil
}

func restoreFileSecurity(path, descriptor string, _ os.FileMode) error {
	sd, err := windows.SecurityDescriptorFromString(descriptor)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		flags = windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil)
}

func privateFileSecurityValid(path string) (bool, error) {
	got, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	control, _, err := got.Control()
	if err != nil {
		return false, err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false, nil
	}
	want, err := privateCurrentUserSecurityDescriptor()
	if err != nil {
		return false, err
	}
	gotDescriptor, wantDescriptor := got.String(), want.String()
	if gotDescriptor == "" || wantDescriptor == "" {
		return false, errors.New("txn: convert Windows DACL to SDDL")
	}
	gotACEs, gotOK := daclACEList(gotDescriptor)
	wantACEs, wantOK := daclACEList(wantDescriptor)
	return gotOK && wantOK && gotACEs == wantACEs, nil
}

func privateFileSnapshot() (os.FileMode, string, error) {
	sd, err := privateCurrentUserSecurityDescriptor()
	if err != nil {
		return 0, "", err
	}
	descriptor := sd.String()
	if descriptor == "" {
		return 0, "", errors.New("txn: convert Windows DACL to SDDL")
	}
	return 0o600, descriptor, nil
}

// Windows access control is represented by Security, not FileMode.Perm.
func fileModeMatches(os.FileMode, os.FileMode) bool { return true }

func privateCurrentUserSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	current, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("txn: determine current Windows user: %w", err)
	}
	if current.Uid == "" {
		return nil, errors.New("txn: current Windows user SID is empty")
	}
	return windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + current.Uid + ")")
}

func daclACEList(descriptor string) (string, bool) {
	dacl := strings.Index(descriptor, "D:")
	if dacl < 0 {
		return "", false
	}
	aces := descriptor[dacl+2:]
	start := strings.IndexByte(aces, '(')
	if start < 0 {
		return "", false
	}
	aces = aces[start:]
	if sacl := strings.Index(aces, "S:"); sacl >= 0 {
		aces = aces[:sacl]
	}
	return aces, true
}

// Windows does not expose the POSIX directory-fsync primitive. File contents
// are flushed before atomic replacement; MoveFileEx supplies the rename step.
func syncDir(string) error { return nil }
