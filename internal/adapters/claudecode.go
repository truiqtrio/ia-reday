package adapters

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"relay-install/internal/contract"
	"relay-install/internal/probe"
	"relay-install/internal/txn"
)

// claudeCodeAdapter Claude Code:merge 式写 settings.json env 块,
// 只覆盖显式枚举的 5 个受管变量,其余键语义保留,0600。
type claudeCodeAdapter struct {
	// home and target are test seams. target, when present, is the complete
	// settings.json path; otherwise target is resolved below home.
	home    string
	target  string
	lookup  func(string) (string, error)
	version func(context.Context, string) (string, error)
	engine  txn.Engine
}

func NewClaudeCode() Adapter { return claudeCodeAdapter{} }

func (claudeCodeAdapter) ID() contract.ClientID { return contract.ClientClaudeCode }

// Detect 查 claude 二进制与 ~/.claude/settings.json 存在性(只读)
func (a claudeCodeAdapter) Detect(ctx context.Context) (DetectResult, error) {
	res := DetectResult{Client: contract.ClientClaudeCode}
	lookup := a.lookup
	if lookup == nil {
		lookup = exec.LookPath
	}
	if path, err := lookup("claude"); err == nil {
		res.Installed = true
		res.Path = path
		version := a.version
		if version == nil {
			version = func(ctx context.Context, path string) (string, error) {
				out, err := exec.CommandContext(ctx, path, "--version").Output()
				return string(out), err
			}
		}
		if out, err := version(ctx, path); err == nil {
			res.Version = strings.TrimSpace(out)
		}
	}
	settings, err := a.settingsPath()
	if err != nil {
		return res, err
	}
	if data, err := os.ReadFile(settings); err == nil {
		if err := validateClaudeCodeSettings(data); err != nil {
			return res, fmt.Errorf("%w: %s: %v", ErrClaudeCodeSettingsInvalid, settings, err)
		}
		res.Detail = settings + " 已存在,将合并"
		if res.Path == "" {
			res.Path = settings
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	if !res.Installed && res.Detail == "" {
		res.Detail = "未安装"
	}
	return res, nil
}

func (a claudeCodeAdapter) settingsPath() (string, error) {
	if a.target != "" {
		return a.target, nil
	}
	home := a.home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("claudecode: resolve home: %w", err)
		}
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func (claudeCodeAdapter) Plan(context.Context, PlanRequest) (Plan, error) {
	return Plan{}, ErrNotImplemented
}

func (claudeCodeAdapter) Validate(_ context.Context, set ChangeSet) error {
	if set.Client != contract.ClientClaudeCode {
		return fmt.Errorf("claudecode: 错误的 ChangeSet 客户端 %q", set.Client)
	}
	if set.CCSwitch != nil || len(set.Changes) == 0 {
		return errors.New("claudecode: ChangeSet 必须包含文件变更")
	}
	if err := validateChangeSetSecretIsolation(set); err != nil {
		return err
	}
	for _, change := range set.Changes {
		if err := validateClaudeCodeChange(change); err != nil {
			return sanitizeChangeSetError(set, err)
		}
	}
	return nil
}

func (a claudeCodeAdapter) Apply(ctx context.Context, set ChangeSet) (txn.Result, error) {
	if err := a.Validate(ctx, set); err != nil {
		return txn.Result{}, err
	}
	changes := make([]txn.Change, 0, len(set.Changes))
	for _, change := range set.Changes {
		target := change.Point.PathHint
		changes = append(changes, toTxnChange(change,
			func() error {
				if err := checkClaudeCodeTarget(target); err != nil {
					return err
				}
				return checkExpectedBefore(change)
			},
			func(stagedPath string) error {
				content, err := os.ReadFile(stagedPath)
				if err != nil {
					return fmt.Errorf("claudecode: 读取待校验 settings.json: %w", err)
				}
				if err := validateClaudeCodeSettings(content); err != nil {
					return fmt.Errorf("%w: %v", ErrClaudeCodeSettingsInvalid, err)
				}
				return nil
			},
		))
	}
	return defaultTxnEngine(a.engine).Apply(ctx, txn.Request{Client: string(contract.ClientClaudeCode), Changes: changes})
}

// Probe Messages 最小请求(max_tokens=1);"热生效"存疑,v1 不依赖它做验证
func (claudeCodeAdapter) Probe(context.Context) (probe.Result, error) {
	return probe.Result{}, ErrNotImplemented
}

func validateClaudeCodeChange(change Change) error {
	if change.Point.Client != contract.ClientClaudeCode || change.Point.Kind != contract.ParserJSON || change.Point.Perm != 0o600 {
		return errors.New("claudecode: Change 写入点不符合受管契约")
	}
	var validationErr error
	change.Secret.Reveal(func(plaintext string) {
		validationErr = contract.ValidateChange(
			contract.ParserJSON,
			change.Content,
			change.Point.Blacklist,
			plaintext,
			change.AllowedSecretPaths,
		)
	})
	return sanitizeKeyError(change.Secret, validationErr)
}

func checkClaudeCodeTarget(path string) error {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claudecode: 读取 settings.json: %w", err)
	}
	if err := validateClaudeCodeSettings(content); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrClaudeCodeSettingsInvalid, path, err)
	}
	return nil
}
