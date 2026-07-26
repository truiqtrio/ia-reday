package adapters

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"relay-install/internal/contract"
	"relay-install/internal/secret"
	"relay-install/internal/txn"
)

func adapterTestEngine(t *testing.T) (*txn.FileEngine, txn.Options) {
	t.Helper()
	root := t.TempDir()
	opts := txn.Options{
		StateDir:   filepath.Join(root, "state"),
		BackupRoot: filepath.Join(root, "backups"),
		LockPath:   filepath.Join(root, "txn.lock"),
	}
	return txn.NewFileEngine(opts), opts
}

type stagedStrictRunner struct {
	t     *testing.T
	paths []string
}

func (r *stagedStrictRunner) StrictConfig(_ context.Context, binary, path string) error {
	r.t.Helper()
	if binary != "/test/codex" {
		r.t.Errorf("strict binary = %q", binary)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := ValidateCodexConfig(content); err != nil {
		return err
	}
	if err := privatePathError(path); err != nil {
		return fmt.Errorf("strict input security: %w", err)
	}
	r.paths = append(r.paths, path)
	return nil
}

func TestCodexApplyUsesTxnStrictValidationAndSecondApplyIsNoop(t *testing.T) {
	engine, opts := adapterTestEngine(t)
	home := t.TempDir()
	runner := &stagedStrictRunner{t: t}
	a := codexAdapter{
		path:               "/test/codex",
		codexHome:          home,
		strictConfigRunner: runner,
		engine:             engine,
	}
	change, err := a.PlanCodexConfig(testCodexConfig(t, "alpha", CodexProfileGuarded))
	if err != nil {
		t.Fatal(err)
	}
	set := ChangeSet{Client: contract.ClientCodex, Changes: []Change{change}}
	first, err := a.Apply(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	if first.Noop {
		t.Fatal("first Codex Apply unexpectedly reported NO-OP")
	}
	if len(runner.paths) != 1 || !strings.HasPrefix(filepath.Base(runner.paths[0]), ".relay-install-stage-") {
		t.Fatalf("strict runner paths = %v, want staged path", runner.paths)
	}
	assertPrivateTarget(t, change.Point.PathHint)
	beforeHash := fileSHA256(t, change.Point.PathHint)
	backupCount := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCodex))
	if backupCount != 1 {
		t.Fatalf("first Apply backup count = %d, want 1", backupCount)
	}

	second, err := a.Apply(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Noop {
		t.Fatalf("second Codex Apply result = %#v, want Noop", second)
	}
	if got := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCodex)); got != backupCount {
		t.Fatalf("NO-OP created backups: before=%d after=%d", backupCount, got)
	}
	if afterHash := fileSHA256(t, change.Point.PathHint); afterHash != beforeHash {
		t.Fatalf("NO-OP changed target hash: before=%x after=%x", beforeHash, afterHash)
	}
}

func TestCodexApplyRechecksManagedMarkerUnderTxnLock(t *testing.T) {
	engine, opts := adapterTestEngine(t)
	home := t.TempDir()
	a := codexAdapter{codexHome: home, engine: engine}
	change, err := a.PlanCodexConfig(testCodexConfig(t, "alpha", CodexProfileGuarded))
	if err != nil {
		t.Fatal(err)
	}
	foreign := []byte("model = \"foreign\"\n")
	if err := os.WriteFile(change.Point.PathHint, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = a.Apply(context.Background(), ChangeSet{Client: contract.ClientCodex, Changes: []Change{change}})
	if !errors.Is(err, ErrCodexConfigConflict) {
		t.Fatalf("Apply error = %v, want conflict", err)
	}
	got, readErr := os.ReadFile(change.Point.PathHint)
	if readErr != nil || string(got) != string(foreign) {
		t.Fatalf("foreign target changed: %q, %v", got, readErr)
	}
	if got := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCodex)); got != 0 {
		t.Fatalf("conflict created %d backup transactions", got)
	}
}

