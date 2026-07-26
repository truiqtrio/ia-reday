//go:build windows

package txn

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverPreservesExistingJournalStateDirectoryDACL(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	before := securitySnapshot(t, stateDir)
	if err := os.WriteFile(filepath.Join(stateDir, "t1.json"), []byte(`{"txn_id":"t1","state":"COMMITTED"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	e := NewFileEngine(Options{
		StateDir:   stateDir,
		BackupRoot: filepath.Join(root, "backups"),
		LockPath:   filepath.Join(root, "txn.lock"),
	})
	entries, err := e.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].TxnID != "t1" {
		t.Fatalf("Recover = %#v", entries)
	}
	assertSecuritySnapshot(t, stateDir, before)
}

func TestRecoverCreatesPrivateJournalStateDirectory(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "relay-install", "state")
	e := NewFileEngine(Options{
		StateDir:   stateDir,
		BackupRoot: filepath.Join(root, "backups"),
		LockPath:   filepath.Join(root, "txn.lock"),
	})

	entries, err := e.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Recover = %#v, want no entries", entries)
	}
	assertPrivateSecurity(t, stateDir)
}
