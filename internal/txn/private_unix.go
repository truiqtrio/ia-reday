//go:build !windows

package txn

import (
	"fmt"
	"os"
)

func openExclusivePrivate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func applyPrivateFileSecurity(path string) error { return os.Chmod(path, 0o600) }

func applyPrivateDirSecurity(path string) error { return os.Chmod(path, 0o700) }

func captureFileSecurity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04o", info.Mode().Perm()), nil
}

func restoreFileSecurity(path, _ string, mode os.FileMode) error {
	return os.Chmod(path, mode.Perm())
}

func privateFileSecurityValid(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm() == 0o600, nil
}

func privateFileSnapshot() (os.FileMode, string, error) { return 0o600, "0600", nil }

func fileModeMatches(current, expected os.FileMode) bool {
	return current.Perm() == expected.Perm()
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil {
		return err
	}
	return closeErr
}
