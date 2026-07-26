//go:build windows

package txn

import "testing"

func assertPrivateDirectorySecurity(t *testing.T, path string) {
	t.Helper()
	assertPrivateSecurity(t, path)
}
