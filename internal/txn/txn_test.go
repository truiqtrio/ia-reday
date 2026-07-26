package txn

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relay-install/internal/contract"
	"relay-install/internal/secret"
)

func testEngine(t *testing.T, fault FaultInjector) (*FileEngine, Options) {
	t.Helper()
	root := t.TempDir()
	opts := Options{
		StateDir:      filepath.Join(root, "state"),
		BackupRoot:    filepath.Join(root, "backups"),
		LockPath:      filepath.Join(root, "lock"),
		FaultInjector: fault,
	}
	return NewFileEngine(opts), opts
}

func change(target, content string) Change {
	return Change{Target: target, Content: []byte(`"` + content + `"`), Perm: 0o600, ParserKind: contract.ParserJSON}
}

func writeMode(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	var err error
	if mode.Perm() == 0o600 {
		err = applyPrivateFileSecurity(path)
	} else {
		err = os.Chmod(path, mode)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func securitySnapshot(t *testing.T, path string) string {
	t.Helper()
	security, err := captureFileSecurity(path)
	if err != nil {
		t.Fatal(err)
	}
	return security
}

func assertSecuritySnapshot(t *testing.T, path, want string) {
	t.Helper()
	if got := securitySnapshot(t, path); got != want {
		t.Fatalf("security(%s) = %q, want %q", path, got, want)
	}
}

func assertPrivateSecurity(t *testing.T, path string) {
	t.Helper()
	valid, err := privateFileSecurityValid(path)
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatalf("security(%s) is not private", path)
	}
}

func TestApplyHardFailureRollsBackBytesAndPermissions(t *testing.T) {
	var first bool
	e, opts := testEngine(t, func(p FaultPoint) error {
		if p == FaultAfterRename && !first {
			first = true
			return errors.New("commit failed")
		}
		return nil
	})
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, "old-canary", 0o640)
	beforeSecurity := securitySnapshot(t, target)
	_, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(target, "new")}})
	if err == nil || errors.Is(err, ErrManualRecoveryRequired) {
		t.Fatalf("Apply error = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "old-canary" {
		t.Fatalf("restored content = %q, %v", got, err)
	}
	assertSecuritySnapshot(t, target, beforeSecurity)
	entries, err := e.Recover(context.Background())
	if err != nil || len(entries) != 1 || entries[0].State != StateRolledBack {
		t.Fatalf("journal = %#v, %v", entries, err)
	}
	assertPrivateDirectorySecurity(t, entries[0].BackupDir)
	assertPrivateSecurity(t, entries[0].Changes[0].Backup)
	_ = opts
}

