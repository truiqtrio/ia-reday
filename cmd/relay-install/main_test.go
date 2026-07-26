package main

import (
	"bytes"
	"strings"
	"testing"
)

func execute(args ...string) (string, error) {
	cmd := newRootCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// TestHelpSmoke 各子命令 help 可达,全局 flag 在帮助中可见。
func TestHelpSmoke(t *testing.T) {
	out, err := execute("--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	for _, name := range []string{"plan", "apply", "verify", "recover", "keys"} {
		if !strings.Contains(out, name) {
			t.Errorf("root help 缺少子命令 %q", name)
		}
	}
	for _, flag := range []string{"--base-url", "--key-stdin", "--skip-live", "--print-only", "--lang", "--profile"} {
		if !strings.Contains(out, flag) {
			t.Errorf("root help 缺少全局 flag %q", flag)
		}
	}
	for _, name := range []string{"plan", "apply", "verify", "recover", "keys"} {
		if _, err := execute(name, "--help"); err != nil {
			t.Errorf("%s --help: %v", name, err)
		}
	}
}

// TestDefaultFlags 默认值落地:base-url 与模型默认值(owner 裁定 #7)。
func TestDefaultFlags(t *testing.T) {
	out, err := execute("--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	for _, want := range []string{
		"https://backend.intelalloc.com",
		"gpt-5.6-sol-high",
		"gpt-5.6-sol",
		"claude-opus-5",
		"claude-sonnet-5[1M]",
		"claude-fable-5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help 默认值缺少 %q", want)
		}
	}
}

// TestApplyRequiresKeyChannel 非交互无 --key-stdin:apply 必须拒绝,不得进入执行。
func TestApplyRequiresKeyChannel(t *testing.T) {
	_, err := execute("apply", "--skip-live")
	if err == nil {
		t.Fatal("非交互无 --key-stdin 应报错")
	}
	if !strings.Contains(err.Error(), "key-stdin") {
		t.Errorf("错误应提示 --key-stdin: %v", err)
	}
}

// TestUnknownFlagRejected flag 解析冒烟:未知 flag 报错。
func TestUnknownFlagRejected(t *testing.T) {
	if _, err := execute("plan", "--no-such-flag"); err == nil {
		t.Fatal("未知 flag 应报错")
	}
}
