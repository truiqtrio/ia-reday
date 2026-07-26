// Package txn is the only file-writing path for relay-install changes.
package txn

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"relay-install/internal/contract"
	"relay-install/internal/secret"
)

var (
	ErrLocked                 = errors.New("txn: another transaction is active")
	ErrRecoveryRequired       = errors.New("txn: unfinished transaction requires recover")
	ErrTargetChanged          = errors.New("txn: target changed after before-image")
	ErrManualRecoveryRequired = errors.New("txn: manual recovery required")
	ErrCrashInjected          = errors.New("txn: crash injected")
)

type State string

const (
	StatePending                State = "PENDING"
	StateBackedUp               State = "BACKED_UP"
	StateStaged                 State = "STAGED"
	StateValidated              State = "VALIDATED"
	StateCommitted              State = "COMMITTED"
	StateRollingBack            State = "ROLLING_BACK"
	StateRolledBack             State = "ROLLED_BACK"
	StateManualRecoveryRequired State = "MANUAL_RECOVERY_REQUIRED"
)

type FaultPoint string

const (
	FaultBeforeCommit      FaultPoint = "before_commit"
	FaultBeforePrivacy     FaultPoint = "before_action_privacy"
	FaultAfterRename       FaultPoint = "after_rename"
	FaultAfterCommitted    FaultPoint = "after_committed"
	FaultBeforeRestore     FaultPoint = "before_restore"
	FaultAfterRestoreStage FaultPoint = "after_restore_stage"
)

// FaultInjector is test-only plumbing. Returning ErrCrashInjected models a
// process death: Apply returns immediately and leaves recovery to a new engine.
type FaultInjector func(FaultPoint) error

type Options struct {
	// Defaults place no-secret journals in the user config directory under
	// relay-install/state. Backups live beside it under backups; they can copy
	// pre-existing plaintext credentials and are therefore retained under 0700.
	StateDir      string
	BackupRoot    string
	LockPath      string
	FaultInjector FaultInjector
}

type Change struct {
	Target             string
	Content            []byte
	Perm               os.FileMode
	ParserKind         contract.ParserKind
	Blacklist          []string
	Key                secret.Key
	AllowedSecretPaths [][]string
	// Precondition rechecks mutable target assumptions under the transaction lock.
	Precondition func() error
	PostValidate func(stagedPath string) error
}

type Request struct {
	Client  string
	Changes []Change
}

// ActionRequest wraps a non-file-rename mutation (for example a SQLite import)
// in the same backup, journal, and compensating-rollback protocol.
type ActionRequest struct {
	Client           string
	Targets          []string
	Secrets          []secret.Key
	PrecreateMissing []string
	IsNoop           func(context.Context) (bool, error)
	Execute          func(context.Context, string) error
}

type Result struct {
	TxnID     string
	BackupDir string
	State     State
	Applied   int
	Noop      bool
}

// JournalChange deliberately has no content or key fields. Backup files can
// contain secrets, so their 0700/0600 (current-user DACL on Windows) retention
// surface must be disclosed by UI.
type JournalChange struct {
	Target             string      `json:"target"`
	Backup             string      `json:"backup,omitempty"`
	Stage              string      `json:"stage,omitempty"`
	RestoreStage       string      `json:"restore_stage,omitempty"`
	StageParentCreated bool        `json:"stage_parent_created,omitempty"`
	Hash               string      `json:"hash"`
	Perm               os.FileMode `json:"perm"`
	Security           string      `json:"security,omitempty"`
	Existed            bool        `json:"existed"`
	Applied            bool        `json:"applied"`
	Restoring          bool        `json:"restoring,omitempty"`
	AfterKnown         bool        `json:"after_known,omitempty"`
	AfterExisted       bool        `json:"after_existed,omitempty"`
	AfterHash          string      `json:"after_hash,omitempty"`
	AfterPerm          os.FileMode `json:"after_perm,omitempty"`
	AfterSecurity      string      `json:"after_security,omitempty"`
}

