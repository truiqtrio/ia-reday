package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"relay-install/internal/secret"
)

type wizardScript struct {
	out         strings.Builder
	lines       []string
	lineIndex   int
	keys        []KeyInput
	keyIndex    int
	linePrompts []string
	keyPrompts  []KeyPrompt
}

func (s *wizardScript) callbacks(t *testing.T) WizardCallbacks {
	t.Helper()
	return WizardCallbacks{
		Output: func(text string) error {
			s.out.WriteString(text)
			return nil
		},
		ReadLine: func(_ context.Context, prompt string) (string, error) {
			s.out.WriteString(prompt)
			s.linePrompts = append(s.linePrompts, prompt)
			if s.lineIndex >= len(s.lines) {
				return "", errors.New("script:缺少普通输入")
			}
			answer := s.lines[s.lineIndex]
			s.lineIndex++
			s.out.WriteString("\n")
			return answer, nil
		},
		ReadKey: func(_ context.Context, prompt KeyPrompt) (KeyInput, error) {
			s.out.WriteString(prompt.Prompt)
			s.out.WriteString("\n")
			s.keyPrompts = append(s.keyPrompts, prompt)
			if s.keyIndex >= len(s.keys) {
				return KeyInput{Skip: true}, nil
			}
			entry := s.keys[s.keyIndex]
			s.keyIndex++
			return entry, nil
		},
		ClassifyKey: func(_ context.Context, key secret.Key) (KeyClassification, error) {
			switch key.Ref().Name {
			case "gpt-main":
				return KeyClassification{Group: KeyGroupGPT, Status: StatusConfirmed, Detail: "识别为 GPT 组"}, nil
			case "claude-main":
				return KeyClassification{Group: KeyGroupAnthropic, Status: StatusConfirmed, Detail: "识别为 Anthropic 组"}, nil
			case "china-main":
				return KeyClassification{Group: KeyGroupChina, Status: StatusConfirmed, Detail: "识别为中国大模型组"}, nil
			default:
				return KeyClassification{}, errors.New("probe unavailable")
			}
		},
		BuildPlan: func(_ context.Context, selection WizardResult) (PlanView, error) {
			return PlanView{
				Header:         "relay-install · plan",
				Detect:         []Card{{Status: StatusConfirmed, Name: "codex CLI", Detail: "0.145.0", Path: "/usr/local/bin/codex", Note: "已检测"}},
				WillDo:         []string{"备份并写入配置"},
				WontTouch:      []string{"~/.codex/config.toml"},
				Next:           "授权后执行一次 apply",
				LiveCalls:      len(selection.Clients) * 2,
				CostNotice:     "真实调用可能按量计费",
				BackupLocation: "~/.local/share/relay-install/backups/run-1 (0600)",
			}, nil
		},
	}
}

