package adapters

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"strings"

	"relay-install/internal/secret"
	"relay-install/internal/txn"
)

var ErrAdapterStale = errors.New("adapters: target changed since generation")

func toTxnChange(change Change, precondition func() error, postValidate func(string) error) txn.Change {
	return txn.Change{
		Target:             change.Point.PathHint,
		Content:            change.Content,
		Perm:               os.FileMode(change.Point.Perm),
		ParserKind:         change.Point.Kind,
		Blacklist:          append([]string(nil), change.Point.Blacklist...),
		Key:                change.Secret,
		AllowedSecretPaths: cloneSecretPaths(change.AllowedSecretPaths),
		Precondition:       precondition,
		PostValidate:       postValidate,
	}
}

func cloneSecretPaths(paths [][]string) [][]string {
	out := make([][]string, len(paths))
	for i := range paths {
		out[i] = append([]string(nil), paths[i]...)
	}
	return out
}

func defaultTxnEngine(engine txn.Engine) txn.Engine {
	if engine != nil {
		return engine
	}
	return txn.NewFileEngine(txn.Options{})
}

func sanitizeKeyError(key secret.Key, err error) error {
	return sanitizeKeysError([]secret.Key{key}, err)
}

func sanitizeChangeSetError(set ChangeSet, err error) error {
	return sanitizeKeysError(changeSetKeys(set), err)
}

func sanitizeKeysError(keys []secret.Key, err error) error {
	if err == nil {
		return nil
	}
	for _, key := range keys {
		leaks := false
		key.Reveal(func(plaintext string) {
			leaks = plaintext != "" && strings.Contains(err.Error(), plaintext)
		})
		if leaks {
			return errors.New("adapters: validation failed with redacted output")
		}
	}
	return err
}

func changeSetKeys(set ChangeSet) []secret.Key {
	keys := make([]secret.Key, 0, len(set.Changes))
	for _, change := range set.Changes {
		keys = append(keys, change.Secret)
	}
	return keys
}

func validateChangeSetSecretIsolation(set ChangeSet) error {
	for _, contentChange := range set.Changes {
		for _, selected := range set.Changes {
			leaks := false
			contentChange.Secret.Reveal(func(ownPlaintext string) {
				selected.Secret.Reveal(func(plaintext string) {
					leaks = plaintext != "" && plaintext != ownPlaintext && bytes.Contains(contentChange.Content, []byte(plaintext))
				})
			})
			if leaks {
				return errors.New("adapters: selected key appears in another change content")
			}
		}
	}
	return nil
}

func rememberBefore(change *Change, content []byte, existed bool) {
	change.BeforeKnown = true
	change.BeforeExisted = existed
	if existed {
		change.BeforeHash = sha256.Sum256(content)
	}
}

func checkExpectedBefore(change Change) error {
	if !change.BeforeKnown {
		return nil
	}
	content, err := os.ReadFile(change.Point.PathHint)
	if errors.Is(err, os.ErrNotExist) {
		if !change.BeforeExisted {
			return nil
		}
		return ErrAdapterStale
	}
	if err != nil {
		return err
	}
	if sha256.Sum256(content) == sha256.Sum256(change.Content) {
		return nil
	}
	if !change.BeforeExisted || sha256.Sum256(content) != change.BeforeHash {
		return ErrAdapterStale
	}
	return nil
}
