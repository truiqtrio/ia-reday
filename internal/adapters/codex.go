package adapters

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"relay-install/internal/contract"
	"relay-install/internal/probe"
	"relay-install/internal/txn"
)

// codexAdapter Codex CLI:写 $CODEX_HOME/intelalloc-<alias>.config.toml,
// 仅含已验证字段 + wire_api="responses",明确剔除三个废弃字段。
const minCodexVersion = "0.134.0"

var (
	// ErrCodexVersionTooOld means the installed Codex CLI cannot safely consume
	// the managed configuration format.
	ErrCodexVersionTooOld = errors.New("codex: 版本低于 0.134,拒绝配置")
	ErrCodexVersionParse  = errors.New("codex: 无法解析版本")
	ErrCodexVersionQuery  = errors.New("codex: 无法读取版本")
	ErrCodexConfigInvalid = errors.New("codex: 受管配置无法解析")
	// ErrCodexConfigConflict means a target profile belongs to somebody else.
	ErrCodexConfigConflict = errors.New("codex: 目标配置不是 relay-install 管理,拒绝覆盖")
)

// CodexStrictConfigRunner permits the transaction layer to validate a staged
// profile with `codex exec --strict-config`. It deliberately receives only a
// path: this adapter never writes the staged file or prints command output.
type CodexStrictConfigRunner interface {
	StrictConfig(ctx context.Context, binary, configPath string) error
}

type codexAdapter struct {
	path               string
	codexHome          string
	lookupPath         func(string) (string, error)
	commandOutput      func(context.Context, string, ...string) ([]byte, error)
	strictConfigRunner CodexStrictConfigRunner
	engine             txn.Engine
}

func NewCodex() Adapter { return codexAdapter{} }

func (codexAdapter) ID() contract.ClientID { return contract.ClientCodex }