func mustKey(t *testing.T, name, value string) secret.Key {
	t.Helper()
	key, err := secret.New(name, value)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func defaultWizardOptions(advanced bool) WizardOptions {
	return WizardOptions{
		TTY:            true,
		DefaultInstall: "/opt/relay-install",
		Advanced:       advanced,
		Clients: []ClientChoice{
			{ID: "codex", Name: "codex CLI", Detected: true},
			{ID: "claudecode", Name: "Claude Code", Detected: true},
			{ID: "codexapp", Name: "Codex App", Detected: false},
		},
	}
}

func TestWizardDefaultPathThreeKeysEnterAndAuthorize(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	values := map[KeyGroup]string{
		KeyGroupGPT:       "sk-gpt-AAAA1111",
		KeyGroupAnthropic: "sk-ant-BBBB2222",
		KeyGroupChina:     "sk-cn-CCCC3333",
	}
	script := &wizardScript{
		lines: []string{"", "yes"},
		keys: []KeyInput{
			{Key: mustKey(t, "gpt-main", values[KeyGroupGPT]), AddAnother: true},
			{Key: mustKey(t, "claude-main", values[KeyGroupAnthropic]), AddAnother: true},
			{Key: mustKey(t, "china-main", values[KeyGroupChina])},
		},
	}
	wizard, err := NewWizard(defaultWizardOptions(false), script.callbacks(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := wizard.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Authorized || got.InstallLocation != "/opt/relay-install" || got.Safety != SafetyGuarded {
		t.Fatalf("默认结果错误:%+v", got)
	}
	if fmt.Sprint(got.Clients) != "[claudecode codex]" {
		t.Fatalf("默认应全选已检测客户端:%v", got.Clients)
	}
	if len(got.Keys) != 3 || len(script.keyPrompts) != 3 {
		t.Fatalf("应连续收集三枚 key:keys=%d prompts=%d", len(got.Keys), len(script.keyPrompts))
	}
	if len(script.linePrompts) != 2 {
		t.Fatalf("默认路径除 key 外只应询问安装位置和一次授权,得到 %d 次", len(script.linePrompts))
	}
	output := script.out.String()
	for _, plaintext := range values {
		if strings.Contains(output, plaintext) {
			t.Fatalf("向导输出泄漏明文 key:%s", plaintext)
		}
		if strings.Contains(fmt.Sprintf("%+v", got), plaintext) {
			t.Fatalf("WizardResult 默认格式化泄漏明文 key:%s", plaintext)
		}
	}
	for _, expected := range []string{"gpt-main(…1111", "claude-main(…2222", "china-main(…3333", "语义级真实调用: 4", "备份会复制目标中已有明文密钥"} {
		if !strings.Contains(output, expected) {
			t.Errorf("向导输出缺少 %q\n%s", expected, output)
		}
	}
	if strings.Contains(output, "\x1b[") {
		t.Fatal("NO_COLOR 向导输出不应含 ANSI")
	}
}

func TestWizardUnrestrictedRejectsWrongConfirmation(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	script := oneKeyAdvancedScript(t, []string{"", "", "unrestricted", "UNRESTRICTED"})
	wizard, err := NewWizard(defaultWizardOptions(true), script.callbacks(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = wizard.Run(context.Background())
	if !errors.Is(err, ErrUnrestrictedConfirmation) {
		t.Fatalf("错误确认词应拒绝,得到 %v", err)
	}
	if strings.Count(script.out.String(), "  ⚠ ") != 3 {
		t.Fatalf("unrestricted 必须展开三行风险说明\n%s", script.out.String())
	}
}

func TestWizardUnrestrictedAcceptsExactConfirmation(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	script := oneKeyAdvancedScript(t, []string{"", "", "unrestricted", "unrestricted", "yes"})
	wizard, err := NewWizard(defaultWizardOptions(true), script.callbacks(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := wizard.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Safety != SafetyUnrestricted || !got.Authorized {
		t.Fatalf("应接受逐字确认并完成一次授权:%+v", got)
	}
}

func oneKeyAdvancedScript(t *testing.T, lines []string) *wizardScript {
	t.Helper()
	return &wizardScript{
		lines: lines,
		keys: []KeyInput{
			{Key: mustKey(t, "gpt-main", "sk-gpt-ZZZZ9999")},
		},
	}
}

func TestWizardRejectsNonTTY(t *testing.T) {
	script := &wizardScript{}
	opts := defaultWizardOptions(false)
	opts.TTY = false
	wizard, err := NewWizard(opts, script.callbacks(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = wizard.Run(context.Background())
	if !errors.Is(err, ErrInteractiveTTYRequired) {
		t.Fatalf("非 TTY 缺 flag 应拒绝,得到 %v", err)
	}
}

func TestValidateNonInteractiveSafetyGate(t *testing.T) {
	if mode, err := ValidateNonInteractive(NonInteractiveOptions{}); err != nil || mode != SafetyGuarded {
		t.Fatalf("省略档位应安全地默认 guarded:mode=%s err=%v", mode, err)
	}
	if _, err := ValidateNonInteractive(NonInteractiveOptions{MissingFlags: []string{"--key-stdin"}}); !errors.Is(err, ErrNonInteractiveFlags) {
		t.Fatalf("缺少非交互 flag 应拒绝:%v", err)
	}
	if _, err := ValidateNonInteractive(NonInteractiveOptions{Safety: SafetyUnrestricted}); !errors.Is(err, ErrUnrestrictedFlagRequired) {
		t.Fatalf("unrestricted 缺少显式档位 flag 应拒绝:%v", err)
	}
	mode, err := ValidateNonInteractive(NonInteractiveOptions{Safety: SafetyUnrestricted, SafetyExplicit: true})
	if err != nil || mode != SafetyUnrestricted {
		t.Fatalf("显式 unrestricted flag 应允许:mode=%s err=%v", mode, err)
	}
}
