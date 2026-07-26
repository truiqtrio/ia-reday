package adapters

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relay-install/internal/contract"
	"relay-install/internal/secret"
)

func testCodexConfig(t *testing.T, alias string, profile CodexSafetyProfile) CodexConfig {
	t.Helper()
	key, err := secret.New(alias, "sk-canary-"+alias+"-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	return CodexConfig{
		Alias: alias, BaseURL: "https://backend.intelalloc.example", Model: "gpt-5.6-luna",
		ReviewModel: "gpt-5.6-sol", ModelReasoningEffort: "high", PlanModeReasoningEffort: "xhigh",
		SafetyProfile: profile, Key: key,
	}
}

func TestGenerateCodexConfigGolden(t *testing.T) {
	cases := []struct {
		alias   string
		profile CodexSafetyProfile
	}{
		{"alpha", ""},
		{"bravo", CodexProfileGuarded},
		{"charlie", CodexProfileUnrestricted},
	}
	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			got, err := GenerateCodexConfig(testCodexConfig(t, tc.alias, tc.profile))
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "codex", tc.alias+".config.toml")
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("生成结果与 golden 不同\nwant:\n%s\ngot:\n%s", want, got)
			}
			if !IsManagedCodexConfig(got) {
				t.Fatal("生成配置缺少管理标记")
			}
			for _, forbidden := range []string{"disable_response_storage", "network_access", "windows_wsl_setup_acknowledged"} {
				if strings.Contains(string(got), forbidden) {
					t.Fatalf("生成配置包含黑名单字段 %q", forbidden)
				}
			}
		})
	}
}

func TestPlanCodexConfigConflictAndManagedReplacement(t *testing.T) {
	home := t.TempDir()
	a := codexAdapter{codexHome: home}
	cfg := testCodexConfig(t, "alpha", CodexProfileGuarded)
	path, err := CodexConfigPath(home, cfg.Alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model = \"foreign\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PlanCodexConfig(cfg); !errors.Is(err, ErrCodexConfigConflict) {
		t.Fatalf("无管理标记应冲突，得到 %v", err)
	}
	managed, err := GenerateCodexConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, managed, 0o600); err != nil {
		t.Fatal(err)
	}
	change, err := a.PlanCodexConfig(cfg)
	if err != nil {
		t.Fatalf("已有受管配置应可替换: %v", err)
	}
	if change.Point.PathHint != path {
		t.Errorf("目标路径 = %q, want %q", change.Point.PathHint, path)
	}
}

func TestPlanCodexConfigRejectsBrokenManagedTOML(t *testing.T) {
	home := t.TempDir()
	a := codexAdapter{codexHome: home}
	cfg := testCodexConfig(t, "alpha", CodexProfileGuarded)
	path, err := CodexConfigPath(home, cfg.Alias)
	if err != nil {
		t.Fatal(err)
	}
	broken := []byte(contract.CodexManagedHeader + `model = "unterminated` + "\n")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PlanCodexConfig(cfg); !errors.Is(err, ErrCodexConfigInvalid) {
		t.Fatalf("损坏的受管 TOML 应停止，得到 %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(broken) {
		t.Fatal("冲突检测修改了损坏的目标文件")
	}
}

func TestCodexWritePointUsesContractSchema(t *testing.T) {
	point := codexWritePoint("/tmp/config.toml")
	if !equalStrings(point.Managed, contract.CodexManagedFields) {
		t.Fatalf("Managed = %#v, want contract schema %#v", point.Managed, contract.CodexManagedFields)
	}
}

func TestCodexConfigPathRejectsTraversal(t *testing.T) {
	if _, err := CodexConfigPath(t.TempDir(), "../outside"); err == nil {
		t.Fatal("alias traversal 应被拒绝")
	}
}

func TestGenerateCodexConfigRejectsUnknownProfileAndEmptyKey(t *testing.T) {
	cfg := testCodexConfig(t, "alpha", CodexSafetyProfile("other"))
	if _, err := GenerateCodexConfig(cfg); err == nil {
		t.Fatal("未知 profile 应被拒绝")
	}
	cfg = testCodexConfig(t, "alpha", CodexProfileGuarded)
	cfg.Key = secret.Key{}
	if _, err := GenerateCodexConfig(cfg); err == nil {
		t.Fatal("零值 key 应被拒绝")
	}
}

func TestValidateCodexConfigRejectsBlacklist(t *testing.T) {
	content, err := GenerateCodexConfig(testCodexConfig(t, "alpha", CodexProfileGuarded))
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, []byte("network_access = true\n")...)
	if err := ValidateCodexConfig(content); err == nil {
		t.Fatal("黑名单字段应被拒绝")
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