func TestClaudeCodeApplyUsesTxnAndSecondApplyIsNoop(t *testing.T) {
	engine, opts := adapterTestEngine(t)
	target := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := []byte(`{"permissions":{"allow":["Read"]},"env":{"KEEP":"yes"}}`)
	if err := os.WriteFile(target, existing, 0o644); err != nil {
		t.Fatal(err)
	}
	change, err := GenerateClaudeCodeChange(target, existing, "https://relay.example.test", testClaudeCodeKey(t), ClaudeCodeModels{})
	if err != nil {
		t.Fatal(err)
	}
	a := claudeCodeAdapter{target: target, engine: engine}
	set := ChangeSet{Client: contract.ClientClaudeCode, Changes: []Change{change}}
	first, err := a.Apply(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	if first.Noop {
		t.Fatal("first Claude Code Apply unexpectedly reported NO-OP")
	}
	assertPrivateTarget(t, target)
	beforeHash := fileSHA256(t, target)
	backupCount := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientClaudeCode))
	if backupCount != 1 {
		t.Fatalf("first Apply backup count = %d, want 1", backupCount)
	}
	second, err := a.Apply(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Noop {
		t.Fatalf("second Claude Code Apply result = %#v, want Noop", second)
	}
	if got := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientClaudeCode)); got != backupCount {
		t.Fatalf("NO-OP created backups: before=%d after=%d", backupCount, got)
	}
	if afterHash := fileSHA256(t, target); afterHash != beforeHash {
		t.Fatalf("NO-OP changed target hash: before=%x after=%x", beforeHash, afterHash)
	}
}