func TestRecoverCrashBeforeCommitAndAfterRename(t *testing.T) {
	for _, point := range []FaultPoint{FaultBeforeCommit, FaultAfterRename} {
		t.Run(string(point), func(t *testing.T) {
			fired := false
			e, opts := testEngine(t, func(p FaultPoint) error {
				if p == point && !fired {
					fired = true
					return ErrCrashInjected
				}
				return nil
			})
			target := filepath.Join(t.TempDir(), "config")
			writeMode(t, target, "before", 0o640)
			beforeSecurity := securitySnapshot(t, target)
			_, err := e.Apply(context.Background(), Request{Client: "claudecode", Changes: []Change{change(target, "after")}})
			if !errors.Is(err, ErrCrashInjected) {
				t.Fatalf("Apply error = %v", err)
			}
			recovered := NewFileEngine(Options{StateDir: opts.StateDir, BackupRoot: opts.BackupRoot, LockPath: opts.LockPath})
			if point == FaultAfterRename {
				journals, err := filepath.Glob(filepath.Join(opts.StateDir, "*.json"))
				if err != nil || len(journals) != 1 {
					t.Fatalf("journals = %v, %v", journals, err)
				}
				entry, err := readJournal(journals[0])
				if err != nil {
					t.Fatal(err)
				}
				if got := securitySnapshot(t, target); got != entry.Changes[0].AfterSecurity {
					t.Fatalf("after-image security = %q, want target security %q", entry.Changes[0].AfterSecurity, got)
				}
			}
			entries, err := recovered.Recover(context.Background())
			if err != nil || len(entries) != 1 || entries[0].State != StateRolledBack {
				t.Fatalf("Recover = %#v, %v", entries, err)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "before" {
				t.Fatalf("recovered content = %q, %v", got, err)
			}
			assertSecuritySnapshot(t, target, beforeSecurity)
		})
	}
}

func TestCrashBeforeRestoreLeavesJournalForNewEngine(t *testing.T) {
	renamed := false
	e, opts := testEngine(t, func(p FaultPoint) error {
		if p == FaultAfterRename && !renamed {
			renamed = true
			return errors.New("force rollback")
		}
		if p == FaultBeforeRestore {
			return ErrCrashInjected
		}
		return nil
	})
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, "before", 0o600)
	_, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(target, "after")}})
	if !errors.Is(err, ErrCrashInjected) {
		t.Fatalf("Apply error = %v", err)
	}
	recovered := NewFileEngine(Options{StateDir: opts.StateDir, BackupRoot: opts.BackupRoot, LockPath: opts.LockPath})
	if _, err := recovered.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "before" {
		t.Fatalf("recovered content = %q, %v", got, err)
	}
}

func TestCrashAfterRestoreStageSyncRecoversAndCleansSecretStage(t *testing.T) {
	const before = "before-restore-stage-secret-canary"
	commitFailed := false
	restoreCrashed := false
	e, opts := testEngine(t, func(point FaultPoint) error {
		if point == FaultAfterRename && !commitFailed {
			commitFailed = true
			return errors.New("force rollback")
		}
		if point == FaultAfterRestoreStage && !restoreCrashed {
			restoreCrashed = true
			return ErrCrashInjected
		}
		return nil
	})
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, before, 0o640)
	beforeSecurity := securitySnapshot(t, target)
	if _, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(target, "after")}}); !errors.Is(err, ErrCrashInjected) {
		t.Fatalf("Apply error = %v", err)
	}
	journals, err := filepath.Glob(filepath.Join(opts.StateDir, "*.json"))
	if err != nil || len(journals) != 1 {
		t.Fatalf("journals = %v, %v", journals, err)
	}
	entry, err := readJournal(journals[0])
	if err != nil {
		t.Fatal(err)
	}
	restoreStage := entry.Changes[0].RestoreStage
	stageData, err := os.ReadFile(restoreStage)
	if err != nil || string(stageData) != before {
		t.Fatalf("restore stage = %q, %v", stageData, err)
	}
	assertPrivateSecurity(t, restoreStage)

	restarted := NewFileEngine(Options{StateDir: opts.StateDir, BackupRoot: opts.BackupRoot, LockPath: opts.LockPath})
	if _, err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != before {
		t.Fatalf("restored target = %q, %v", got, err)
	}
	assertSecuritySnapshot(t, target, beforeSecurity)
	if _, err := os.Stat(restoreStage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore stage remains: %v", err)
	}
	journalData, err := os.ReadFile(journals[0])
	if err != nil || strings.Contains(string(journalData), before) {
		t.Fatalf("journal leaked before-image: %q, %v", journalData, err)
	}
}

