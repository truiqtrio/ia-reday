package ui

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type ProgressState string

const (
	ProgressPending        ProgressState = "pending"
	ProgressRunning        ProgressState = "running"
	ProgressSucceeded      ProgressState = "succeeded"
	ProgressFailed         ProgressState = "failed"
	ProgressSkipped        ProgressState = "skipped"
	ProgressRolledBack     ProgressState = "rolled_back"
	ProgressRollbackFailed ProgressState = "rollback_failed"
)

type ProgressStep struct {
	State ProgressState
	Text  string
}

type ProgressView struct {
	Title string
	Steps []ProgressStep
}

func progressGlyph(state ProgressState) string {
	switch state {
	case ProgressSucceeded:
		return "✓"
	case ProgressFailed, ProgressRollbackFailed:
		return "✕"
	case ProgressSkipped:
		return "—"
	case ProgressRolledBack:
		return "↻"
	case ProgressRunning:
		return "⠋"
	default:
		return "○"
	}
}

func (p *Printer) progressSymbol(state ProgressState) string {
	glyph := progressGlyph(state)
	switch state {
	case ProgressSucceeded:
		return p.style(glyph, colorGreen)
	case ProgressFailed, ProgressRollbackFailed:
		return p.style(glyph, colorRed)
	case ProgressRolledBack:
		return p.style(glyph, colorMagenta)
	case ProgressRunning:
		return p.style(glyph, colorCyan)
	default:
		return p.style(glyph, colorGray)
	}
}

// RenderProgress renders a stable snapshot. Completed history stays in input
// order and only a running item receives a spinner frame.
func (p *Printer) RenderProgress(v ProgressView) {
	title := v.Title
	if title == "" {
		title = p.text("执行中", "Applying")
	}
	p.Line("%s", title)
	runningSeen := false
	failedSeen := false
	for _, step := range v.Steps {
		state := step.State
		if failedSeen && state != ProgressRolledBack && state != ProgressRollbackFailed {
			state = ProgressSkipped
		}
		if state == ProgressRunning {
			if runningSeen {
				state = ProgressPending
			} else {
				runningSeen = true
			}
		}
		p.Line("  %s %s", p.progressSymbol(state), step.Text)
		if state == ProgressFailed || state == ProgressRollbackFailed {
			failedSeen = true
		}
	}
}

// Spinner owns only the current terminal row. Callers drive Tick from their
// own clock; no goroutine or process is started by the UI package.
type Spinner struct {
	p       *Printer
	frames  []string
	frame   int
	current string
	active  bool
}

func NewSpinner(p *Printer) *Spinner {
	return &Spinner{p: p, frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}}
}

func (s *Spinner) Start(text string) error {
	if s.active {
		return errors.New("ui: spinner 已有当前行")
	}
	if err := s.p.Err(); err != nil {
		return err
	}
	s.current, s.active, s.frame = text, true, 0
	if s.p.Terminal() && s.p.ColorEnabled() {
		s.p.write("  %s %s", s.p.style(s.frames[0], colorCyan), text)
		if err := s.p.Err(); err != nil {
			s.active = false
			return err
		}
	}
	return nil
}

func (s *Spinner) Tick() {
	if !s.active || !s.p.Terminal() || !s.p.ColorEnabled() {
		return
	}
	s.frame = (s.frame + 1) % len(s.frames)
	s.p.write("\r\x1b[2K  %s %s", s.p.style(s.frames[s.frame], colorCyan), s.current)
}

func (s *Spinner) Finish(state ProgressState, text string) error {
	if !s.active {
		return errors.New("ui: spinner 没有当前行")
	}
	if err := s.p.Err(); err != nil {
		return err
	}
	if state == ProgressRunning || state == ProgressPending {
		return errors.New("ui: spinner 只能结束为终态")
	}
	if text == "" {
		text = s.current
	}
	if s.p.Terminal() && s.p.ColorEnabled() {
		s.p.write("\r\x1b[2K")
	}
	s.p.Line("  %s %s", s.p.progressSymbol(state), text)
	s.active = false
	return s.p.Err()
}

type BannerKind string

const (
	BannerComplete   BannerKind = "complete"
	BannerPartial    BannerKind = "partial"
	BannerRolledBack BannerKind = "rolled_back"
)

type Banner struct {
	Kind       BannerKind
	Configured int
	Total      int
	Detail     string
}

