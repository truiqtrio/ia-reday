// Package ui 全部输出的唯一出口:业务逻辑不直接 print(硬约束)。
// 视觉规格落地:七态符号、16 色 ANSI、NO_COLOR/非 TTY 降级、三行卡片、plan 四节。
package ui

import (
	"fmt"
	"io"
	"os"
)

// Language controls UI copy. Technical names such as TOML, profile, and
// provider are deliberately left untranslated.
type Language string

const (
	LanguageZH Language = "zh"
	LanguageEN Language = "en"
)

// Status 逐端报告状态:七态 + MANUAL_RECOVERY_REQUIRED / ROLLED_BACK
type Status string

const (
	StatusConfirmed              Status = "CONFIRMED"
	StatusNoOp                   Status = "NO-OP"
	StatusRestartRequired        Status = "RESTART_REQUIRED"
	StatusReadyForImport         Status = "READY_FOR_IMPORT"
	StatusUnconfirmed            Status = "UNCONFIRMED"
	StatusBlocked                Status = "BLOCKED"
	StatusFailed                 Status = "FAILED"
	StatusManualRecoveryRequired Status = "MANUAL_RECOVERY_REQUIRED"
	StatusRolledBack             Status = "ROLLED_BACK"
)

// color 16 色 ANSI 安全色
type color string

const (
	colorGreen   color = "32"
	colorYellow  color = "33"
	colorRed     color = "31"
	colorBlue    color = "34"
	colorGray    color = "90"
	colorMagenta color = "35"
	colorCyan    color = "36"
)

// statusStyle 状态符号映射。全工具只用这 7 个符号:● ◐ ○ ✕ ↻ ⚠ →。
// 硬规则:UNCONFIRMED 系(非 OK 全态)禁止绿色/对勾,与 OK 视觉严格区分;
// 七态 + MANUAL_RECOVERY_REQUIRED 互不可混淆(符号+颜色组合区分)。
type statusStyle struct {
	glyph string
	color color
}

var statusStyles = map[Status]statusStyle{
	StatusConfirmed:              {"●", colorGreen},   // 已配置且验证通过
	StatusNoOp:                   {"●", colorGray},    // 已配置且一致,无需动作
	StatusRestartRequired:        {"⚠", colorYellow},  // 需完全退出并重启
	StatusReadyForImport:         {"◐", colorBlue},    // 已写待导入,实测前不算确认
	StatusUnconfirmed:            {"◐", colorYellow},  // 诚实态,非失败态
	StatusBlocked:                {"⚠", colorRed},     // takeover 等阻断
	StatusFailed:                 {"✕", colorRed},     // 确定性失败
	StatusManualRecoveryRequired: {"⚠", colorMagenta}, // 回滚不完整,保留现场
	StatusRolledBack:             {"↻", colorMagenta}, // 已回滚
}

// glyphGuide 指引符号(青),用于"下一步"行
const glyphGuide = "→"

// Glyph 状态符号(无色)
func Glyph(s Status) string { return statusStyles[s].glyph }

// Printer 输出器:NO_COLOR 与非 TTY 自动降级为纯文本(信息等价,非残版)
type Printer struct {
	w     io.Writer
	color bool
	tty   bool
	lang  Language
	err   error
}

// NewPrinter 构造输出器,颜色自动判定
func NewPrinter(w io.Writer) *Printer {
	tty := isTerminal(w)
	return &Printer{w: w, color: colorEnabledForTTY(tty), tty: tty, lang: LanguageZH}
}

// NewPrinterForTerminal constructs a printer around an injected output. The
// caller supplies terminal capability so callback-backed and scripted TTYs do
// not need to be *os.File. NO_COLOR still takes precedence.
func NewPrinterForTerminal(w io.Writer, tty bool) *Printer {
	return &Printer{w: w, color: colorEnabledForTTY(tty), tty: tty, lang: LanguageZH}
}

// SetLanguage changes UI copy. Unknown values fall back to Chinese.
func (p *Printer) SetLanguage(lang Language) {
	if lang == LanguageEN {
		p.lang = LanguageEN
		return
	}
	p.lang = LanguageZH
}

// colorEnabled NO_COLOR 优先;非字符设备(管道/文件)一律无色
func colorEnabled(w io.Writer) bool {
	return colorEnabledForTTY(isTerminal(w))
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func colorEnabledForTTY(tty bool) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return tty
}

// ColorEnabled 当前是否输出颜色
func (p *Printer) ColorEnabled() bool { return p.color }

// Terminal reports whether cursor-local rendering such as a spinner is safe.
func (p *Printer) Terminal() bool { return p.tty }

// Err returns the first output error observed by the printer.
func (p *Printer) Err() error { return p.err }

// Symbol 带色状态符号;降级时返回纯符号
func (p *Printer) Symbol(s Status) string {
	st := statusStyles[s]
	return p.style(st.glyph, st.color)
}

func (p *Printer) style(value string, c color) string {
	if !p.color {
		return value
	}
	return "\x1b[" + string(c) + "m" + value + "\x1b[0m"
}

func (p *Printer) guide() string { return p.style(glyphGuide, colorCyan) }

// Mark includes a textual state label, keeping otherwise similar states such
// as READY_FOR_IMPORT and UNCONFIRMED unambiguous after plain-text fallback.
func (p *Printer) Mark(s Status) string {
	return p.Symbol(s) + " " + string(s)
}

// Line 输出一行
func (p *Printer) Line(format string, args ...any) {
	p.write(format+"\n", args...)
}

func (p *Printer) write(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

func (p *Printer) text(zh, en string) string {
	if p.lang == LanguageEN {
		return en
	}
	return zh
}