func TestRollbackCASPreservesExternalEditAfterRestoreStageSync(t *testing.T) {
	const before = "before-restore-cas-secret-canary"
	commitFailed := false
	externalWritten := false
	var target string
	e, opts := testEngine(t, func(point FaultPoint) error {
		if point == FaultAfterRename && !commitFailed {
			commitFailed = true
			return errors.New("force rollback")
		}
		if point == FaultAfterRestoreStage && !externalWritten {
			externalWritten = true
			writeMode(t, target, "external", 0o600)
		}
		return nil
	})
	target = filepath.Join(t.TempDir(), "config")
	writeMode(t, target, before, 0o640)
	_, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(target, "after")}})
	if !errors.Is(err, ErrManualRecoveryRequired) {
		t.Fatalf("Apply error = %v", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != "external" {
		t.Fatalf("external target = %q, %v", got, readErr)
	}
	journals, globErr := filepath.Glob(filepath.Join(opts.StateDir, "*.json"))
	if globErr != nil || len(journals) != 1 {
		t.Fatalf("journals = %v, %v", journals, globErr)
	}
	entry, readJournalErr := readJournal(journals[0])
	if readJournalErr != nil || entry.State != StateManualRecoveryRequired || !entry.Changes[0].Restoring {
		t.Fatalf("journal = %#v, %v", entry, readJournalErr)
	}
	stageData, stageErr := os.ReadFile(entry.Changes[0].RestoreStage)
	if stageErr != nil || string(stageData) != before {
		t.Fatalf("retained restore stage = %q, %v", stageData, stageErr)
	}
	if !strings.Contains(err.Error(), opts.BackupRoot) {
		t.Fatalf("manual recovery error lacks backup path: %v", err)
	}
}

func TestRecoverAfterCommittedCrashPreservesTarget(t *testing.T) {
	e, opts := testEngine(t, func(p FaultPoint) error {
		if p == FaultAfterCommitted {
			return ErrCrashInjected
		}
		return nil
	})
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, "before", 0o600)
	_, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(target, "after")}})
	if !errors.Is(err, ErrCrashInjected) {
		t.Fatalf("Apply error = %v", err)
	}
	recovered := NewFileEngine(Options{StateDir: opts.StateDir, BackupRoot: opts.BackupRoot, LockPath: opts.LockPath})
	entries, err := recovered.Recover(context.Background())
	if err != nil || len(entries) != 1 || entries[0].State != StateCommitted {
		t.Fatalf("Recover = %#v, %v", entries, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != `"after"` {
		t.Fatalf("committed target = %q, %v", got, err)
	}
}

func TestApplyBlocksUntilUnfinishedJournalIsRecovered(t *testing.T) {
	e, opts := testEngine(t, func(p FaultPoint) error {
		if p == FaultAfterRename {
			return ErrCrashInjected
		}
		return nil
	})
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, "before", 0o600)
	req := Request{Client: "codex", Changes: []Change{change(target, "after")}}
	if _, err := e.Apply(context.Background(), req); !errors.Is(err, ErrCrashInjected) {
		t.Fatalf("crash Apply error = %v", err)
	}
	restarted := NewFileEngine(Options{StateDir: opts.StateDir, BackupRoot: opts.BackupRoot, LockPath: opts.LockPath})
	if _, err := restarted.Apply(context.Background(), req); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Apply with unfinished journal error = %v", err)
	}
	if _, err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "before" {
		t.Fatalf("recovered target = %q, %v", got, err)
	}
}

func TestRecoverDoesNotOverwritePostCrashExternalChange(t *testing.T) {
	e, opts := testEngine(t, func(p FaultPoint) error {
		if p == FaultAfterRename {
			return ErrCrashInjected
		}
		return nil
	})
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, "before", 0o600)
	if _, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(target, "after")}}); !errors.Is(err, ErrCrashInjected) {
		t.Fatalf("crash Apply error = %v", err)
	}
	writeMode(t, target, "external-change", 0o600)
	restarted := NewFileEngine(Options{StateDir: opts.StateDir, BackupRoot: opts.BackupRoot, LockPath: opts.LockPath})
	if _, err := restarted.Recover(context.Background()); !errors.Is(err, ErrManualRecoveryRequired) {
		t.Fatalf("Recover error = %v, want manual recovery", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "external-change" {
		t.Fatalf("Recover overwrote external change: %q, %v", got, err)
	}
}

func TestCommitCASPreservesChangeMadeDuringValidation(t *testing.T) {
	e, _ := testEngine(t, nil)
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, `"before"`, 0o600)
	c := change(target, "after")
	c.PostValidate = func(string) error {
		writeMode(t, target, `"external"`, 0o600)
		return nil
	}
	if _, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{c}}); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("Apply error = %v, want target-changed error", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != `"external"` {
		t.Fatalf("commit overwrote concurrent change: %q, %v", got, err)
	}
}

