package contract

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type codexTOMLField struct {
	key string
}

var codexManagedTopFields = []codexTOMLField{
	{CodexFieldModelProvider},
	{CodexFieldModel},
	{CodexFieldReviewModel},
	{CodexFieldModelReasoningEffort},
	{CodexFieldPlanModeReasoningEffort},
	{CodexFieldApprovalPolicy},
	{CodexFieldSandboxMode},
}

var codexManagedProviderFields = []codexTOMLField{
	{CodexProviderFieldName},
	{CodexProviderFieldBaseURL},
	{CodexProviderFieldWireAPI},
	{CodexProviderFieldBearerToken},
}

// ValidateManagedCodexTOML validates the strict TOML subset emitted by v1.
// It is intentionally narrower than general TOML: a managed file that drifts
// from this schema stops replacement instead of being guessed at. JSON string
// literals are a compatible subset of TOML basic strings after the extra
// solidus/surrogate checks below.
func ValidateManagedCodexTOML(data []byte) error {
	_, err := parseManagedCodexTOML(data)
	return err
}

func parseManagedCodexTOML(data []byte) (map[string]string, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("codex TOML is not valid UTF-8")
	}
	text := strings.TrimSuffix(string(data), "\n")
	lines := strings.Split(text, "\n")
	wantLines := 1 + len(codexManagedTopFields) + 1 + 1 + len(codexManagedProviderFields)
	if len(lines) != wantLines {
		return nil, fmt.Errorf("codex TOML line count %d, want %d", len(lines), wantLines)
	}
	if lines[0]+"\n" != CodexManagedHeader {
		return nil, errors.New("codex TOML missing managed header")
	}
	values := make(map[string]string, len(codexManagedTopFields)+len(codexManagedProviderFields))
	line := 1
	for _, field := range codexManagedTopFields {
		value, err := parseManagedTOMLStringLine(lines[line], field.key)
		if err != nil {
			return nil, err
		}
		values[field.key] = value
		line++
	}
	if lines[line] != "" {
		return nil, errors.New("codex TOML missing provider separator")
	}
	line++
	wantTable := "[" + CodexFieldModelProviders + "." + CodexProviderIntelalloc + "]"
	if lines[line] != wantTable {
		return nil, fmt.Errorf("codex TOML provider table does not match %q", wantTable)
	}
	line++
	for _, field := range codexManagedProviderFields {
		value, err := parseManagedTOMLStringLine(lines[line], field.key)
		if err != nil {
			return nil, err
		}
		values[CodexFieldModelProviders+"."+CodexProviderIntelalloc+"."+field.key] = value
		line++
	}
	for key, value := range values {
		if value == "" {
			return nil, fmt.Errorf("codex TOML field %q is empty", key)
		}
	}
	if values[CodexFieldModelProvider] != CodexProviderIntelalloc ||
		values[CodexFieldModelProviders+"."+CodexProviderIntelalloc+"."+CodexProviderFieldName] != CodexProviderIntelalloc ||
		values[CodexFieldModelProviders+"."+CodexProviderIntelalloc+"."+CodexProviderFieldWireAPI] != CodexWireAPIResponses {
		return nil, errors.New("codex TOML fixed provider values do not match v1 contract")
	}
	approval, sandbox := values[CodexFieldApprovalPolicy], values[CodexFieldSandboxMode]
	if !((approval == CodexApprovalOnRequest && sandbox == CodexSandboxWorkspaceWrite) ||
		(approval == CodexApprovalNever && sandbox == CodexSandboxDangerFullAccess)) {
		return nil, errors.New("codex TOML safety profile is not a supported pair")
	}
	return values, nil
}

func parseManagedTOMLStringLine(line, wantKey string) (string, error) {
	prefix := wantKey + " = "
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("codex TOML field line does not match %q", wantKey)
	}
	raw := strings.TrimPrefix(line, prefix)
	if strings.Contains(raw, `\/`) {
		return "", fmt.Errorf("codex TOML field %q uses unsupported solidus escape", wantKey)
	}
	for start := 0; ; {
		i := strings.Index(raw[start:], `\u`)
		if i < 0 {
			break
		}
		i += start
		if i+6 > len(raw) {
			return "", fmt.Errorf("codex TOML field %q has incomplete Unicode escape", wantKey)
		}
		decoded, err := hex.DecodeString(raw[i+2 : i+6])
		if err != nil {
			return "", fmt.Errorf("codex TOML field %q has invalid Unicode escape", wantKey)
		}
		code := uint16(decoded[0])<<8 | uint16(decoded[1])
		if code >= 0xd800 && code <= 0xdfff {
			return "", fmt.Errorf("codex TOML field %q contains a surrogate escape", wantKey)
		}
		start = i + 6
	}
	var value string
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", fmt.Errorf("codex TOML field %q is not a supported basic string: %w", wantKey, err)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("codex TOML field %q contains a control character", wantKey)
		}
	}
	return value, nil
}
