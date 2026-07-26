package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relay-install/internal/contract"
	"relay-install/internal/secret"
)

const claudeCodeCanary = "sk-claude-canary-9Qx7Wm2ZaBcD"

func testClaudeCodeKey(t *testing.T) secret.Key {
	t.Helper()
	key, err := secret.New("claude-test", claudeCodeCanary)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestMergeClaudeCodeSettingsPreservesUnmanagedSemantics(t *testing.T) {
	input := []byte(`{
  "permissions": {"allow": ["Bash(git:*)"], "deny": ["Read(.env)"]},
  "env": {
    "KEEP_ME": {"nested": [1, true, null]},
    "ANTHROPIC_AUTH_TOKEN": "old-token",
    "ANTHROPIC_API_KEY": "legacy-kept",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "old-opus"
  },
  "theme": "dark",
  "unknown": ["one", {"two": 2}],
  "mcpServers": {"local": {"command": "node", "args": ["server.js"], "env": {"MCP_MODE": "test"}}}
}`)
	merged, err := MergeClaudeCodeSettings(input, "https://relay.example.test", testClaudeCodeKey(t), ClaudeCodeModels{})
	if err != nil {
		t.Fatal(err)
	}

	var before, after map[string]any
	if err := json.Unmarshal(input, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(merged, &after); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"permissions", "theme", "unknown", "mcpServers"} {
		if !semanticEqual(before[field], after[field]) {
			t.Errorf("unmanaged top-level field %s changed: %#v", field, after[field])
		}
	}
	beforeEnv := before["env"].(map[string]any)
	afterEnv := after["env"].(map[string]any)
	for _, field := range []string{"KEEP_ME", "ANTHROPIC_API_KEY"} {
		if !semanticEqual(beforeEnv[field], afterEnv[field]) {
			t.Errorf("unmanaged env field %s changed: %#v", field, afterEnv[field])
		}
	}
	want := map[string]string{
		"ANTHROPIC_BASE_URL":             "https://relay.example.test",
		"ANTHROPIC_AUTH_TOKEN":           claudeCodeCanary,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   defaultClaudeCodeOpus,
		"ANTHROPIC_DEFAULT_SONNET_MODEL": defaultClaudeCodeSonnet,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  defaultClaudeCodeHaiku,
	}
	for name, value := range want {
		if got, ok := afterEnv[name].(string); !ok || got != value {
			t.Errorf("%s = %#v, want %q", name, afterEnv[name], value)
		}
	}
	if strings.Index(string(merged), `"permissions"`) > strings.Index(string(merged), `"env"`) {
		t.Error("top-level member order was needlessly changed")
	}
}

func TestMergeClaudeCodeSettingsRejectsEmptyKey(t *testing.T) {
	_, err := MergeClaudeCodeSettings([]byte(`{}`), "https://relay.example.test", secret.Key{}, ClaudeCodeModels{})
	if err == nil {
		t.Fatal("零值 key 应被拒绝")
	}
}

func TestMergeClaudeCodeSettingsRejectsKeyInUnmanagedValue(t *testing.T) {
	inputs := [][]byte{
		[]byte(`{"env":{"OTHER":"prefix-` + claudeCodeCanary + `-suffix"}}`),
		[]byte(`{"` + claudeCodeCanary + `":"value"}`),
		[]byte(`{"nested":{"duplicate":"` + claudeCodeCanary + `","duplicate":"safe"}}`),
	}
	for _, input := range inputs {
		_, err := MergeClaudeCodeSettings(input, "https://relay.example.test", testClaudeCodeKey(t), ClaudeCodeModels{})
		if err == nil {
			t.Fatalf("非许可位置含所选 key 时应停止: %s", input)
		}
	}
}

func TestMergeClaudeCodeSettingsTreatsEmptyInputAsMissingFile(t *testing.T) {
	merged, err := MergeClaudeCodeSettings(nil, "https://relay.example.test", testClaudeCodeKey(t), ClaudeCodeModels{})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(merged) {
		t.Fatalf("empty input merge produced invalid JSON: %q", merged)
	}
}

func TestMergeClaudeCodeSettingsCanaryOnlyAtAuthTokenValue(t *testing.T) {
	merged, err := MergeClaudeCodeSettings([]byte(`{"env":{"OTHER":"safe"}}`), "https://relay.example.test", testClaudeCodeKey(t), ClaudeCodeModels{})
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(merged), claudeCodeCanary); count != 1 {
		t.Fatalf("canary appears %d times, want 1", count)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(merged, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Env["ANTHROPIC_AUTH_TOKEN"] != claudeCodeCanary {
		t.Error("canary is not the AUTH_TOKEN value")
	}
}

func TestMergeClaudeCodeSettingsUsesCustomModels(t *testing.T) {
	models := ClaudeCodeModels{Opus: "custom-opus", Sonnet: "custom-sonnet", Haiku: "custom-haiku"}
	merged, err := MergeClaudeCodeSettings([]byte(`{}`), "https://relay.example.test", testClaudeCodeKey(t), models)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(merged, &settings); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   models.Opus,
		"ANTHROPIC_DEFAULT_SONNET_MODEL": models.Sonnet,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  models.Haiku,
	} {
		if got := settings.Env[name]; got != want {
			t.Errorf("%s = %q, want custom value %q", name, got, want)
		}
	}
}

func TestMergeClaudeCodeSettingsRejectsInvalidJSON(t *testing.T) {
	_, err := MergeClaudeCodeSettings([]byte(`{"env":`), "https://relay.example.test", testClaudeCodeKey(t), ClaudeCodeModels{})
	if !errors.Is(err, ErrClaudeCodeSettingsInvalid) {
		t.Errorf("invalid JSON error = %v, want stopping condition", err)
	}
}

func TestMergeClaudeCodeSettingsRejectsNonObjectRootsAndEnv(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`[]`),
		[]byte(`{"env":[]}`),
		[]byte(`{"env":null}`),
	} {
		_, err := MergeClaudeCodeSettings(input, "https://relay.example.test", testClaudeCodeKey(t), ClaudeCodeModels{})
		if !errors.Is(err, ErrClaudeCodeSettingsInvalid) {
			t.Errorf("%s: error = %v, want stopping condition", input, err)
		}
	}
}