func TestClaudeCodeApplyRejectsStaleMergeWithoutLosingUnknownKeys(t *testing.T) {
	engine, _ := adapterTestEngine(t)
	target := filepath.Join(t.TempDir(), "settings.json")
	existing := []byte(`{"env":{"KEEP":"yes"}}`)
	if err := os.WriteFile(target, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	change, err := GenerateClaudeCodeChange(target, existing, "https://relay.example.test", testClaudeCodeKey(t), ClaudeCodeModels{})
	if err != nil {
		t.Fatal(err)
	}
	updated := []byte(`{"env":{"KEEP":"yes"},"mcpServers":{"new":{"command":"node"}}}`)
	if err := os.WriteFile(target, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	a := claudeCodeAdapter{target: target, engine: engine}
	_, err = a.Apply(context.Background(), ChangeSet{Client: contract.ClientClaudeCode, Changes: []Change{change}})
	if !errors.Is(err, ErrAdapterStale) {
		t.Fatalf("Apply error = %v, want stale", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != string(updated) {
		t.Fatalf("stale merge overwrote unknown keys: %q, %v", got, readErr)
	}
}

func TestClaudeCodeApplyRejectsCanaryOutsidePermittedFieldWithoutLeak(t *testing.T) {
	engine, opts := adapterTestEngine(t)
	target := filepath.Join(t.TempDir(), "settings.json")
	change, err := GenerateClaudeCodeChange(target, nil, "https://relay.example.test", testClaudeCodeKey(t), ClaudeCodeModels{})
	if err != nil {
		t.Fatal(err)
	}
	change.Content = []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://relay.example.test","ANTHROPIC_AUTH_TOKEN":"` + claudeCodeCanary + `","ANTHROPIC_DEFAULT_OPUS_MODEL":"x","ANTHROPIC_DEFAULT_SONNET_MODEL":"y","ANTHROPIC_DEFAULT_HAIKU_MODEL":"z","OTHER":"` + claudeCodeCanary + `"}}`)
	a := claudeCodeAdapter{target: target, engine: engine}
	_, err = a.Apply(context.Background(), ChangeSet{Client: contract.ClientClaudeCode, Changes: []Change{change}})
	if err == nil {
		t.Fatal("Apply accepted key outside permitted field")
	}
	if strings.Contains(err.Error(), claudeCodeCanary) {
		t.Fatalf("error leaked canary: %v", err)
	}
	assertTreeDoesNotContain(t, filepath.Dir(opts.StateDir), claudeCodeCanary)
}

func TestClaudeCodeApplyRejectsAnotherSelectedKeyBeforeParsing(t *testing.T) {
	const (
		firstCanary  = "sk-adapter-first-cross-key-canary"
		secondCanary = "sk-adapter-second-cross-key-canary"
	)
	firstKey, err := secret.New("first", firstCanary)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := secret.New("second", secondCanary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	first, err := GenerateClaudeCodeChange(filepath.Join(root, "first.json"), nil, "https://relay.example.test", firstKey, ClaudeCodeModels{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateClaudeCodeChange(filepath.Join(root, "second.json"), nil, "https://relay.example.test", secondKey, ClaudeCodeModels{})
	if err != nil {
		t.Fatal(err)
	}
	first.Content = []byte(`{"` + secondCanary + `":1,"` + secondCanary + `":2}`)
	engine, opts := adapterTestEngine(t)
	a := claudeCodeAdapter{engine: engine}
	_, err = a.Apply(context.Background(), ChangeSet{Client: contract.ClientClaudeCode, Changes: []Change{first, second}})
	if err == nil || strings.Contains(err.Error(), firstCanary) || strings.Contains(err.Error(), secondCanary) {
		t.Fatalf("Apply cross-key error = %v", err)
	}
	if _, err := os.Stat(opts.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("adapter validation created txn state: %v", err)
	}
}

func TestCCSwitchApplyUsesTxnAndSecondApplyIsNoop(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	engine, opts := adapterTestEngine(t)
	a := ccswitchAdapter{
		dbPath:         db,
		sqlite3:        bin,
		engine:         engine,
		applyPreflight: func(context.Context) error { return nil },
	}
	set := ChangeSet{
		Client:   contract.ClientCCSwitch,
		CCSwitch: &CCSwitchChange{Providers: testCCSwitchProviders()},
	}
	first, err := a.Apply(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	if first.Noop {
		t.Fatal("first Apply unexpectedly reported NO-OP")
	}
	beforeHash := fileSHA256(t, db)
	backupCount := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCCSwitch))
	if backupCount != 1 {
		t.Fatalf("first Apply backup count = %d, want 1", backupCount)
	}
	assertTreeDoesNotContain(t, opts.StateDir, ccswitchCanary)
	assertTreeDoesNotContain(t, opts.BackupRoot, ccswitchCanary)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(db + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected SQLite sidecar %s after commit: %v", suffix, err)
		}
	}

	second, err := a.Apply(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Noop {
		t.Fatalf("second Apply result = %#v, want NO-OP", second)
	}
	if got := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCCSwitch)); got != backupCount {
		t.Fatalf("NO-OP created backups: before=%d after=%d", backupCount, got)
	}
	if afterHash := fileSHA256(t, db); afterHash != beforeHash {
		t.Fatalf("NO-OP changed database hash: before=%x after=%x", beforeHash, afterHash)
	}
}

func TestCCSwitchApplyRepairsManagedFieldDrift(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	engine, opts := adapterTestEngine(t)
	a := ccswitchAdapter{
		dbPath:         db,
		sqlite3:        bin,
		engine:         engine,
		applyPreflight: func(context.Context) error { return nil },
	}
	set := ChangeSet{
		Client:   contract.ClientCCSwitch,
		CCSwitch: &CCSwitchChange{Providers: testCCSwitchProviders()},
	}
	if _, err := a.Apply(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, db, `UPDATE providers SET sort_index=999 WHERE id='relay-intelalloc-claude';`)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	before := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCCSwitch))
	if _, err := a.Apply(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	if after := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCCSwitch)); after != before+1 {
		t.Fatalf("drift repair backups = %d, want %d", after, before+1)
	}
	out, err := exec.Command(bin, "-noheader", db, `SELECT sort_index FROM providers WHERE id='relay-intelalloc-claude';`).Output()
	if err != nil || strings.TrimSpace(string(out)) != "7" {
		t.Fatalf("sort_index was not repaired: %q, %v", out, err)
	}
}

func TestCCSwitchApplyPreflightStopsBeforeTxn(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	engine, opts := adapterTestEngine(t)
	a := ccswitchAdapter{
		dbPath:  db,
		sqlite3: bin,
		engine:  engine,
		applyPreflight: func(context.Context) error {
			return ErrCCSwitchTakeover
		},
	}
	beforeHash := fileSHA256(t, db)
	_, err := a.Apply(context.Background(), ChangeSet{
		Client:   contract.ClientCCSwitch,
		CCSwitch: &CCSwitchChange{Providers: testCCSwitchProviders()},
	})
	if !errors.Is(err, ErrCCSwitchTakeover) {
		t.Fatalf("Apply error = %v, want preflight error", err)
	}
	if afterHash := fileSHA256(t, db); afterHash != beforeHash {
		t.Fatal("preflight failure changed database")
	}
	if got := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCCSwitch)); got != 0 {
		t.Fatalf("preflight failure created %d backups", got)
	}
}