// RenderBanner emits exactly one of the three allowed terminal outcomes.
func (p *Printer) RenderBanner(b Banner) error {
	switch b.Kind {
	case BannerComplete:
		p.Line("%s %s  %d/%d %s", p.Symbol(StatusConfirmed), p.text("完成", "Complete"), b.Configured, b.Total, p.text("客户端已配置并验证", "clients configured and verified"))
	case BannerPartial:
		if strings.TrimSpace(b.Detail) == "" {
			return errors.New("ui:部分未验证横幅缺少 UNCONFIRMED 说明")
		}
		p.Line("%s %s  %d/%d %s;%s", p.Symbol(StatusUnconfirmed), p.text("完成(部分未验证)", "Complete (partially unverified)"), b.Configured, b.Total, p.text("已配置", "configured"), b.Detail)
	case BannerRolledBack:
		if strings.TrimSpace(b.Detail) == "" {
			return errors.New("ui:已回滚横幅缺少净变更与原因说明")
		}
		p.Line("%s %s  %s", p.Symbol(StatusRolledBack), p.text("已回滚", "Rolled back"), b.Detail)
	default:
		return fmt.Errorf("ui:未知结尾横幅 %q", b.Kind)
	}
	return p.Err()
}

// RenderManualRecoveryRequired is deliberately separate from the three normal
// completion banners: an incomplete rollback must never be labeled rolled back.
func (p *Printer) RenderManualRecoveryRequired(reason, backupPath string) {
	p.Line("%s MANUAL_RECOVERY_REQUIRED  %s;%s:%s", p.Symbol(StatusManualRecoveryRequired), reason, p.text("保留现场", "retained evidence"), backupPath)
}

type HealthCheck struct {
	Status   Status
	Protocol string
	Endpoint string
	Model    string
	Latency  time.Duration
	Detail   string
}

type HealthView struct {
	Responses HealthCheck
	Messages  HealthCheck
}

// RenderHealth always emits the two protocol rows. Anything other than a
// semantic CONFIRMED result is visibly UNCONFIRMED and never green or checked.
func (p *Printer) RenderHealth(v HealthView) {
	p.Line("%s", p.text("健康检查", "Health check"))
	checks := []HealthCheck{v.Responses, v.Messages}
	if checks[0].Protocol == "" {
		checks[0].Protocol = "Responses"
	}
	if checks[0].Endpoint == "" {
		checks[0].Endpoint = "/v1/responses"
	}
	if checks[1].Protocol == "" {
		checks[1].Protocol = "Messages"
	}
	if checks[1].Endpoint == "" {
		checks[1].Endpoint = "/v1/messages"
	}
	hasUnconfirmedMessages := false
	for _, check := range checks {
		status := check.Status
		model := check.Model
		if status != StatusConfirmed {
			status = StatusUnconfirmed
			model = "UNCONFIRMED"
			if strings.EqualFold(check.Protocol, "Messages") {
				hasUnconfirmedMessages = true
			}
		}
		latency := ""
		if check.Latency > 0 {
			latency = fmt.Sprintf("%dms", check.Latency.Milliseconds())
		}
		p.Line("  %s %-11s %-15s %-18s %-7s %s", p.Symbol(status), check.Protocol, check.Endpoint, model, latency, check.Detail)
	}
	if hasUnconfirmedMessages {
		p.Line("%s %s", p.guide(), p.text("Messages 未证实不影响已生效配置;Claude 系客户端使用前请确认中转支持。", "Unconfirmed Messages does not undo applied configuration; confirm relay support before using Claude clients."))
	}
}

type BackoffErrorCard struct {
	StatusCode int
	What       string
	Why        string
	Action     string
	RequestID  string
	RetryAfter string // raw header value; never parsed for display
	StatusURL  string
}

// RenderBackoffErrorCard renders the required three-part error card for
// retryable HTTP failures. The Retry-After value is reproduced verbatim.
func (p *Printer) RenderBackoffErrorCard(card BackoffErrorCard) error {
	if card.StatusCode != 429 && card.StatusCode != 502 && card.StatusCode != 503 {
		return fmt.Errorf("ui:不支持的退避错误状态 %d", card.StatusCode)
	}
	if strings.TrimSpace(card.What) == "" || strings.TrimSpace(card.Why) == "" {
		return errors.New("ui:退避错误卡缺少发生了什么或为什么")
	}
	if strings.Contains(card.Action, "换 key 重试") {
		return errors.New("ui:退避建议禁止要求换 key 重试")
	}
	action := card.Action
	if strings.TrimSpace(action) == "" {
		action = p.text("按 Retry-After 等待,随后指数退避重试;不要轮换 key。", "Wait for Retry-After, then retry with exponential backoff; do not rotate keys.")
	}
	p.Line("%s HTTP %d · UNCONFIRMED", p.Symbol(StatusUnconfirmed), card.StatusCode)
	p.Line("  %s:%s", p.text("发生了什么", "What happened"), card.What)
	p.Line("  %s:%s", p.text("为什么", "Why"), card.Why)
	p.Line("  %s:%s", p.text("你可以怎么办", "What you can do"), action)
	p.Line("  Request ID:%s", valueOrUnknown(card.RequestID, p.text("未提供", "not provided")))
	p.Line("  Retry-After:%s", valueOrUnknown(card.RetryAfter, p.text("未提供", "not provided")))
	if card.StatusURL != "" {
		p.Line("  %s:%s", p.text("提供方状态页", "Provider status"), card.StatusURL)
	}
	return p.Err()
}

func valueOrUnknown(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