func TestPostValidateCannotMutateStagedFile(t *testing.T) {
	e, _ := testEngine(t, nil)
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, `"before"`, 0o600)
	c := change(target, "after")
	c.PostValidate = func(stage string) error {
		if err := os.WriteFile(stage, []byte(`"tampered"`), 0o644); err != nil {
			return err
		}
		return os.Chmod(stage, 0o644)
	}
	if _, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{c}}); err == nil {
		t.Fatal("Apply accepted a mutated staged file")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != `"before"` {
		t.Fatalf("mutated stage reached target: %q, %v", got, err)
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".relay-install-stage-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("staged secret material remains: %v, %v", stages, err)
	}
}

func TestRunActionCrashRecoversGenericBeforeImages(t *testing.T) {
	e, opts := testEngine(t, func(p FaultPoint) error {
		if p == FaultAfterRename {
			return ErrCrashInjected
		}
		return nil
	})
	target := filepath.Join(t.TempDir(), "database")
	writeMode(t, target, "before-db", 0o600)
	_, err := e.RunAction(context.Background(), ActionRequest{
		Client:  "ccswitch",
		Targets: []string{target},
		IsNoop:  func(context.Context) (bool, error) { return false, nil },
		Execute: func(_ context.Context, backupDir string) error {
			if !filepath.IsAbs(backupDir) {
				t.Fatalf("relative backup dir: %s", backupDir)
			}
			return os.WriteFile(target, []byte("after-db"), 0o600)
		},
	})
	if !errors.Is(err, ErrCrashInjected) {
		t.Fatalf("RunAction error = %v", err)
	}
	recovered := NewFileEngine(Options{StateDir: opts.StateDir, BackupRoot: opts.BackupRoot, LockPath: opts.LockPath})
	entries, err := recovered.Recover(context.Background())
	if err != nil || len(entries) != 1 || entries[0].State != StateRolledBack {
		t.Fatalf("Recover = %#v, %v", entries, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "before-db" {
		t.Fatalf("restored action target = %q, %v", got, err)
	}
}

func TestNoopCreatesNoBackupOrJournal(t *testing.T) {
	e, opts := testEngine(t, nil)
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, `"same"`, 0o600)
	result, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(target, "same")}})
	if err != nil || !result.Noop {
		t.Fatalf("Apply = %#v, %v", result, err)
	}
	entries, err := os.ReadDir(opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("unexpected journal: %s", entry.Name())
		}
	}
	if _, err := os.Stat(opts.BackupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup root exists or unexpected err: %v", err)
	}
}

func TestNoopRechecksTargetAfterValidation(t *testing.T) {
	e, opts := testEngine(t, nil)
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, `"same"`, 0o600)
	c := change(target, "same")
	c.PostValidate = func(string) error {
		writeMode(t, target, `"external"`, 0o600)
		return nil
	}
	if _, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{c}}); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("Apply error = %v, want target changed", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != `"external"` {
		t.Fatalf("external target = %q, %v", got, err)
	}
	if _, err := os.Stat(opts.BackupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NO-OP recheck failure created backups: %v", err)
	}
}

