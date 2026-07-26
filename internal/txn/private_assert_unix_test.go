//go:build !windows

package txn

import (
	"os"
	"testing"
)

func assertPrivateDirectorySecurity(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode(%s) = %#o, want 0700", path, info.Mode().Perm())
	}
}
