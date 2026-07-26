//go:build windows

package adapters

import (
	"fmt"
	"os/user"
	"strings"

	"golang.org/x/sys/windows"
)

func privatePathError(path string) error {
	current, err := user.Current()
	if err != nil {
		return err
	}
	want, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + current.Uid + ")")
	if err != nil {
		return err
	}
	got, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	control, _, err := got.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("DACL is not protected")
	}
	gotACEs, gotOK := testDACLACEList(got.String())
	wantACEs, wantOK := testDACLACEList(want.String())
	if !gotOK || !wantOK || gotACEs != wantACEs {
		return fmt.Errorf("DACL is not exactly current-user FullControl")
	}
	return nil
}

func testDACLACEList(descriptor string) (string, bool) {
	dacl := strings.Index(descriptor, "D:")
	if dacl < 0 {
		return "", false
	}
	aces := descriptor[dacl+2:]
	start := strings.IndexByte(aces, '(')
	if start < 0 {
		return "", false
	}
	return aces[start:], true
}
