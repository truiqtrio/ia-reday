package ui

import (
	"bytes"
	"strings"
	"testing"
)

// 全部状态都必须有符号映射,且只用规定的 7 个符号
func TestStatusGlyphsCovered(t *testing.T) {
	allowed := map[string]bool{"●": true, "◐": true, "○": true, "✕": true, "↻": true, "⚠": true, "→": true}
	statuses := []Status{
		StatusConfirmed, StatusNoOp, StatusRestartRequired, StatusReadyForImport,
		StatusUnconfirmed, StatusBlocked, StatusFailed,
		StatusManualRecoveryRequired, StatusRolledBack,
	}
	for _, s := range statuses {
		g := Glyph(s)
		if g == "" {
			t.Errorf("状态 %s 缺少符号映射", s)
			continue
		}
		if !allowed[g] {
			t.Errorf("状态 %s 使用了规定之外的符号 %q", s, g)
		}
	}
}

// 硬规则:UNCONFIRMED 系(非 OK 全态)禁止绿色/对勾,不得与 OK 同色系
func TestUnconfirmedNotGreen(t *testing.T) {
	for _, s := range []Status{
		StatusUnconfirmed, StatusReadyForImport, StatusRestartRequired,
		StatusBlocked, StatusFailed, StatusManualRecoveryRequired,
	} {
		st := statusStyles[s]
		if st.color == colorGreen {
			t.Errorf("状态 %s 误用绿色(仅 CONFIRMED 可绿)", s)
		}
		if st.glyph == "✓" {
			t.Errorf("状态 %s 误用对勾", s)
		}
	}
	if statusStyles[StatusConfirmed].color != colorGreen {
		t.Error("CONFIRMED 应为绿色 ●")
	}
}

// 非 TTY(管道/文件)降级:输出纯符号,无 ANSI 转义
func TestNonTTYDegrades(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf) // 非 *os.File → 必降级
	if p.ColorEnabled() {
		t.Fatal("非 TTY 不应启用颜色")
	}
	p.RenderCard(Card{Status: StatusConfirmed, Name: "codex CLI", Detail: "0.145.0", Path: "/x", Note: "n"})
	if strings.Contains(buf.String(), "\x1b[") {
		t.Error("降级输出不应含 ANSI 转义")
	}
	if !strings.Contains(buf.String(), "●") {
		t.Error("降级输出仍应含状态符号")
	}
}

// NO_COLOR 优先于一切(同包内直接验证判定逻辑)
func TestNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(nil) {
		t.Error("NO_COLOR 置位时必须禁用颜色")
	}
}

func TestPlainTextStatusMarksAreUnambiguous(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf)
	statuses := []Status{
		StatusConfirmed, StatusNoOp, StatusRestartRequired, StatusReadyForImport,
		StatusUnconfirmed, StatusBlocked, StatusFailed,
		StatusManualRecoveryRequired, StatusRolledBack,
	}
	seen := make(map[string]Status)
	for _, status := range statuses {
		mark := p.Mark(status)
		if prior, ok := seen[mark]; ok {
			t.Fatalf("纯文本状态 %s 与 %s 不可区分:%q", prior, status, mark)
		}
		seen[mark] = status
	}
}