func TestRunActionChecksBeforeImageImmediatelyBeforeExecute(t *testing.T) {
	target := filepath.Join(t.TempDir(), "database")
	writeMode(t, target, "before", 0o600)
	executed := false
	e, opts := testEngine(t, func(point FaultPoint) error {
		if point == FaultBeforeCommit {
			writeMode(t, target, "external", 0o600)
		}
		return nil
	})
	result, err := e.RunAction(context.Background(), ActionRequest{
		Client:  "ccswitch",
		Targets: []string{target},
		IsNoop:  func(context.Context) (bool, error) { return false, nil },
		Execute: func(context.Context, string) error {
			executed = true
			return nil
		},
	})
	if !errors.Is(err, ErrTargetChanged) || executed {
		t.Fatalf("RunAction = %#v, %v, executed=%v", result, err, executed)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != "external" {
		t.Fatalf("external target = %q, %v", got, readErr)
	}
	entries, recoverErr := e.Recover(context.Background())
	if recoverErr != nil || len(entries) != 1 || entries[0].State != StateRolledBack {
		t.Fatalf("Recover = %#v, %v", entries, recoverErr)
	}
	if entries[0].BackupDir == "" || !strings.HasPrefix(entries[0].BackupDir, opts.BackupRoot) {
		t.Fatalf("backup path = %q", entries[0].BackupDir)
	}
}

func TestRunActionMakesExistingAndMissingTargetsPrivateBeforeExecute(t *testing.T) {
	e, _ := testEngine(t, nil)
	root := t.TempDir()
	existing := filepath.Join(root, "database")
	missing := filepath.Join(root, "database-wal")
	writeMode(t, existing, "before", 0o644)
	_, err := e.RunAction(context.Background(), ActionRequest{
		Client:           "ccswitch",
		Targets:          []string{existing, missing},
		PrecreateMissing: []string{missing},
		IsNoop:           func(context.Context) (bool, error) { return false, nil },
		Execute: func(context.Context, string) error {
			for _, target := range []string{existing, missing} {
				private, err := privateFileSecurityValid(target)
				if err != nil {
					return err
				}
				if !private {
					return errors.New("target security during Execute is not private")
				}
			}
			return os.WriteFile(existing, []byte("after"), 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunActionDoesNotDeleteSidecarCreatedAfterCAS(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "database")
	sidecar := filepath.Join(root, "database-wal")
	writeMode(t, database, "before", 0o644)
	beforeSecurity := securitySnapshot(t, database)
	e, _ := testEngine(t, func(point FaultPoint) error {
		if point == FaultBeforePrivacy {
			writeMode(t, sidecar, "external", 0o600)
		}
		return nil
	})
	executed := false
	_, err := e.RunAction(context.Background(), ActionRequest{
		Client:           "ccswitch",
		Targets:          []string{database, sidecar},
		PrecreateMissing: []string{sidecar},
		IsNoop:           func(context.Context) (bool, error) { return false, nil },
		Execute: func(context.Context, string) error {
			executed = true
			return nil
		},
	})
	if !errors.Is(err, ErrTargetChanged) || executed {
		t.Fatalf("RunAction error = %v, executed=%v", err, executed)
	}
	content, readErr := os.ReadFile(sidecar)
	if readErr != nil || string(content) != "external" {
		t.Fatalf("external sidecar = %q, %v", content, readErr)
	}
	assertSecuritySnapshot(t, database, beforeSecurity)
}

func TestRunActionRechecksNoopAfterPrivacyInspection(t *testing.T) {
	e, opts := testEngine(t, nil)
	target := filepath.Join(t.TempDir(), "database")
	writeMode(t, target, "before", 0o600)
	checks := 0
	executed := false
	result, err := e.RunAction(context.Background(), ActionRequest{
		Client:  "ccswitch",
		Targets: []string{target},
		IsNoop: func(context.Context) (bool, error) {
			checks++
			return checks == 1, nil
		},
		Execute: func(context.Context, string) error {
			executed = true
			return os.WriteFile(target, []byte("after"), 0o600)
		},
	})
	if err != nil || !executed || result.Noop || checks != 2 {
		t.Fatalf("RunAction = %#v, %v, executed=%v, checks=%d", result, err, executed, checks)
	}
	if result.BackupDir == "" || !strings.HasPrefix(result.BackupDir, opts.BackupRoot) {
		t.Fatalf("backup path = %q", result.BackupDir)
	}
}

func TestRunActionRejectsSecretsFromJournalMetadata(t *testing.T) {
	const canary = "sk-txn-action-metadata-canary"
	key, err := secret.New("action", canary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	e := NewFileEngine(Options{
		StateDir:   stateDir,
		BackupRoot: filepath.Join(root, canary, "backups"),
		LockPath:   filepath.Join(root, "lock"),
	})
	executed := false
	_, err = e.RunAction(context.Background(), ActionRequest{
		Client:  "ccswitch",
		Targets: []string{filepath.Join(root, "database")},
		Secrets: []secret.Key{key},
		IsNoop:  func(context.Context) (bool, error) { return false, nil },
		Execute: func(context.Context, string) error {
			executed = true
			return nil
		},
	})
	if err == nil || executed || strings.Contains(err.Error(), canary) {
		t.Fatalf("RunAction metadata error = %v, executed=%v", err, executed)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata rejection created state: %v", err)
	}
}

func TestRequestValidationAndLockedPrecondition(t *testing.T) {
	e, _ := testEngine(t, nil)
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, `"before"`, 0o600)
	for _, req := range []Request{
		{Client: "../codex", Changes: []Change{change(target, "after")}},
		{Client: "codex", Changes: []Change{{Target: target, Content: []byte(`"after"`), Perm: 0o644, ParserKind: contract.ParserJSON}}},
		{Client: "codex", Changes: []Change{change(target, "after"), change(target, "after")}},
	} {
		if _, err := e.Apply(context.Background(), req); err == nil {
			t.Fatalf("invalid request accepted: %#v", req)
		}
	}
	called := false
	_, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{{
		Target: target, Content: []byte(`"after"`), Perm: 0o600, ParserKind: contract.ParserJSON,
		Precondition: func() error { called = true; return errors.New("changed under lock") },
	}}})
	if err == nil || !called {
		t.Fatalf("Precondition result = %v, called=%v", err, called)
	}
}

func TestRollbackRemovesTransactionCreatedStageParent(t *testing.T) {
	e, _ := testEngine(t, func(p FaultPoint) error {
		if p == FaultBeforeCommit {
			return errors.New("stop before commit")
		}
		return nil
	})
	parent := filepath.Join(t.TempDir(), "new-parent")
	target := filepath.Join(parent, "config")
	_, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(target, "after")}})
	if err == nil {
		t.Fatal("Apply unexpectedly succeeded")
	}
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction-created parent remains: %v", err)
	}
}