func TestCCSwitchApplyRejectsUnknownLiveColumnBeforeTxn(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	if err := exec.Command(bin, db, `ALTER TABLE providers ADD COLUMN "`+ccswitchCanary+`" TEXT DEFAULT 'preserve';`).Run(); err != nil {
		t.Fatal(err)
	}
	engine, opts := adapterTestEngine(t)
	a := ccswitchAdapter{
		dbPath:         db,
		sqlite3:        bin,
		engine:         engine,
		applyPreflight: func(context.Context) error { return nil },
	}
	_, err := a.Apply(context.Background(), ChangeSet{
		Client:   contract.ClientCCSwitch,
		CCSwitch: &CCSwitchChange{Providers: testCCSwitchProviders()},
	})
	if err == nil {
		t.Fatal("Apply accepted an unverified live column")
	}
	if strings.Contains(err.Error(), ccswitchCanary) {
		t.Fatalf("schema rejection leaked selected key: %v", err)
	}
	if got := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCCSwitch)); got != 0 {
		t.Fatalf("schema rejection created %d backups", got)
	}
}

func TestCCSwitchSafetyChecksSupportImplementedPlatforms(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if !ccSwitchSafetyChecksSupported(goos) {
			t.Fatalf("safety checks unexpectedly disabled for %s", goos)
		}
	}
	if ccSwitchSafetyChecksSupported("freebsd") {
		t.Fatal("safety checks unexpectedly enabled for freebsd")
	}
}

func TestCCSwitchApplyPassesSecretsToTxnMetadataGuard(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	engine := txn.NewFileEngine(txn.Options{
		StateDir:   stateDir,
		BackupRoot: filepath.Join(root, ccswitchCanary, "backups"),
		LockPath:   filepath.Join(root, "lock"),
	})
	a := ccswitchAdapter{
		dbPath: db, sqlite3: bin, engine: engine,
		applyPreflight: func(context.Context) error { return nil },
	}
	before := fileSHA256(t, db)
	_, err := a.Apply(context.Background(), ChangeSet{
		Client:   contract.ClientCCSwitch,
		CCSwitch: &CCSwitchChange{Providers: testCCSwitchProviders()},
	})
	if err == nil || strings.Contains(err.Error(), ccswitchCanary) {
		t.Fatalf("Apply metadata error = %v", err)
	}
	if fileSHA256(t, db) != before {
		t.Fatal("metadata rejection changed database")
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata rejection created txn state: %v", err)
	}
}

func assertPrivateTarget(t *testing.T, path string) {
	t.Helper()
	if err := privatePathError(path); err != nil {
		t.Fatalf("private security(%s): %v", path, err)
	}
}

func backupTransactionCount(t *testing.T, root, client string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, client))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}

func fileSHA256(t *testing.T, path string) [32]byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(content)
}

func assertTreeDoesNotContain(t *testing.T, root, canary string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), canary) {
			return fmt.Errorf("canary found in %s", path)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
