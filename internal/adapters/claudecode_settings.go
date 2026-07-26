package adapters

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"relay-install/internal/contract"
	"relay-install/internal/secret"
)

// ErrClaudeCodeSettingsInvalid is a stopping-condition error. A pre-existing
// settings file that cannot be parsed must never be overwritten.
var ErrClaudeCodeSettingsInvalid = errors.New("claudecode: settings.json is invalid")

// ClaudeCodeModels selects Claude Code's three model defaults. Empty fields use
// the adopted relay defaults.
type ClaudeCodeModels struct {
	Opus   string
	Sonnet string
	Haiku  string
}

const (
	defaultClaudeCodeOpus   = "claude-opus-4-8"
	defaultClaudeCodeSonnet = "claude-sonnet-5[1M]"
	defaultClaudeCodeHaiku  = "claude-fable-5"
)

// MergeClaudeCodeSettings is pure: it does not read or write settings.json.
// It preserves top-level and env member order where possible by keeping raw JSON
// values for all non-managed members. JSON whitespace is normalized only around
// the object members we serialize.
//
// key is deliberately a secret.Key: the token reaches the result only through
// Key.Reveal and is never accepted as a plain string by this API.
func MergeClaudeCodeSettings(existing []byte, baseURL string, key secret.Key, models ClaudeCodeModels) ([]byte, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("claudecode: base URL 为空")
	}
	if len(bytes.TrimSpace(existing)) == 0 {
		existing = []byte("{}")
	}
	root, err := contract.ParseOrderedJSONObject(existing)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrClaudeCodeSettingsInvalid, err)
	}

	env := contract.OrderedJSONObject{}
	if raw, ok := root.Get(contract.ClaudeCodeFieldEnv); ok {
		env, err = contract.ParseOrderedJSONObject(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: env must be a JSON object: %v", ErrClaudeCodeSettingsInvalid, err)
		}
	}

	models = models.withDefaults()
	setString := func(name, value string) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("claudecode: marshal %s: %w", name, err)
		}
		env.Set(name, encoded)
		return nil
	}
	if err := setString(contract.ClaudeCodeEnvBaseURL, baseURL); err != nil {
		return nil, err
	}
	var revealErr error
	key.Reveal(func(plaintext string) {
		if plaintext == "" {
			revealErr = errors.New("claudecode: 空 key 拒绝生成")
			return
		}
		if err := setString(contract.ClaudeCodeEnvAuthToken, plaintext); err != nil {
			revealErr = err
			return
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{contract.ClaudeCodeEnvDefaultOpusModel, models.Opus},
			{contract.ClaudeCodeEnvDefaultSonnetModel, models.Sonnet},
			{contract.ClaudeCodeEnvDefaultHaikuModel, models.Haiku},
		} {
			if err := setString(field.name, field.value); err != nil {
				revealErr = err
				return
			}
		}
		root.Set(contract.ClaudeCodeFieldEnv, env.Marshal())
		count, err := contract.CountJSONStringsContaining(root.Marshal(), plaintext)
		if err != nil {
			revealErr = fmt.Errorf("claudecode: scan merged settings: %w", err)
			return
		}
		if count != 1 {
			revealErr = fmt.Errorf("claudecode: selected key appears %d times; expected only %s.%s", count, contract.ClaudeCodeFieldEnv, contract.ClaudeCodeEnvAuthToken)
		}
	})
	if revealErr != nil {
		return nil, revealErr
	}
	return root.Marshal(), nil
}

// GenerateClaudeCodeChange pairs the pure merged content with its txn write
// point. It performs no filesystem IO; txn owns the eventual 0600 write.
func GenerateClaudeCodeChange(target string, existing []byte, baseURL string, key secret.Key, models ClaudeCodeModels) (Change, error) {
	existed := existing != nil
	content, err := MergeClaudeCodeSettings(existing, baseURL, key, models)
	if err != nil {
		return Change{}, err
	}
	change := Change{
		Point:              ClaudeCodeSettingsPoint(target),
		Content:            content,
		Secret:             key,
		AllowedSecretPaths: [][]string{{contract.ClaudeCodeFieldEnv, contract.ClaudeCodeEnvAuthToken}},
	}
	rememberBefore(&change, existing, existed)
	return change, nil
}

func (m ClaudeCodeModels) withDefaults() ClaudeCodeModels {
	if m.Opus == "" {
		m.Opus = defaultClaudeCodeOpus
	}
	if m.Sonnet == "" {
		m.Sonnet = defaultClaudeCodeSonnet
	}
	if m.Haiku == "" {
		m.Haiku = defaultClaudeCodeHaiku
	}
	return m
}

// ClaudeCodeSettingsPoint describes the generated file for txn.
func ClaudeCodeSettingsPoint(target string) contract.WritePoint {
	return contract.WritePoint{
		Client:   contract.ClientClaudeCode,
		Kind:     contract.ParserJSON,
		PathHint: target,
		Perm:     0o600,
		Managed:  append([]string(nil), contract.ClaudeCodeManagedEnv...),
	}
}

func validateClaudeCodeSettings(data []byte) error {
	_, err := contract.ParseOrderedJSONObject(data)
	return err
}