func TestJournalNeverContainsCanary(t *testing.T) {
	e, opts := testEngine(t, func(p FaultPoint) error {
		if p == FaultBeforeCommit {
			return ErrCrashInjected
		}
		return nil
	})
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, "old-secret-canary", 0o600)
	_, err := e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(target, "new-secret-canary")}})
	if !errors.Is(err, ErrCrashInjected) {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(opts.StateDir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("journals = %v, %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil || strings.Contains(string(b), "canary") {
		t.Fatalf("journal leaks content: %q, %v", b, err)
	}
}

func TestApplyDoesNotLeakCanaryToOutputOrJournal(t *testing.T) {
	const canary = "sk-txn-output-journal-canary"
	key, err := secret.New("test", canary)
	if err != nil {
		t.Fatal(err)
	}
	e, opts := testEngine(t, nil)
	target := filepath.Join(t.TempDir(), "config.json")
	var applyErr error
	output := captureProcessOutput(t, func() {
		_, applyErr = e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{{
			Target:             target,
			Content:            []byte(`{"token":"` + canary + `"}`),
			Perm:               0o600,
			ParserKind:         contract.ParserJSON,
			Key:                key,
			AllowedSecretPaths: [][]string{{"token"}},
		}}})
	})
	if applyErr != nil {
		t.Fatal(applyErr)
	}
	if strings.Contains(output, canary) {
		t.Fatal("Apply leaked canary to stdout or stderr")
	}
	journals, err := filepath.Glob(filepath.Join(opts.StateDir, "*.json"))
	if err != nil || len(journals) != 1 {
		t.Fatalf("journals = %v, %v", journals, err)
	}
	journal, err := os.ReadFile(journals[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journal), canary) {
		t.Fatal("journal leaked canary")
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".relay-install-stage-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("secret-bearing stages remain: %v, %v", stages, err)
	}
}

