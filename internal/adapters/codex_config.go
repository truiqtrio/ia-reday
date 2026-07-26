package adapters

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"relay-install/internal/contract"
	"relay-install/internal/secret"
)

type CodexSafetyProfile string

const (
	CodexProfileGuarded      CodexSafetyProfile = "guarded"
	CodexProfileUnrestricted CodexSafetyProfile = "unrestricted"
)

// CodexConfig is the complete per-alias input. Model values are runtime-owned;
// the adapter deliberately does not guess them.
type CodexConfig struct {
	Alias                   string
	BaseURL                 string
	Model                   string
	ReviewModel             string
	ModelReasoningEffort    string
	PlanModeReasoningEffort string
	SafetyProfile           CodexSafetyProfile
	Key                     secret.Key
}

func codexWritePoint(path string) contract.WritePoint {
	return contract.WritePoint{
		Client: contract.ClientCodex, Kind: contract.ParserTOML, PathHint: path, Perm: 0o600,
		Managed:   append([]string(nil), contract.CodexManagedFields...),
		Blacklist: contract.CodexBlacklist,
	}
}

// GenerateCodexConfig is pure: it creates one self-contained, managed Codex
// profile and does not read or write the filesystem. The key enters only the
// permitted TOML field via secret.Key.Reveal.
func GenerateCodexConfig(cfg CodexConfig) ([]byte, error) {
	if err := validateCodexConfig(cfg); err != nil {
		return nil, err
	}
	approval, sandbox := contract.CodexApprovalOnRequest, contract.CodexSandboxWorkspaceWrite
	if cfg.SafetyProfile == CodexProfileUnrestricted {
		approval, sandbox = contract.CodexApprovalNever, contract.CodexSandboxDangerFullAccess
	}

	var b bytes.Buffer
	b.WriteString(contract.CodexManagedHeader)
	writeCodexTOMLString(&b, contract.CodexFieldModelProvider, contract.CodexProviderIntelalloc)
	writeCodexTOMLString(&b, contract.CodexFieldModel, cfg.Model)
	writeCodexTOMLString(&b, contract.CodexFieldReviewModel, cfg.ReviewModel)
	writeCodexTOMLString(&b, contract.CodexFieldModelReasoningEffort, cfg.ModelReasoningEffort)
	writeCodexTOMLString(&b, contract.CodexFieldPlanModeReasoningEffort, cfg.PlanModeReasoningEffort)
	writeCodexTOMLString(&b, contract.CodexFieldApprovalPolicy, approval)
	writeCodexTOMLString(&b, contract.CodexFieldSandboxMode, sandbox)
	b.WriteString("\n[" + contract.CodexFieldModelProviders + "." + contract.CodexProviderIntelalloc + "]\n")
	writeCodexTOMLString(&b, contract.CodexProviderFieldName, contract.CodexProviderIntelalloc)
	writeCodexTOMLString(&b, contract.CodexProviderFieldBaseURL, cfg.BaseURL)
	writeCodexTOMLString(&b, contract.CodexProviderFieldWireAPI, contract.CodexWireAPIResponses)
	var revealErr error
	cfg.Key.Reveal(func(plaintext string) {
		if plaintext == "" {
			revealErr = errors.New("codex: 空 key 拒绝生成")
			return
		}
		if !utf8.ValidString(plaintext) {
			revealErr = errors.New("codex: key 不是有效 UTF-8")
			return
		}
		writeCodexTOMLString(&b, contract.CodexProviderFieldBearerToken, plaintext)
	})
	if revealErr != nil {
		return nil, revealErr
	}
	content := b.Bytes()
	if err := ValidateCodexConfig(content); err != nil {
		return nil, err
	}
	return content, nil
}

func IsManagedCodexConfig(content []byte) bool {
	return bytes.HasPrefix(content, []byte(contract.CodexManagedHeader))
}

// ValidateCodexConfig is the local, deterministic first validation layer.
// A transaction can additionally inject CodexStrictConfigRunner for the real
// `codex exec --strict-config` invocation against its staged file.
func ValidateCodexConfig(content []byte) error {
	if !IsManagedCodexConfig(content) {
		return errors.New("codex: 缺少 relay-install 管理标记")
	}
	if err := contract.ValidateManagedCodexTOML(content); err != nil {
		return fmt.Errorf("codex: 配置不符合受管 TOML 契约: %w", err)
	}
	return nil
}

func validateCodexConfig(cfg CodexConfig) error {
	if err := validateCodexAlias(cfg.Alias); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{contract.CodexProviderFieldBaseURL, cfg.BaseURL},
		{contract.CodexFieldModel, cfg.Model},
		{contract.CodexFieldReviewModel, cfg.ReviewModel},
		{contract.CodexFieldModelReasoningEffort, cfg.ModelReasoningEffort},
		{contract.CodexFieldPlanModeReasoningEffort, cfg.PlanModeReasoningEffort},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("codex: %s 为空", field.name)
		}
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("codex: %s 不是有效 UTF-8", field.name)
		}
	}
	if cfg.SafetyProfile != "" && cfg.SafetyProfile != CodexProfileGuarded && cfg.SafetyProfile != CodexProfileUnrestricted {
		return fmt.Errorf("codex: 未知安全档位 %q", cfg.SafetyProfile)
	}
	return nil
}

func validateCodexAlias(alias string) error {
	if alias == "" || filepath.Base(alias) != alias || strings.ContainsAny(alias, "\\/") {
		return fmt.Errorf("codex: 非法 alias %q", alias)
	}
	for _, r := range alias {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return fmt.Errorf("codex: 非法 alias %q", alias)
		}
	}
	return nil
}

func writeCodexTOMLString(b *bytes.Buffer, key, value string) {
	encoded, _ := json.Marshal(value)
	b.WriteString(key)
	b.WriteString(" = ")
	b.Write(encoded)
	b.WriteByte('\n')
}