type JournalEntry struct {
	TxnID        string          `json:"txn_id"`
	Client       string          `json:"client"`
	State        State           `json:"state"`
	BackupDir    string          `json:"backup_dir"`
	Targets      []string        `json:"targets"`
	Changes      []JournalChange `json:"changes"`
	AppliedCount int             `json:"applied_count"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type Engine interface {
	Apply(context.Context, Request) (Result, error)
	RunAction(context.Context, ActionRequest) (Result, error)
	Recover(context.Context) ([]JournalEntry, error)
}

type FileEngine struct{ options Options }

func NewFileEngine(options Options) *FileEngine { return &FileEngine{options: options} }

func (e *FileEngine) Apply(ctx context.Context, req Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := validateRequestSecrets(req); err != nil {
		return Result{}, err
	}
	if err := validateCrossChangeSecrets(req); err != nil {
		return Result{}, err
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	if err := validateRequestSecrets(req, e.options.StateDir, e.options.BackupRoot, e.options.LockPath); err != nil {
		return Result{}, err
	}
	if len(req.Changes) == 0 {
		return Result{}, errors.New("txn: client and changes are required")
	}
	opts, err := e.resolvedOptions()
	if err != nil {
		return Result{}, err
	}
	if err := validateRequestSecrets(req, opts.StateDir, opts.BackupRoot, opts.LockPath); err != nil {
		return Result{}, err
	}
	lock, err := acquireLock(opts.LockPath)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	if err := ensureNoIncompleteJournals(opts.StateDir); err != nil {
		return Result{}, err
	}
	if err := validateRequestSecrets(req, opts.StateDir, opts.BackupRoot, opts.LockPath); err != nil {
		return Result{}, err
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	if err := validateCrossChangeSecrets(req); err != nil {
		return Result{}, err
	}

	requestSecrets := requestKeys(req)
	for _, c := range req.Changes {
		if err := validateChange(c); err != nil {
			return Result{}, err
		}
		if c.Precondition != nil {
			if err := c.Precondition(); err != nil {
				return Result{}, sanitizeSecretsError(requestSecrets, err)
			}
		}
	}
	if same, err := allSame(req.Changes); err != nil {
		return Result{}, err
	} else if same {
		for _, c := range req.Changes {
			if c.PostValidate != nil {
				if err := c.PostValidate(c.Target); err != nil {
					return Result{}, sanitizeSecretsError(requestSecrets, err)
				}
			}
		}
		stillSame, err := allSame(req.Changes)
		if err != nil {
			return Result{}, err
		}
		if !stillSame {
			return Result{}, ErrTargetChanged
		}
		return Result{State: StateCommitted, Noop: true}, nil
	}

	id, err := transactionID()
	if err != nil {
		return Result{}, err
	}
	beforeHash, err := beforeImageAggregate(req.Targets())
	if err != nil {
		return Result{}, err
	}
	entry := JournalEntry{TxnID: id, Client: req.Client, State: StatePending, UpdatedAt: time.Now().UTC()}
	entry.BackupDir = filepath.Join(opts.BackupRoot, req.Client, fmt.Sprintf("%s-%s-%s", time.Now().UTC().Format("20060102T150405Z"), id, beforeHash[:8]))
	for _, c := range req.Changes {
		entry.Targets = append(entry.Targets, c.Target)
	}
	journalPath := filepath.Join(opts.StateDir, id+".json")
	if err := writeJournal(journalPath, &entry); err != nil {
		return Result{}, err
	}

	if err := prepareBackupDir(opts.BackupRoot, req.Client, entry.BackupDir); err != nil {
		return e.abort(journalPath, &entry, opts, err)
	}
	for i, c := range req.Changes {
		jc, err := makeBeforeImage(entry.BackupDir, i, c.Target)
		if err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		jc.AfterKnown = true
		jc.RestoreStage = restoreStagePath(c.Target, id, i)
		jc.AfterExisted = true
		jc.AfterHash = hash(c.Content)
		jc.AfterPerm = desiredPerm(c)
		entry.Changes = append(entry.Changes, jc)
	}
	entry.State = StateBackedUp
	if err := writeJournal(journalPath, &entry); err != nil {
		return e.abort(journalPath, &entry, opts, err)
	}
	for i, c := range req.Changes {
		stage := stagePath(c.Target, id, i)
		entry.Changes[i].Stage = stage
		missing, err := stageParentMissing(filepath.Dir(c.Target))
		if err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		entry.Changes[i].StageParentCreated = missing
		// Persist the deterministic reference before creating a secret-bearing file.
		if err := writeJournal(journalPath, &entry); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		if err := ensureStageParent(filepath.Dir(c.Target)); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		if err := stageFile(stage, c.Content); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
	}
	entry.State = StateStaged
	if err := writeJournal(journalPath, &entry); err != nil {
		return e.abort(journalPath, &entry, opts, err)
	}
	for i, c := range req.Changes {
		if err := validateStagedFile(entry.Changes[i].Stage, c); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		// Record the after-image after the same security operation used on the
		// renamed target. Windows can canonicalize a DACL during that operation.
		if err := applyPrivateFileSecurity(entry.Changes[i].Stage); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		security, err := captureFileSecurity(entry.Changes[i].Stage)
		if err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		entry.Changes[i].AfterSecurity = security
		if c.PostValidate != nil {
			if err := c.PostValidate(entry.Changes[i].Stage); err != nil {
				return e.abort(journalPath, &entry, opts, sanitizeSecretsError(requestSecrets, err))
			}
		}
		if err := validateStagedFile(entry.Changes[i].Stage, c); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
	}
	entry.State = StateValidated
	if err := writeJournal(journalPath, &entry); err != nil {
		return e.abort(journalPath, &entry, opts, err)
	}
	if err := inject(opts, FaultBeforeCommit); err != nil {
		return e.faultResult(journalPath, &entry, opts, err)
	}

	for i, c := range req.Changes {
		stillBefore, err := matchesSnapshot(c.Target, entry.Changes[i].Existed, entry.Changes[i].Hash, entry.Changes[i].Perm, entry.Changes[i].Security)
		if err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		if !stillBefore {
			return e.abort(journalPath, &entry, opts, fmt.Errorf("%w: %s", ErrTargetChanged, c.Target))
		}
		// Persist the restore intent before rename so either crash side recovers.
		entry.Changes[i].Applied = true
		entry.AppliedCount++
		if err := writeJournal(journalPath, &entry); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		if err := os.Rename(entry.Changes[i].Stage, c.Target); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		if err := applyPrivateFileSecurity(c.Target); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		if err := syncDir(filepath.Dir(c.Target)); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		entry.Changes[i].Stage = ""
		if err := writeJournal(journalPath, &entry); err != nil {
			return e.abort(journalPath, &entry, opts, err)
		}
		if err := inject(opts, FaultAfterRename); err != nil {
			return e.faultResult(journalPath, &entry, opts, err)
		}
	}
	entry.State = StateCommitted
	if err := writeJournal(journalPath, &entry); err != nil {
		return e.abort(journalPath, &entry, opts, err)
	}
	if err := inject(opts, FaultAfterCommitted); err != nil {
		return e.faultResult(journalPath, &entry, opts, err)
	}
	return resultFor(&entry), nil
}

// RunAction uses generic filesystem before-images to compensate an action that
// mutates its targets outside the rename path (such as sqlite3 import).
func (e *FileEngine) RunAction(ctx context.Context, req ActionRequest) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := validateActionRequest(req); err != nil {
		return Result{}, err
	}
	if err := validateActionRequestSecrets(req, e.options.StateDir, e.options.BackupRoot, e.options.LockPath); err != nil {
		return Result{}, err
	}
	opts, err := e.resolvedOptions()
	if err != nil {
		return Result{}, err
	}
	if err := validateActionRequestSecrets(req, opts.StateDir, opts.BackupRoot, opts.LockPath); err != nil {
		return Result{}, err
	}
	lock, err := acquireLock(opts.LockPath)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	if err := ensureNoIncompleteJournals(opts.StateDir); err != nil {
		return Result{}, err
	}
	if err := validateActionRequest(req); err != nil {
		return Result{}, err
	}
	if err := validateActionRequestSecrets(req, opts.StateDir, opts.BackupRoot, opts.LockPath); err != nil {
		return Result{}, err
	}
	noop, err := req.IsNoop(ctx)
	if err != nil {
		return Result{}, sanitizeSecretsError(req.Secrets, err)
	}
	if noop {
		private, err := actionTargetsPrivate(req.Targets)
		if err != nil {
			return Result{}, err
		}
		if private {
			stillNoop, err := req.IsNoop(ctx)
			if err != nil {
				return Result{}, sanitizeSecretsError(req.Secrets, err)
			}
			if stillNoop {
				return Result{State: StateCommitted, Noop: true}, nil
			}
		}
	}
	id, err := transactionID()
	if err != nil {
		return Result{}, err
	}
	beforeHash, err := beforeImageAggregate(req.Targets)
	if err != nil {
		return Result{}, err
	}
	entry := JournalEntry{TxnID: id, Client: req.Client, State: StatePending}
	entry.BackupDir = filepath.Join(opts.BackupRoot, req.Client, fmt.Sprintf("%s-%s-%s", time.Now().UTC().Format("20060102T150405Z"), id, beforeHash[:8]))
	entry.Targets = append(entry.Targets, req.Targets...)
	path := filepath.Join(opts.StateDir, id+".json")
	if err := writeJournal(path, &entry); err != nil {
		return Result{}, err
	}
	if err := prepareBackupDir(opts.BackupRoot, req.Client, entry.BackupDir); err != nil {
		return e.abort(path, &entry, opts, err)
	}
	for i, target := range req.Targets {
		jc, err := makeBeforeImage(entry.BackupDir, i, target)
		if err != nil {
			return e.abort(path, &entry, opts, err)
		}
		jc.RestoreStage = restoreStagePath(target, id, i)
		entry.Changes = append(entry.Changes, jc)
	}
	entry.State = StateBackedUp
	if err := writeJournal(path, &entry); err != nil {
		return e.abort(path, &entry, opts, err)
	}
	// An action may partially mutate every target. Mark all restore intent first.
	for i := range entry.Changes {
		entry.Changes[i].Applied = true
		entry.AppliedCount++
	}
	entry.State = StateValidated
	if err := writeJournal(path, &entry); err != nil {
		return e.abort(path, &entry, opts, err)
	}
	if err := inject(opts, FaultBeforeCommit); err != nil {
		return e.faultResult(path, &entry, opts, err)
	}
	stillBefore, err := journalChangesMatchBefore(entry.Changes)
	if err != nil {
		return e.cancelActionBeforeExecute(path, &entry, err)
	}
	if !stillBefore {
		return e.cancelActionBeforeExecute(path, &entry, ErrTargetChanged)
	}
	if err := inject(opts, FaultBeforePrivacy); err != nil {
		return e.faultResult(path, &entry, opts, err)
	}
	if err := preparePrivateActionSnapshots(&entry, req.PrecreateMissing); err != nil {
		return e.abort(path, &entry, opts, err)
	}
	if err := writeJournal(path, &entry); err != nil {
		return e.abort(path, &entry, opts, err)
	}
	untouchedTarget, err := makeActionTargetsPrivate(entry.Changes, req.PrecreateMissing)
	if err != nil {
		if untouchedTarget != "" {
			markActionTargetUnapplied(&entry, untouchedTarget)
		}
		return e.abort(path, &entry, opts, err)
	}
	if err := req.Execute(ctx, entry.BackupDir); err != nil {
		cause := sanitizeSecretsError(req.Secrets, err)
		if snapshotErr := captureAfterImages(&entry); snapshotErr != nil {
			cause = fmt.Errorf("%v; after-image capture failed: %w", cause, snapshotErr)
		} else if journalErr := writeJournal(path, &entry); journalErr != nil {
			cause = fmt.Errorf("%v; after-image journal failed: %w", cause, journalErr)
		}
		return e.abort(path, &entry, opts, cause)
	}
	if err := captureAfterImages(&entry); err != nil {
		return e.abort(path, &entry, opts, err)
	}
	if err := writeJournal(path, &entry); err != nil {
		return e.abort(path, &entry, opts, err)
	}
	if err := inject(opts, FaultAfterRename); err != nil {
		return e.faultResult(path, &entry, opts, err)
	}
	entry.State = StateCommitted
	if err := writeJournal(path, &entry); err != nil {
		return e.abort(path, &entry, opts, err)
	}
	if err := inject(opts, FaultAfterCommitted); err != nil {
		return e.faultResult(path, &entry, opts, err)
	}
	return resultFor(&entry), nil
}

func (e *FileEngine) cancelActionBeforeExecute(path string, entry *JournalEntry, cause error) (Result, error) {
	for i := range entry.Changes {
		entry.Changes[i].Applied = false
	}
	entry.AppliedCount = 0
	entry.State = StateRolledBack
	if err := writeJournal(path, entry); err != nil {
		entry.State = StateManualRecoveryRequired
		_ = writeJournal(path, entry)
		return resultFor(entry), manualRecoveryError(entry, fmt.Errorf("journal finalization failed: %v (original error: %v)", err, cause))
	}
	return resultFor(entry), cause
}

func markActionTargetUnapplied(entry *JournalEntry, target string) {
	for i := range entry.Changes {
		if entry.Changes[i].Target == target && entry.Changes[i].Applied {
			entry.Changes[i].Applied = false
			entry.AppliedCount--
			return
		}
	}
}

func (e *FileEngine) Recover(ctx context.Context) ([]JournalEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts, err := e.resolvedOptions()
	if err != nil {
		return nil, err
	}
	lock, err := acquireLock(opts.LockPath)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	files, err := filepath.Glob(filepath.Join(opts.StateDir, "*.json"))
	if err != nil {
		return nil, err
	}
	entries := make([]JournalEntry, 0, len(files))
	for _, path := range files {
		entry, err := readJournal(path)
		if err != nil {
			return entries, err
		}
		switch entry.State {
		case StateCommitted:
			if err := removeStages(entry.Changes); err != nil {
				entry.State = StateManualRecoveryRequired
				_ = writeJournal(path, &entry)
				return entries, manualRecoveryError(&entry, err)
			}
		case StateRolledBack:
			if err := removeStages(entry.Changes); err != nil {
				entry.State = StateManualRecoveryRequired
				_ = writeJournal(path, &entry)
				return entries, manualRecoveryError(&entry, err)
			}
		default:
			if err := rollback(path, &entry, opts); err != nil {
				if errors.Is(err, ErrCrashInjected) {
					return entries, err
				}
				entry.State = StateManualRecoveryRequired
				_ = writeJournal(path, &entry)
				return entries, manualRecoveryError(&entry, err)
			}
			clearAfterImages(&entry)
			entry.State = StateRolledBack
			if err := removeStages(entry.Changes); err != nil {
				entry.State = StateManualRecoveryRequired
				_ = writeJournal(path, &entry)
				return entries, manualRecoveryError(&entry, err)
			}
			if err := writeJournal(path, &entry); err != nil {
				entry.State = StateManualRecoveryRequired
				_ = writeJournal(path, &entry)
				return entries, manualRecoveryError(&entry, err)
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (e *FileEngine) abort(path string, entry *JournalEntry, opts Options, cause error) (Result, error) {
	entry.State = StateRollingBack
	_ = writeJournal(path, entry)
	if err := rollback(path, entry, opts); err != nil {
		if errors.Is(err, ErrCrashInjected) {
			return resultFor(entry), err
		}
		entry.State = StateManualRecoveryRequired
		_ = writeJournal(path, entry)
		return resultFor(entry), manualRecoveryError(entry, fmt.Errorf("%v (original error: %v)", err, cause))
	}
	clearAfterImages(entry)
	entry.State = StateRolledBack
	if err := removeStages(entry.Changes); err != nil {
		entry.State = StateManualRecoveryRequired
		_ = writeJournal(path, entry)
		return resultFor(entry), manualRecoveryError(entry, fmt.Errorf("%v (original error: %v)", err, cause))
	}
	if err := writeJournal(path, entry); err != nil {
		entry.State = StateManualRecoveryRequired
		_ = writeJournal(path, entry)
		return resultFor(entry), manualRecoveryError(entry, fmt.Errorf("journal finalization failed: %v (original error: %v)", err, cause))
	}
	return resultFor(entry), cause
}

func (e *FileEngine) faultResult(path string, entry *JournalEntry, opts Options, err error) (Result, error) {
	if errors.Is(err, ErrCrashInjected) {
		return resultFor(entry), err
	}
	return e.abort(path, entry, opts, err)
}

func (e *FileEngine) resolvedOptions() (Options, error) {
	o := e.options
	if o.StateDir == "" {
		d, err := os.UserConfigDir()
		if err != nil {
			return o, err
		}
		o.StateDir = filepath.Join(d, "relay-install", "state")
	}
	if o.BackupRoot == "" {
		d, err := os.UserConfigDir()
		if err != nil {
			return o, err
		}
		o.BackupRoot = filepath.Join(d, "relay-install", "backups")
	}
	if o.LockPath == "" {
		o.LockPath = filepath.Join(o.StateDir, "txn.lock")
	}
	var err error
	if o.StateDir, err = filepath.Abs(o.StateDir); err != nil {
		return o, err
	}
	if o.BackupRoot, err = filepath.Abs(o.BackupRoot); err != nil {
		return o, err
	}
	if o.LockPath, err = filepath.Abs(o.LockPath); err != nil {
		return o, err
	}
	if err := ensureJournalStateDir(o.StateDir); err != nil {
		return o, err
	}
	if err := os.MkdirAll(filepath.Dir(o.LockPath), 0o700); err != nil {
		return o, err
	}
	return o, nil
}

func resultFor(entry *JournalEntry) Result {
	return Result{TxnID: entry.TxnID, BackupDir: entry.BackupDir, State: entry.State, Applied: entry.AppliedCount}
}

func manualRecoveryError(entry *JournalEntry, cause error) error {
	return fmt.Errorf("%w: backup %s: %v", ErrManualRecoveryRequired, entry.BackupDir, cause)
}

func validateChange(c Change) error {
	var err error
	c.Key.Reveal(func(plaintext string) {
		err = contract.ValidateChange(c.ParserKind, c.Content, c.Blacklist, plaintext, c.AllowedSecretPaths)
		if err != nil && plaintext != "" && strings.Contains(err.Error(), plaintext) {
			err = errors.New("txn: content validation failed")
		}
	})
	return err
}

func validateRequestSecrets(req Request, extraMetadata ...string) error {
	metadata := make([]string, 0, 1+len(req.Changes)+len(extraMetadata))
	metadata = append(metadata, req.Client)
	for _, change := range req.Changes {
		metadata = append(metadata, change.Target)
	}
	metadata = append(metadata, extraMetadata...)
	return validateSecretsAgainstMetadata(requestKeys(req), metadata)
}

func requestKeys(req Request) []secret.Key {
	keys := make([]secret.Key, 0, len(req.Changes))
	for _, change := range req.Changes {
		keys = append(keys, change.Key)
	}
	return keys
}

func validateCrossChangeSecrets(req Request) error {
	for _, contentChange := range req.Changes {
		for _, selected := range req.Changes {
			leaks := false
			contentChange.Key.Reveal(func(ownPlaintext string) {
				selected.Key.Reveal(func(plaintext string) {
					leaks = plaintext != "" && plaintext != ownPlaintext && bytes.Contains(contentChange.Content, []byte(plaintext))
				})
			})
			if leaks {
				return errors.New("txn: selected key appears in another change content")
			}
		}
	}
	return nil
}

func validateActionRequestSecrets(req ActionRequest, extraMetadata ...string) error {
	metadata := make([]string, 0, 1+len(req.Targets)+len(extraMetadata))
	metadata = append(metadata, req.Client)
	metadata = append(metadata, req.Targets...)
	metadata = append(metadata, extraMetadata...)
	return validateSecretsAgainstMetadata(req.Secrets, metadata)
}

func validateSecretsAgainstMetadata(keys []secret.Key, metadata []string) error {
	for _, key := range keys {
		leaks := false
		key.Reveal(func(plaintext string) {
			if plaintext == "" {
				return
			}
			for _, value := range metadata {
				if strings.Contains(value, plaintext) {
					leaks = true
					return
				}
			}
		})
		if leaks {
			return errors.New("txn: selected key appears in journal metadata")
		}
	}
	return nil
}

func sanitizeSecretsError(keys []secret.Key, err error) error {
	if err == nil {
		return nil
	}
	for _, key := range keys {
		leaks := false
		key.Reveal(func(plaintext string) {
			leaks = plaintext != "" && strings.Contains(err.Error(), plaintext)
		})
		if leaks {
			return errors.New("txn: action failed with redacted output")
		}
	}
	return err
}

func sanitizeSecretError(key secret.Key, err error) error {
	if err == nil {
		return nil
	}
	leaks := false
	key.Reveal(func(plaintext string) {
		leaks = plaintext != "" && strings.Contains(err.Error(), plaintext)
	})
	if leaks {
		return errors.New("txn: validator failed with redacted output")
	}
	return err
}

func validateRequest(req Request) error {
	if !safeClient(req.Client) {
		return fmt.Errorf("txn: unsafe client %q", req.Client)
	}
	seen := make(map[string]struct{}, len(req.Changes))
	for _, c := range req.Changes {
		if err := validateTarget(c.Target, seen); err != nil {
			return err
		}
		if c.Perm.Perm() != 0o600 {
			return fmt.Errorf("txn: desired permission for %s must be 0600", c.Target)
		}
	}
	return nil
}

func validateActionRequest(req ActionRequest) error {
	if !safeClient(req.Client) || len(req.Targets) == 0 || req.IsNoop == nil || req.Execute == nil {
		return errors.New("txn: action client, targets, IsNoop, and Execute are required")
	}
	seen := make(map[string]struct{}, len(req.Targets))
	for _, target := range req.Targets {
		if err := validateTarget(target, seen); err != nil {
			return err
		}
	}
	precreateSeen := make(map[string]struct{}, len(req.PrecreateMissing))
	for _, target := range req.PrecreateMissing {
		if _, ok := seen[target]; !ok {
			return fmt.Errorf("txn: precreated path is not an action target: %s", target)
		}
		if _, duplicate := precreateSeen[target]; duplicate {
			return fmt.Errorf("txn: duplicate precreated target: %s", target)
		}
		precreateSeen[target] = struct{}{}
	}
	return nil
}

func ensureNoIncompleteJournals(stateDir string) error {
	paths, err := filepath.Glob(filepath.Join(stateDir, "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		entry, err := readJournal(path)
		if err != nil {
			return fmt.Errorf("%w: unreadable journal", ErrRecoveryRequired)
		}
		if entry.State != StateCommitted && entry.State != StateRolledBack {
			return fmt.Errorf("%w: transaction %s is %s", ErrRecoveryRequired, entry.TxnID, entry.State)
		}
	}
	return nil
}

func safeClient(client string) bool {
	return client != "" && client != "." && client != ".." && !strings.ContainsAny(client, `/\\`)
}

func validateTarget(target string, seen map[string]struct{}) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("txn: target must be absolute: %q", target)
	}
	if _, ok := seen[target]; ok {
		return fmt.Errorf("txn: duplicate target %q", target)
	}
	seen[target] = struct{}{}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("txn: target must be a non-symlink regular file: %s", target)
	}
	return nil
}

func (r Request) Targets() []string {
	targets := make([]string, 0, len(r.Changes))
	for _, c := range r.Changes {
		targets = append(targets, c.Target)
	}
	return targets
}

func allSame(changes []Change) (bool, error) {
	for _, c := range changes {
		data, err := os.ReadFile(c.Target)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		private, err := privateFileSecurityValid(c.Target)
		if err != nil {
			return false, err
		}
		if !equalBytes(data, c.Content) || !private {
			return false, nil
		}
	}
	return true, nil
}

func makeBeforeImage(dir string, index int, target string) (JournalChange, error) {
	jc := JournalChange{Target: target}
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return jc, nil
	}
	if err != nil {
		return jc, err
	}
	if !info.Mode().IsRegular() {
		return jc, fmt.Errorf("txn: target is not a regular file: %s", target)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return jc, err
	}
	if err := ensurePrivateDir(dir); err != nil {
		return jc, err
	}
	security, err := captureFileSecurity(target)
	if err != nil {
		return jc, err
	}
	jc.Existed, jc.Perm, jc.Hash, jc.Security = true, info.Mode().Perm(), hash(data), security
	jc.Backup = filepath.Join(dir, fmt.Sprintf("%02d-%s.before", index, hash([]byte(target))[:8]))
	if err := writePrivateFile(jc.Backup, data); err != nil {
		return jc, err
	}
	return jc, nil
}

func prepareBackupDir(root, client, transactionDir string) error {
	clientDir := filepath.Join(root, client)
	for _, dir := range []string{clientDir, transactionDir} {
		if err := ensurePrivateDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func captureAfterImages(entry *JournalEntry) error {
	for i := range entry.Changes {
		existed, contentHash, perm, security, err := currentSnapshot(entry.Changes[i].Target)
		if err != nil {
			return err
		}
		entry.Changes[i].AfterKnown = true
		entry.Changes[i].AfterExisted = existed
		entry.Changes[i].AfterHash = contentHash
		entry.Changes[i].AfterPerm = perm
		entry.Changes[i].AfterSecurity = security
	}
	return nil
}

func clearAfterImages(entry *JournalEntry) {
	for i := range entry.Changes {
		entry.Changes[i].AfterKnown = false
		entry.Changes[i].AfterExisted = false
		entry.Changes[i].AfterHash = ""
		entry.Changes[i].AfterPerm = 0
		entry.Changes[i].AfterSecurity = ""
	}
}

func currentSnapshot(path string) (bool, string, os.FileMode, string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", 0, "", nil
	}
	if err != nil {
		return false, "", 0, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, "", 0, "", fmt.Errorf("txn: snapshot target is not a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "", 0, "", err
	}
	security, err := captureFileSecurity(path)
	if err != nil {
		return false, "", 0, "", err
	}
	return true, hash(data), info.Mode().Perm(), security, nil
}

func beforeImageAggregate(targets []string) (string, error) {
	h := sha256.New()
	for _, target := range targets {
		_, _ = h.Write([]byte(target))
		info, err := os.Stat(target)
		if errors.Is(err, os.ErrNotExist) {
			_, _ = h.Write([]byte{0})
			continue
		}
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte{1})
		_, _ = h.Write([]byte(fmt.Sprintf("%#o", info.Mode().Perm())))
		security, err := captureFileSecurity(target)
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte(security))
		_, _ = h.Write([]byte(hash(data)))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func stagePath(target, txnID string, index int) string {
	return filepath.Join(filepath.Dir(target), fmt.Sprintf(".relay-install-stage-%s-%02d", txnID, index))
}

func restoreStagePath(target, txnID string, index int) string {
	return filepath.Join(filepath.Dir(target), fmt.Sprintf(".relay-install-restore-%s-%02d", txnID, index))
}

func stageFile(path string, content []byte) error {
	f, err := openExclusivePrivate(path)
	if err != nil {
		return err
	}
	if _, err = f.Write(content); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func validateStagedFile(path string, change Change) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("txn: staged path is not a regular file")
	}
	private, err := privateFileSecurityValid(path)
	if err != nil {
		return err
	}
	if !private {
		_ = applyPrivateFileSecurity(path)
		return errors.New("txn: staged file permission changed")
	}
	staged, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !equalBytes(staged, change.Content) {
		return errors.New("txn: staged content differs from requested content")
	}
	stagedChange := change
	stagedChange.Content = staged
	return validateChange(stagedChange)
}

func stageParentMissing(dir string) (bool, error) {
	_, err := os.Stat(dir)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	return false, err
}

func ensureStageParent(dir string) error {
	missing, err := stageParentMissing(dir)
	if err != nil || !missing {
		return err
	}
	return ensurePrivateDir(dir)
}

func rollback(journalPath string, entry *JournalEntry, opts Options) error {
	for i := len(entry.Changes) - 1; i >= 0; i-- {
		jc := &entry.Changes[i]
		if !jc.Applied {
			continue
		}
		if err := inject(opts, FaultBeforeRestore); err != nil {
			return err
		}
		alreadyBefore, err := matchesSnapshot(jc.Target, jc.Existed, jc.Hash, jc.Perm, jc.Security)
		if err != nil {
			return err
		}
		if alreadyBefore {
			jc.Applied = false
			jc.Restoring = false
			entry.AppliedCount--
			if err := removeRestoreStage(jc.RestoreStage); err != nil {
				return err
			}
			if err := writeJournal(journalPath, entry); err != nil {
				return err
			}
			continue
		}
		if !jc.AfterKnown {
			return fmt.Errorf("txn: target changed with no recorded after-image: %s", jc.Target)
		}
		if !jc.Restoring {
			stillAfter, err := matchesSnapshot(jc.Target, jc.AfterExisted, jc.AfterHash, jc.AfterPerm, jc.AfterSecurity)
			if err != nil {
				return err
			}
			if !stillAfter {
				return fmt.Errorf("txn: target changed after interruption: %s", jc.Target)
			}
			if jc.RestoreStage == "" {
				jc.RestoreStage = restoreStagePath(jc.Target, entry.TxnID, i)
			}
			jc.Restoring = true
			if err := writeJournal(journalPath, entry); err != nil {
				return err
			}
		}
		if jc.Existed {
			data, err := os.ReadFile(jc.Backup)
			if err != nil {
				return err
			}
			if hash(data) != jc.Hash {
				return fmt.Errorf("txn: corrupt backup %s", jc.Backup)
			}
			restoringBefore, err := matchesRestoringBefore(*jc)
			if err != nil {
				return err
			}
			if !restoringBefore {
				stillAfter, err := matchesSnapshot(jc.Target, jc.AfterExisted, jc.AfterHash, jc.AfterPerm, jc.AfterSecurity)
				if err != nil {
					return err
				}
				if !stillAfter {
					return fmt.Errorf("txn: target changed during restore: %s", jc.Target)
				}
				if err := ensureRestoreStage(jc.RestoreStage, data); err != nil {
					return err
				}
				if err := inject(opts, FaultAfterRestoreStage); err != nil {
					return err
				}
				stillAfter, err = matchesSnapshot(jc.Target, jc.AfterExisted, jc.AfterHash, jc.AfterPerm, jc.AfterSecurity)
				if err != nil {
					return err
				}
				if !stillAfter {
					return fmt.Errorf("txn: target changed before restore rename: %s", jc.Target)
				}
				if err := os.Rename(jc.RestoreStage, jc.Target); err != nil {
					return err
				}
				if err := syncDir(filepath.Dir(jc.Target)); err != nil {
					return err
				}
			}
			if err := restoreFileSecurity(jc.Target, jc.Security, jc.Perm); err != nil {
				return err
			}
		} else {
			stillAfter, err := matchesSnapshot(jc.Target, jc.AfterExisted, jc.AfterHash, jc.AfterPerm, jc.AfterSecurity)
			if err != nil {
				return err
			}
			if !stillAfter {
				return fmt.Errorf("txn: target changed before restore removal: %s", jc.Target)
			}
			if err := os.Remove(jc.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := syncDir(filepath.Dir(jc.Target)); err != nil {
				return err
			}
		}
		jc.Applied = false
		jc.Restoring = false
		entry.AppliedCount--
		if err := removeRestoreStage(jc.RestoreStage); err != nil {
			return err
		}
		if err := writeJournal(journalPath, entry); err != nil {
			return err
		}
	}
	return nil
}

func matchesRestoringBefore(change JournalChange) (bool, error) {
	perm, security, err := privateFileSnapshot()
	if err != nil {
		return false, err
	}
	return matchesSnapshot(change.Target, true, change.Hash, perm, security)
}

func ensureRestoreStage(path string, data []byte) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("txn: restore stage is not a regular file")
		}
		private, securityErr := privateFileSecurityValid(path)
		if securityErr != nil {
			return securityErr
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !private || hash(existing) != hash(data) {
			return errors.New("txn: restore stage failed integrity validation")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := openExclusivePrivate(path)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func removeRestoreStage(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return syncDir(filepath.Dir(path))
}

func matchesSnapshot(path string, existed bool, contentHash string, perm os.FileMode, security string) (bool, error) {
	currentExisted, currentHash, currentPerm, currentSecurity, err := currentSnapshot(path)
	if err != nil {
		return false, err
	}
	if currentExisted != existed {
		return false, nil
	}
	if !existed {
		return true, nil
	}
	return currentHash == contentHash && fileModeMatches(currentPerm, perm) && currentSecurity == security, nil
}

func journalChangesMatchBefore(changes []JournalChange) (bool, error) {
	for _, change := range changes {
		matches, err := matchesSnapshot(change.Target, change.Existed, change.Hash, change.Perm, change.Security)
		if err != nil || !matches {
			return matches, err
		}
	}
	return true, nil
}

func actionTargetsPrivate(targets []string) (bool, error) {
	for _, target := range targets {
		_, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		private, err := privateFileSecurityValid(target)
		if err != nil || !private {
			return false, err
		}
	}
	return true, nil
}

func preparePrivateActionSnapshots(entry *JournalEntry, precreateMissing []string) error {
	perm, security, err := privateFileSnapshot()
	if err != nil {
		return err
	}
	precreate := pathSet(precreateMissing)
	for i := range entry.Changes {
		entry.Changes[i].AfterKnown = true
		_, create := precreate[entry.Changes[i].Target]
		entry.Changes[i].AfterExisted = entry.Changes[i].Existed || create
		if !entry.Changes[i].AfterExisted {
			continue
		}
		entry.Changes[i].AfterHash = entry.Changes[i].Hash
		if !entry.Changes[i].Existed {
			entry.Changes[i].AfterHash = hash(nil)
		}
		entry.Changes[i].AfterPerm = perm
		entry.Changes[i].AfterSecurity = security
	}
	return nil
}

// makeActionTargetsPrivate returns untouchedTarget only when it detected a
// conflict before changing that target. The caller must exclude only that
// target from compensation while rolling back earlier privacy changes.
func makeActionTargetsPrivate(changes []JournalChange, precreateMissing []string) (untouchedTarget string, err error) {
	precreate := pathSet(precreateMissing)
	for _, change := range changes {
		matches, err := matchesSnapshot(change.Target, change.Existed, change.Hash, change.Perm, change.Security)
		if err != nil {
			return "", err
		}
		if !matches {
			return change.Target, ErrTargetChanged
		}
		if !change.Existed {
			if _, create := precreate[change.Target]; create {
				if err := createPrivateEmptyFile(change.Target); err != nil {
					if errors.Is(err, ErrTargetChanged) {
						return change.Target, err
					}
					return "", err
				}
				matches, err = matchesSnapshot(change.Target, true, change.AfterHash, change.AfterPerm, change.AfterSecurity)
				if err != nil || !matches {
					return "", errors.Join(ErrTargetChanged, err)
				}
			}
			continue
		}
		if err := applyPrivateFileSecurity(change.Target); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return change.Target, ErrTargetChanged
			}
			return "", err
		}
		matches, err = matchesSnapshot(change.Target, true, change.AfterHash, change.AfterPerm, change.AfterSecurity)
		if err != nil || !matches {
			return "", errors.Join(ErrTargetChanged, err)
		}
	}
	return "", nil
}

func pathSet(paths []string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		set[path] = struct{}{}
	}
	return set
}

func createPrivateEmptyFile(path string) error {
	f, err := openExclusivePrivate(path)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrTargetChanged
		}
		return err
	}
	if err = f.Sync(); err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func writeJournal(path string, entry *JournalEntry) error {
	entry.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return writePrivateFile(path, append(data, '\n'))
}
func readJournal(path string) (JournalEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return JournalEntry{}, err
	}
	var entry JournalEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return entry, fmt.Errorf("txn: invalid journal %s: %w", path, err)
	}
	return entry, nil
}
func writePrivateFile(path string, data []byte) error {
	tmp, f, err := privateTemp(path)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = applyPrivateFileSecurity(path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func privateTemp(path string) (string, *os.File, error) {
	for attempt := 0; attempt < 8; attempt++ {
		suffix, err := transactionID()
		if err != nil {
			return "", nil, err
		}
		tmp := path + ".new-" + suffix
		f, err := openExclusivePrivate(tmp)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return tmp, f, err
	}
	return "", nil, errors.New("txn: could not allocate private temporary file")
}
func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return applyPrivateDirSecurity(path)
}

func removeStages(changes []JournalChange) error {
	for _, c := range changes {
		if c.Stage != "" {
			if err := os.Remove(c.Stage); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := removeRestoreStage(c.RestoreStage); err != nil {
			return err
		}
		if c.StageParentCreated {
			// Only remove a directory created by this transaction, and only if empty.
			_ = os.Remove(filepath.Dir(c.Target))
		}
	}
	return nil
}
func inject(opts Options, p FaultPoint) error {
	if opts.FaultInjector == nil {
		return nil
	}
	return opts.FaultInjector(p)
}
func desiredPerm(c Change) os.FileMode {
	if c.Perm == 0 {
		return 0o600
	}
	return c.Perm.Perm()
}
func transactionID() (string, error) {
	b := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func hash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