func TestClaudeCodeSettingsPointUsesContractAnd0600(t *testing.T) {
	point := ClaudeCodeSettingsPoint("/tmp/settings.json")
	if point.Perm != 0o600 {
		t.Errorf("Perm = %#o, want 0600", point.Perm)
	}
	if !semanticEqual(point.Managed, contract.ClaudeCodeManagedEnv) {
		t.Errorf("Managed = %#v, want contract list %#v", point.Managed, contract.ClaudeCodeManagedEnv)
	}
}

func TestGenerateClaudeCodeChangeCarries0600Point(t *testing.T) {
	change, err := GenerateClaudeCodeChange("/tmp/settings.json", []byte(`{}`), "https://relay.example.test", testClaudeCodeKey(t), ClaudeCodeModels{})
	if err != nil {
		t.Fatal(err)
	}
	if change.Point.Perm != 0o600 || change.Point.PathHint != "/tmp/settings.json" {
		t.Errorf("Change point = %#v, want target with 0600", change.Point)
	}
	if !json.Valid(change.Content) {
		t.Errorf("Change content is not JSON: %q", change.Content)
	}
}

func TestClaudeCodeDetectUsesTempHomeAndRejectsInvalidSettings(t *testing.T) {
	home := t.TempDir()
	a := claudeCodeAdapter{
		home:   home,
		lookup: func(string) (string, error) { return "", os.ErrNotExist },
	}
	res, err := a.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "" || res.Detail == "" {
		t.Errorf("missing settings detect = %#v", res)
	}

	target := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"env":{"X":"y"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = a.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != target || !strings.Contains(res.Detail, "已存在") {
		t.Errorf("valid temp settings detect = %#v", res)
	}
	if err := os.WriteFile(target, []byte(`{"env":`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = a.Detect(context.Background())
	if !errors.Is(err, ErrClaudeCodeSettingsInvalid) {
		t.Errorf("invalid temp settings error = %v, want stopping condition", err)
	}
}

func TestClaudeCodeDetectUsesInjectedTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "not-home", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := claudeCodeAdapter{
		target: target,
		lookup: func(string) (string, error) {
			return "", os.ErrNotExist
		},
	}
	res, err := a.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != target || !strings.Contains(res.Detail, "已存在") {
		t.Errorf("injected target detect = %#v", res)
	}
}

func semanticEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
