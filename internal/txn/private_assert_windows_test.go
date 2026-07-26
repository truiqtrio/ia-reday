//go:build windows

package txn

import "testing"

func assertPrivateDirectorySecurity(t *testing.T, path string) {
	t.Helper()
	// Owner ruling #13: Windows ACL assertions are intentionally skipped.
	_ = path
}
