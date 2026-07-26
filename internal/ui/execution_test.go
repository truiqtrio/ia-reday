package ui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProgressAndBannersGolden(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf)
	p.RenderProgress(ProgressView{Steps: []ProgressStep{
		{State: ProgressSucceeded, Text: "备份完成(2 个文件,sha256 已记录)"},
		{State: ProgressSucceeded, Text: "codex/intelalloc.config.toml 暂存 → strict-config 通过 → 生效"},
		{State: ProgressRunning, Text: "claude/settings.json 合并中…"},
		{State: ProgressSkipped, Text: "ccswitch provider 跳过"},
	}})
	p.Line("")
	p.RenderBanner(Banner{Kind: BannerComplete, Configured: 3, Total: 3})
	p.RenderBanner(Banner{Kind: BannerPartial, Configured: 2, Total: 3, Detail: "health:Messages 端点 UNCONFIRMED(见下)"})
	p.RenderBanner(Banner{Kind: BannerRolledBack, Detail: "未做任何净变更;原因:strict-config 拒绝(第 2 步)"})
	assertGolden(t, "progress_banners.golden", buf.String())
}

func TestSpinnerReturnsAndPreservesFirstOutputError(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	want := errors.New("broken output")
	p := NewPrinterForTerminal(errorWriter{err: want}, true)
	spinner := NewSpinner(p)
	if err := spinner.Start("working"); err != nil {
		t.Fatal(err)
	}
	if err := spinner.Finish(ProgressSucceeded, "done"); !errors.Is(err, want) {
		t.Fatalf("Finish 应返回输出错误,得到 %v", err)
	}
	p.Line("later output must not erase the first error")
	if !errors.Is(p.Err(), want) {
		t.Fatalf("Printer 必须保留第一次输出错误,得到 %v", p.Err())
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestHealthAndBackoffCard(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf)
	p.RenderHealth(HealthView{
		Responses: HealthCheck{Status: StatusConfirmed, Model: "gpt-5.6-luna", Latency: 312 * time.Millisecond, Detail: "响应有效"},
		Messages:  HealthCheck{Status: StatusUnconfirmed, Detail: "端点返回 404"},
	})
	if err := p.RenderBackoffErrorCard(BackoffErrorCard{
		StatusCode: 503,
		What:       "提供方暂时不可用",
		Why:        "上游返回 503",
		RequestID:  "req-123",
		RetryAfter: "Wed, 25 Jul 2026 10:00:00 GMT",
		StatusURL:  "https://status.example.test",
	}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, expected := range []string{"● Responses", "◐ Messages", "UNCONFIRMED", "Request ID:req-123", "Retry-After:Wed, 25 Jul 2026 10:00:00 GMT", "指数退避"} {
		if !strings.Contains(got, expected) {
			t.Errorf("输出缺少 %q\n%s", expected, got)
		}
	}
	if strings.Contains(got, "✓ Messages") || strings.Contains(got, "换 key 重试") {
		t.Fatalf("UNCONFIRMED 或退避文案违反硬规则\n%s", got)
	}
}

func TestBackoffCardsAcceptOnlySpecifiedStatuses(t *testing.T) {
	for _, status := range []int{429, 502, 503} {
		var buf bytes.Buffer
		if err := NewPrinter(&buf).RenderBackoffErrorCard(BackoffErrorCard{StatusCode: status, What: "暂时不可用", Why: "上游限流"}); err != nil {
			t.Errorf("HTTP %d 应支持:%v", status, err)
		}
	}
	var buf bytes.Buffer
	if err := NewPrinter(&buf).RenderBackoffErrorCard(BackoffErrorCard{StatusCode: 500}); err == nil {
		t.Error("非 429/502/503 不应使用退避错误卡")
	}
}

func TestProgressSkipsOrdinaryStepsAfterFailure(t *testing.T) {
	var buf bytes.Buffer
	NewPrinter(&buf).RenderProgress(ProgressView{Steps: []ProgressStep{
		{State: ProgressFailed, Text: "写入失败"},
		{State: ProgressRunning, Text: "本不应继续"},
		{State: ProgressRollbackFailed, Text: "恢复 before-image 失败"},
	}})
	NewPrinter(&buf).RenderManualRecoveryRequired("before-image 恢复失败", "/backups/run-1")
	if !strings.Contains(buf.String(), "— 本不应继续") || !strings.Contains(buf.String(), "✕ 恢复 before-image 失败") || !strings.Contains(buf.String(), "MANUAL_RECOVERY_REQUIRED") {
		t.Fatalf("失败后的普通步骤必须跳过,回滚失败必须诚实显示:%s", buf.String())
	}
}

func TestNoColorDisablesSpinnerANSI(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	p := NewPrinterForTerminal(&buf, true)
	spinner := NewSpinner(p)
	if err := spinner.Start("working"); err != nil {
		t.Fatal(err)
	}
	spinner.Tick()
	if err := spinner.Finish(ProgressSucceeded, "done"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Fatalf("NO_COLOR spinner 不得含 ANSI:%q", buf.String())
	}
	if buf.String() != "  ✓ done\n" {
		t.Fatalf("NO_COLOR 仍需输出等价终态:%q", buf.String())
	}
}

func TestColoredBannersKeepPartialDistinctFromComplete(t *testing.T) {
	old, had := os.LookupEnv("NO_COLOR")
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("NO_COLOR", old)
		} else {
			_ = os.Unsetenv("NO_COLOR")
		}
	})
	var buf bytes.Buffer
	p := NewPrinterForTerminal(&buf, true)
	p.RenderBanner(Banner{Kind: BannerComplete, Configured: 1, Total: 1})
	p.RenderBanner(Banner{Kind: BannerPartial, Configured: 1, Total: 2, Detail: "UNCONFIRMED"})
	if !strings.Contains(buf.String(), "\x1b[32m●") || !strings.Contains(buf.String(), "\x1b[33m◐") {
		t.Fatalf("完成与部分未验证横幅必须使用不同色系:%q", buf.String())
	}
}

func TestBannerRejectsUnknownOrIncompleteOutcome(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf)
	if err := p.RenderBanner(Banner{Kind: "unexpected"}); err == nil {
		t.Fatal("未知横幅不得静默输出")
	}
	if err := p.RenderBanner(Banner{Kind: BannerPartial}); err == nil {
		t.Fatal("部分未验证横幅必须说明 UNCONFIRMED 原因")
	}
	if err := p.RenderBanner(Banner{Kind: BannerRolledBack}); err == nil {
		t.Fatal("已回滚横幅必须说明净变更与原因")
	}
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("golden %s 不匹配\n--- want ---\n%s--- got ---\n%s", name, string(want), got)
	}
}
