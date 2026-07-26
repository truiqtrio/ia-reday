//go:build windows

package txn

import "os"

func openExclusivePrivate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// Owner ruling #13: Windows uses the OS-default DACL. The txn engine does not
// set, snapshot, restore, or compare Windows ACLs; CAS is content-only there.
func applyPrivateFileSecurity(string) error { return nil }

func applyPrivateDirSecurity(string) error { return nil }

func ensureJournalStateDir(path string) error { return os.MkdirAll(path, 0o700) }

func captureFileSecurity(string) (string, error) { return "", nil }

func restoreFileSecurity(string, string, os.FileMode) error { return nil }

func privateFileSecurityValid(string) (bool, error) { return true, nil }

func privateFileSnapshot() (os.FileMode, string, error) { return 0o600, "", nil }

func fileModeMatches(os.FileMode, os.FileMode) bool { return true }

func syncDir(string) error { return nil }