func captureProcessOutput(t *testing.T, run func()) string {
	t.Helper()
	stdout, stderr := os.Stdout, os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = writer, writer
	defer func() {
		os.Stdout, os.Stderr = stdout, stderr
		_ = reader.Close()
		_ = writer.Close()
	}()
	run()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = stdout, stderr
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestSecretMetadataAndValidatorErrorsAreRejectedWithoutLeak(t *testing.T) {
	key, err := secret.New("test", "sk-txn-metadata-canary")
	if err != nil {
		t.Fatal(err)
	}
	e, opts := testEngine(t, nil)
	content := []byte(`{"token":"sk-txn-metadata-canary"}`)
	_, err = e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{{
		Target:             filepath.Join(t.TempDir(), "sk-txn-metadata-canary.json"),
		Content:            content,
		Perm:               0o600,
		ParserKind:         contract.ParserJSON,
		Key:                key,
		AllowedSecretPaths: [][]string{{"token"}},
	}}})
	if err == nil || strings.Contains(err.Error(), "sk-txn-metadata-canary") {
		t.Fatalf("metadata validation error = %v", err)
	}

	target := filepath.Join(t.TempDir(), "config.json")
	_, err = e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{{
		Target:             target,
		Content:            content,
		Perm:               0o600,
		ParserKind:         contract.ParserJSON,
		Key:                key,
		AllowedSecretPaths: [][]string{{"token"}},
		PostValidate: func(string) error {
			return errors.New("validator echoed sk-txn-metadata-canary")
		},
	}}})
	if err == nil || strings.Contains(err.Error(), "sk-txn-metadata-canary") {
		t.Fatalf("post-validator error = %v", err)
	}
	journals, globErr := filepath.Glob(filepath.Join(opts.StateDir, "*.json"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	for _, journal := range journals {
		data, readErr := os.ReadFile(journal)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(data), "sk-txn-metadata-canary") {
			t.Fatalf("journal leaked canary: %s", journal)
		}
	}
}