// Detect 查 codex 二进制与版本(只读),版本低于 0.134 直接拒绝。
func (a codexAdapter) Detect(ctx context.Context) (DetectResult, error) {
	res := DetectResult{Client: contract.ClientCodex}
	codexHome, homeErr := a.resolveCodexHome()
	if homeErr != nil {
		return res, homeErr
	}
	res.Detail = "CODEX_HOME=" + codexHome
	lookup := a.lookupPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	path := a.path
	var err error
	if path == "" {
		path, err = lookup("codex")
	}
	if err != nil {
		res.Detail += "; 未安装"
		return res, nil
	}
	res.Installed = true
	res.Path = path
	output := a.commandOutput
	if output == nil {
		output = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	out, err := output(ctx, path, "--version")
	if err != nil {
		res.Detail += "; 版本读取失败"
		return res, fmt.Errorf("%w: %v", ErrCodexVersionQuery, err)
	}
	parsedVersion, err := parseCodexVersion(string(out))
	if err != nil {
		return res, err
	}
	res.Version = fmt.Sprintf("%d.%d.%d", parsedVersion[0], parsedVersion[1], parsedVersion[2])
	comparison, err := compareCodexVersion(res.Version, minCodexVersion)
	if err != nil {
		return res, err
	}
	if comparison < 0 {
		res.Detail += "; 版本低于 0.134"
		return res, ErrCodexVersionTooOld
	}
	if comparison == 0 && codexPrereleasePattern.MatchString(string(out)) {
		res.Detail += "; 预发布版本低于 0.134.0"
		return res, ErrCodexVersionTooOld
	}
	return res, nil
}

func (codexAdapter) Plan(context.Context, PlanRequest) (Plan, error) {
	return Plan{}, ErrNotImplemented
}

func (a codexAdapter) Validate(ctx context.Context, set ChangeSet) error {
	if set.Client != contract.ClientCodex {
		return fmt.Errorf("codex: 错误的 ChangeSet 客户端 %q", set.Client)
	}
	if set.CCSwitch != nil || len(set.Changes) == 0 {
		return errors.New("codex: ChangeSet 必须包含文件变更")
	}
	if err := validateChangeSetSecretIsolation(set); err != nil {
		return err
	}
	for _, change := range set.Changes {
		if err := validateCodexChange(change); err != nil {
			return sanitizeChangeSetError(set, err)
		}
		if a.strictConfigRunner != nil {
			binary, err := a.codexBinary()
			if err != nil {
				return err
			}
			// The txn layer passes the staged configuration path in PathHint when
			// invoking validation; this adapter never materializes that file.
			if err := a.strictConfigRunner.StrictConfig(ctx, binary, change.Point.PathHint); err != nil {
				return sanitizeChangeSetError(set, fmt.Errorf("codex: strict-config 校验失败: %w", err))
			}
		}
	}
	return nil
}

// TODO: provide the production CodexStrictConfigRunner. It must invoke
// `codex exec --strict-config` without putting a
// credential in argv, env, stdout, or stderr.

func (a codexAdapter) Apply(ctx context.Context, set ChangeSet) (txn.Result, error) {
	if set.Client != contract.ClientCodex {
		return txn.Result{}, fmt.Errorf("codex: 错误的 ChangeSet 客户端 %q", set.Client)
	}
	if set.CCSwitch != nil || len(set.Changes) == 0 {
		return txn.Result{}, errors.New("codex: ChangeSet 必须包含文件变更")
	}
	if err := validateChangeSetSecretIsolation(set); err != nil {
		return txn.Result{}, err
	}

	binary := ""
	if a.strictConfigRunner != nil {
		var err error
		binary, err = a.codexBinary()
		if err != nil {
			return txn.Result{}, err
		}
	}
	changes := make([]txn.Change, 0, len(set.Changes))
	for _, change := range set.Changes {
		if err := validateCodexChange(change); err != nil {
			return txn.Result{}, sanitizeChangeSetError(set, err)
		}
		target := change.Point.PathHint
		changes = append(changes, toTxnChange(change,
			func() error {
				if err := checkManagedCodexTarget(target); err != nil {
					return err
				}
				return checkExpectedBefore(change)
			},
			func(stagedPath string) error {
				content, err := os.ReadFile(stagedPath)
				if err != nil {
					return fmt.Errorf("codex: 读取待校验配置: %w", err)
				}
				if err := ValidateCodexConfig(content); err != nil {
					return err
				}
				if a.strictConfigRunner != nil {
					if err := a.strictConfigRunner.StrictConfig(ctx, binary, stagedPath); err != nil {
						return fmt.Errorf("codex: strict-config 校验失败: %w", err)
					}
				}
				return nil
			},
		))
	}
	return defaultTxnEngine(a.engine).Apply(ctx, txn.Request{Client: string(contract.ClientCodex), Changes: changes})
}

// Probe 语法级 codex exec --strict-config + 语义级 codex -p <profile> exec 最小真实调用
// (默认开,--skip-live 可关);codex doctor 不出现在任何验证路径。
func (codexAdapter) Probe(context.Context) (probe.Result, error) {
	return probe.Result{}, ErrNotImplemented
}

func (a codexAdapter) resolveCodexHome() (string, error) {
	if a.codexHome != "" {
		return a.codexHome, nil
	}
	if fromEnv := os.Getenv("CODEX_HOME"); fromEnv != "" {
		return fromEnv, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codex: 解析默认 CODEX_HOME: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

func (a codexAdapter) codexBinary() (string, error) {
	if a.path != "" {
		return a.path, nil
	}
	lookup := a.lookupPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	path, err := lookup("codex")
	if err != nil {
		return "", fmt.Errorf("codex: 未找到 strict-config 可执行文件: %w", err)
	}
	return path, nil
}

// CodexConfigPath returns the isolated profile path for an alias.
func CodexConfigPath(codexHome, alias string) (string, error) {
	if err := validateCodexAlias(alias); err != nil {
		return "", err
	}
	if codexHome == "" {
		return "", errors.New("codex: CODEX_HOME 为空")
	}
	return filepath.Join(codexHome, contract.CodexConfigFilename(alias)), nil
}

// PlanCodexConfig reads only the selected target and refuses to overwrite an
// unmanaged profile. Managed profiles are intentionally replaceable.
func (a codexAdapter) PlanCodexConfig(cfg CodexConfig) (Change, error) {
	home, err := a.resolveCodexHome()
	if err != nil {
		return Change{}, err
	}
	path, err := CodexConfigPath(home, cfg.Alias)
	if err != nil {
		return Change{}, err
	}
	existing, existed, err := readManagedCodexTarget(path)
	if err != nil {
		return Change{}, sanitizeKeyError(cfg.Key, err)
	}
	content, err := GenerateCodexConfig(cfg)
	if err != nil {
		return Change{}, err
	}
	change := Change{
		Point:   codexWritePoint(path),
		Content: content,
		Secret:  cfg.Key,
		AllowedSecretPaths: [][]string{{
			contract.CodexFieldModelProviders,
			contract.CodexProviderIntelalloc,
			contract.CodexProviderFieldBearerToken,
		}},
	}
	rememberBefore(&change, existing, existed)
	return change, nil
}

func validateCodexChange(change Change) error {
	if change.Point.Client != contract.ClientCodex || change.Point.Kind != contract.ParserTOML || change.Point.Perm != 0o600 {
		return errors.New("codex: Change 写入点不符合受管契约")
	}
	if err := ValidateCodexConfig(change.Content); err != nil {
		return sanitizeKeyError(change.Secret, err)
	}
	return nil
}

func checkManagedCodexTarget(path string) error {
	_, _, err := readManagedCodexTarget(path)
	return err
}

func readManagedCodexTarget(path string) ([]byte, bool, error) {
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("codex: 读取目标配置: %w", err)
	}
	if !IsManagedCodexConfig(existing) {
		return nil, true, fmt.Errorf("%w: %s", ErrCodexConfigConflict, path)
	}
	if err := contract.ValidateManagedCodexTOML(existing); err != nil {
		return nil, true, fmt.Errorf("%w: %s", ErrCodexConfigInvalid, path)
	}
	return existing, true, nil
}

var codexVersionPattern = regexp.MustCompile(`(?:^|[^0-9])v?([0-9]+)\.([0-9]+)(?:\.([0-9]+))?(?:$|[^0-9])`)
var codexPrereleasePattern = regexp.MustCompile(`(?:^|[^0-9])v?[0-9]+\.[0-9]+(?:\.[0-9]+)?-[0-9A-Za-z]`)

func compareCodexVersion(got, want string) (int, error) {
	gv, err := parseCodexVersion(got)
	if err != nil {
		return 0, err
	}
	wv, err := parseCodexVersion(want)
	if err != nil {
		return 0, fmt.Errorf("codex: 内部最小版本错误: %w", err)
	}
	for i := range gv {
		if gv[i] != wv[i] {
			if gv[i] < wv[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func parseCodexVersion(raw string) ([3]int, error) {
	match := codexVersionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return [3]int{}, fmt.Errorf("%w: %q", ErrCodexVersionParse, strings.TrimSpace(raw))
	}
	var version [3]int
	for i := 0; i < 3; i++ {
		if match[i+1] == "" {
			continue
		}
		if _, err := fmt.Sscanf(match[i+1], "%d", &version[i]); err != nil {
			return [3]int{}, fmt.Errorf("%w: %q: %v", ErrCodexVersionParse, match[0], err)
		}
	}
	return version, nil
}