func TestEverySelectedKeyIsRejectedFromAllJournalMetadata(t *testing.T) {
	const (
		firstCanary  = "sk-txn-cross-target-canary"
		secondCanary = "sk-txn-second-canary"
	)
	firstKey, err := secret.New("first", firstCanary)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := secret.New("second", secondCanary)
	if err != nil {
		t.Fatal(err)
	}
	e, opts := testEngine(t, nil)
	root := t.TempDir()
	req := Request{Client: "codex", Changes: []Change{
		{
			Target: filepath.Join(root, "first.json"), Content: []byte(`{"token":"` + firstCanary + `"}`),
			Perm: 0o600, ParserKind: contract.ParserJSON, Key: firstKey, AllowedSecretPaths: [][]string{{"token"}},
		},
		{
			Target: filepath.Join(root, firstCanary+".json"), Content: []byte(`{"token":"` + secondCanary + `"}`),
			Perm: 0o600, ParserKind: contract.ParserJSON, Key: secondKey, AllowedSecretPaths: [][]string{{"token"}},
		},
	}}
	if _, err := e.Apply(context.Background(), req); err == nil || strings.Contains(err.Error(), firstCanary) {
		t.Fatalf("cross-target metadata error = %v", err)
	}
	if _, err := os.Stat(opts.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata rejection created state: %v", err)
	}

	e = NewFileEngine(Options{
		StateDir:   filepath.Join(root, "state"),
		BackupRoot: filepath.Join(root, firstCanary, "backups"),
		LockPath:   filepath.Join(root, "lock"),
	})
	if _, err := e.Apply(context.Background(), Request{Client: "codex", Changes: req.Changes[:1]}); err == nil || strings.Contains(err.Error(), firstCanary) {
		t.Fatalf("backup-root metadata error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("option metadata rejection created state: %v", err)
	}
}

func TestEverySelectedKeyIsRejectedFromOtherChangeContent(t *testing.T) {
	const (
		firstCanary  = "sk-txn-cross-content-canary"
		secondCanary = "sk-txn-content-second-canary"
	)
	firstKey, err := secret.New("first", firstCanary)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := secret.New("second", secondCanary)
	if err != nil {
		t.Fatal(err)
	}
	e, opts := testEngine(t, nil)
	root := t.TempDir()
	req := Request{Client: "codex", Changes: []Change{
		{
			Target: filepath.Join(root, "first.json"), Content: []byte(`{"token":"` + firstCanary + `"}`),
			Perm: 0o600, ParserKind: contract.ParserJSON, Key: firstKey, AllowedSecretPaths: [][]string{{"token"}},
		},
		{
			Target: filepath.Join(root, "second.json"), Content: []byte(`{"token":"` + secondCanary + `","other":"` + firstCanary + `"}`),
			Perm: 0o600, ParserKind: contract.ParserJSON, Key: secondKey, AllowedSecretPaths: [][]string{{"token"}},
		},
	}}
	if _, err := e.Apply(context.Background(), req); err == nil || strings.Contains(err.Error(), firstCanary) {
		t.Fatalf("cross-content error = %v", err)
	}
	if _, err := os.Stat(opts.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("content rejection created state: %v", err)
	}
}

func TestBlacklistedContentIsRejectedBeforeCommit(t *testing.T) {
	key, err := secret.New("test", "sk-txn-blacklist-canary")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := testEngine(t, nil)
	target := filepath.Join(t.TempDir(), "config.json")
	_, err = e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{{
		Target:             target,
		Content:            []byte(`{"token":"sk-txn-blacklist-canary","network_access":true}`),
		Perm:               0o600,
		ParserKind:         contract.ParserJSON,
		Blacklist:          []string{"network_access"},
		Key:                key,
		AllowedSecretPaths: [][]string{{"token"}},
	}}})
	if err == nil {
		t.Fatal("blacklisted content was accepted")
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("blacklisted content reached target: %v", statErr)
	}
}

func TestLockAndRollbackFailureRequiresManualRecovery(t *testing.T) {
	e, opts := testEngine(t, nil)
	lock, err := acquireLock(opts.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(filepath.Join(t.TempDir(), "x"), "x")}})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("locked Apply error = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	var renamed bool
	e.options.FaultInjector = func(p FaultPoint) error {
		if p == FaultAfterRename && !renamed {
			renamed = true
			return errors.New("hard failure")
		}
		if p == FaultBeforeRestore {
			return errors.New("restore failure")
		}
		return nil
	}
	target := filepath.Join(t.TempDir(), "config")
	writeMode(t, target, "before", 0o600)
	_, err = e.Apply(context.Background(), Request{Client: "codex", Changes: []Change{change(target, "after")}})
	if !errors.Is(err, ErrManualRecoveryRequired) {
		t.Fatalf("Apply error = %v", err)
	}
	if !strings.Contains(err.Error(), opts.BackupRoot) {
		t.Fatalf("manual recovery error lacks backup path: %v", err)
	}
	entries, recoverErr := e.Recover(context.Background())
	if !errors.Is(recoverErr, ErrManualRecoveryRequired) || len(entries) != 0 {
		t.Fatalf("Recover = %#v, %v", entries, recoverErr)
	}
}
