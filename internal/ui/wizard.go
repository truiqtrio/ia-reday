package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"relay-install/internal/secret"
)

var (
	ErrInteractiveTTYRequired    = errors.New("ui: 交互向导需要 TTY;非交互模式必须提供全部 flag")
	ErrWizardCallbacksIncomplete = errors.New("ui: 向导回调不完整")
	ErrNoKeys                    = errors.New("ui: 至少需要一枚 key")
	ErrNoClients                 = errors.New("ui: 至少选择一个已检测客户端")
	ErrUnrestrictedConfirmation  = errors.New("ui: unrestricted 确认词不匹配")
	ErrAuthorizationDenied       = errors.New("ui: 用户未授权执行")
	ErrIncompletePlanDisclosure  = errors.New("ui: plan 缺少费用或备份留存面说明")
	ErrNonInteractiveFlags       = errors.New("ui: 非交互模式缺少必需 flag")
	ErrUnrestrictedFlagRequired  = errors.New("ui: 非交互 unrestricted 必须显式提供安全档位 flag")
)

// KeyGroup is a subscription grouping slot, not a provider guess. Unknown is
// retained when /v1/models cannot establish a group.
type KeyGroup string

const (
	KeyGroupGPT         KeyGroup = "gpt"
	KeyGroupAnthropic   KeyGroup = "anthropic"
	KeyGroupChina       KeyGroup = "china"
	KeyGroupUnconfirmed KeyGroup = "unconfirmed"
)

func (g KeyGroup) Display(lang Language) string {
	if lang == LanguageEN {
		switch g {
		case KeyGroupGPT:
			return "GPT group"
		case KeyGroupAnthropic:
			return "Anthropic group"
		case KeyGroupChina:
			return "China-model group"
		default:
			return "UNCONFIRMED group"
		}
	}
	switch g {
	case KeyGroupGPT:
		return "GPT 组"
	case KeyGroupAnthropic:
		return "Anthropic 组"
	case KeyGroupChina:
		return "中国大模型组"
	default:
		return "分组 UNCONFIRMED"
	}
}

type BillingKind string

const (
	BillingSubscription BillingKind = "subscription"
	BillingMetered      BillingKind = "metered"
)

func (b BillingKind) Display(lang Language) string {
	if b == BillingMetered {
		if lang == LanguageEN {
			return "metered"
		}
		return "按量"
	}
	if lang == LanguageEN {
		return "subscription"
	}
	return "订阅"
}

type SafetyMode string

const (
	SafetyGuarded      SafetyMode = "guarded"
	SafetyUnrestricted SafetyMode = "unrestricted"
)

// ClientChoice describes a selectable client. Only detected clients are part
// of the default selection.
type ClientChoice struct {
	ID       string
	Name     string
	Detected bool
}

type KeyPrompt struct {
	Sequence int
	Prompt   string
}

// KeyInput is supplied by the secret-owned input callback. AddAnother keeps
// collecting keys; grouping happens only after the injected /v1/models probe.
type KeyInput struct {
	Key        secret.Key
	Billing    BillingKind
	Skip       bool
	AddAnother bool
}

type KeyClassification struct {
	Group  KeyGroup
	Status Status
	Detail string
}

type WizardKey struct {
	Key       secret.Key
	Group     KeyGroup
	Billing   BillingKind
	Status    Status
	Judgement string
}

// WizardCallbacks inject all terminal and orchestration effects. The wizard
// itself never opens a terminal, probes a network, reads a filesystem, or
// starts apply.
type WizardCallbacks struct {
	Output      func(text string) error
	ReadLine    func(ctx context.Context, prompt string) (string, error)
	ReadKey     func(ctx context.Context, prompt KeyPrompt) (KeyInput, error)
	ClassifyKey func(ctx context.Context, key secret.Key) (KeyClassification, error)
	BuildPlan   func(ctx context.Context, selection WizardResult) (PlanView, error)
}

type WizardOptions struct {
	TTY               bool
	Language          Language
	DefaultInstall    string
	Clients           []ClientChoice
	Advanced          bool
	AuthorizationWord string
}

type WizardResult struct {
	Keys            []WizardKey
	InstallLocation string
	Clients         []string
	Safety          SafetyMode
	Plan            PlanView
	Authorized      bool
}

type Wizard struct {
	opts WizardOptions
	cb   WizardCallbacks
	p    *Printer
}

type callbackWriter struct{ output func(string) error }

func (w callbackWriter) Write(data []byte) (int, error) {
	if err := w.output(string(data)); err != nil {
		return 0, err
	}
	return len(data), nil
}

func NewWizard(opts WizardOptions, cb WizardCallbacks) (*Wizard, error) {
	if cb.Output == nil || cb.ReadLine == nil || cb.ReadKey == nil || cb.ClassifyKey == nil || cb.BuildPlan == nil {
		return nil, ErrWizardCallbacksIncomplete
	}
	if opts.Language != LanguageEN {
		opts.Language = LanguageZH
	}
	if opts.AuthorizationWord == "" {
		opts.AuthorizationWord = "yes"
	}
	p := NewPrinterForTerminal(callbackWriter{output: cb.Output}, opts.TTY)
	p.SetLanguage(opts.Language)
	return &Wizard{opts: opts, cb: cb, p: p}, nil
}

// Run collects phase-A inputs and returns only after the single apply
// authorization succeeds. It deliberately does not execute the plan.
func (w *Wizard) Run(ctx context.Context) (WizardResult, error) {
	if !w.opts.TTY {
		return WizardResult{}, ErrInteractiveTTYRequired
	}

	w.p.Line("%s", w.p.text("凭据", "Credentials"))
	if err := w.p.Err(); err != nil {
		return WizardResult{}, err
	}
	keys, err := w.collectKeys(ctx)
	if err != nil {
		return WizardResult{}, err
	}
	if err := w.p.Err(); err != nil {
		return WizardResult{}, err
	}
	if len(keys) == 0 {
		return WizardResult{}, ErrNoKeys
	}

	install, err := w.cb.ReadLine(ctx, fmt.Sprintf(w.p.text("安装位置 [%s],回车接受默认值: ", "Install location [%s], press Enter for default: "), w.opts.DefaultInstall))
	if err != nil {
		return WizardResult{}, err
	}
	install = strings.TrimSpace(install)
	if install == "" {
		install = w.opts.DefaultInstall
	}

	clients, err := w.selectClients(ctx)
	if err != nil {
		return WizardResult{}, err
	}
	mode, err := w.selectSafety(ctx)
	if err != nil {
		return WizardResult{}, err
	}

	result := WizardResult{Keys: keys, InstallLocation: install, Clients: clients, Safety: mode}
	plan, err := w.cb.BuildPlan(ctx, result)
	if err != nil {
		return WizardResult{}, err
	}
	if strings.TrimSpace(plan.CostNotice) == "" || strings.TrimSpace(plan.BackupLocation) == "" {
		return WizardResult{}, ErrIncompletePlanDisclosure
	}
	plan.Keys = make([]PlanKey, 0, len(keys))
	for _, k := range keys {
		plan.Keys = append(plan.Keys, PlanKey{Ref: k.Key.Ref(), Group: k.Group, Billing: k.Billing, Status: k.Status, Judgement: k.Judgement})
	}
	result.Plan = plan
	w.p.Line("")
	w.p.RenderPlan(plan)
	if err := w.p.Err(); err != nil {
		return WizardResult{}, err
	}

	answer, err := w.cb.ReadLine(ctx, fmt.Sprintf(w.p.text("一次授权确认:输入 %q 执行以上 plan: ", "One authorization: type %q to apply this plan: "), w.opts.AuthorizationWord))
	if err != nil {
		return WizardResult{}, err
	}
	if strings.TrimSpace(answer) != w.opts.AuthorizationWord {
		return WizardResult{}, ErrAuthorizationDenied
	}
	result.Authorized = true
	return result, nil
}

func (w *Wizard) collectKeys(ctx context.Context) ([]WizardKey, error) {
	var out []WizardKey
	for sequence := 1; ; sequence++ {
		prompt := w.p.text("输入 key(隐藏输入;录入后自动判定分组): ", "Enter a key (hidden; group is detected automatically): ")
		input, err := w.cb.ReadKey(ctx, KeyPrompt{Sequence: sequence, Prompt: prompt})
		if err != nil {
			return nil, err
		}
		if input.Skip {
			break
		}
		if input.Key.Ref().Len == 0 {
			return nil, ErrNoKeys
		}
		if input.Billing == "" {
			input.Billing = BillingSubscription
		}
		classification, classifyErr := w.cb.ClassifyKey(ctx, input.Key)
		if classifyErr != nil || !confirmedKeyGroup(classification.Group) {
			classification.Group = KeyGroupUnconfirmed
			classification.Status = StatusUnconfirmed
			if classification.Detail == "" {
				classification.Detail = w.p.text("/v1/models 探针失败;不猜分组", "/v1/models probe failed; group not guessed")
			}
		}
		if classification.Status != StatusConfirmed {
			classification.Status = StatusUnconfirmed
		}
		entry := WizardKey{
			Key:       input.Key,
			Group:     classification.Group,
			Billing:   input.Billing,
			Status:    classification.Status,
			Judgement: classification.Detail,
		}
		out = append(out, entry)
		w.p.Line("  %s %s  %s / %s  %s", w.p.Symbol(entry.Status), entry.Key.Ref().Masked(), entry.Group.Display(w.opts.Language), entry.Billing.Display(w.opts.Language), entry.Judgement)
		if err := w.p.Err(); err != nil {
			return nil, err
		}
		if !input.AddAnother {
			break
		}
	}
	return out, nil
}

func confirmedKeyGroup(group KeyGroup) bool {
	switch group {
	case KeyGroupGPT, KeyGroupAnthropic, KeyGroupChina:
		return true
	default:
		return false
	}
}

// NonInteractiveOptions is the UI-level gate used after flag parsing. The
// caller supplies the names of any required flags it found missing.
type NonInteractiveOptions struct {
	MissingFlags   []string
	Safety         SafetyMode
	SafetyExplicit bool
}

// ValidateNonInteractive enforces complete flag input and a separate explicit
// unrestricted opt-in. An omitted safety flag resolves to guarded.
func ValidateNonInteractive(opts NonInteractiveOptions) (SafetyMode, error) {
	if len(opts.MissingFlags) > 0 {
		return "", fmt.Errorf("%w:%s", ErrNonInteractiveFlags, strings.Join(opts.MissingFlags, ","))
	}
	mode := opts.Safety
	if mode == "" {
		mode = SafetyGuarded
	}
	switch mode {
	case SafetyGuarded:
		return mode, nil
	case SafetyUnrestricted:
		if !opts.SafetyExplicit {
			return "", ErrUnrestrictedFlagRequired
		}
		return mode, nil
	default:
		return "", fmt.Errorf("ui:未知档位 %q", mode)
	}
}

func (w *Wizard) selectClients(ctx context.Context) ([]string, error) {
	available := make(map[string]ClientChoice)
	var defaults []string
	for _, client := range w.opts.Clients {
		if !client.Detected {
			continue
		}
		available[client.ID] = client
		defaults = append(defaults, client.ID)
	}
	sort.Strings(defaults)
	if len(defaults) == 0 {
		return nil, ErrNoClients
	}
	if !w.opts.Advanced {
		w.p.Line("%s: %s", w.p.text("客户端(已检测项默认全选)", "Clients (all detected selected)"), strings.Join(defaults, ", "))
		if err := w.p.Err(); err != nil {
			return nil, err
		}
		return defaults, nil
	}
	answer, err := w.cb.ReadLine(ctx, fmt.Sprintf(w.p.text("客户端多选,逗号分隔 [%s]: ", "Select clients, comma separated [%s]: "), strings.Join(defaults, ",")))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(answer) == "" {
		return defaults, nil
	}
	seen := make(map[string]bool)
	var selected []string
	for _, id := range strings.Split(answer, ",") {
		id = strings.TrimSpace(id)
		if _, ok := available[id]; !ok {
			return nil, fmt.Errorf("%w:%s", ErrNoClients, id)
		}
		if !seen[id] {
			selected = append(selected, id)
			seen[id] = true
		}
	}
	if len(selected) == 0 {
		return nil, ErrNoClients
	}
	return selected, nil
}

func (w *Wizard) selectSafety(ctx context.Context) (SafetyMode, error) {
	if !w.opts.Advanced {
		w.p.Line("%s: guarded (workspace-write + on-request)", w.p.text("档位(默认)", "Safety (default)"))
		if err := w.p.Err(); err != nil {
			return "", err
		}
		return SafetyGuarded, nil
	}
	answer, err := w.cb.ReadLine(ctx, w.p.text("档位 [guarded/unrestricted] (guarded): ", "Safety [guarded/unrestricted] (guarded): "))
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" || answer == string(SafetyGuarded) {
		return SafetyGuarded, nil
	}
	if answer != string(SafetyUnrestricted) {
		return "", fmt.Errorf("ui:未知档位 %q", answer)
	}
	w.p.Line("  %s %s", w.p.Symbol(StatusRestartRequired), w.p.text("命令无需逐次批准即可执行。", "Commands can run without per-command approval."))
	w.p.Line("  %s %s", w.p.Symbol(StatusRestartRequired), w.p.text("进程可读写 workspace 之外的文件。", "Processes can read and write outside the workspace."))
	w.p.Line("  %s %s", w.p.Symbol(StatusRestartRequired), w.p.text("错误命令可能造成不可逆的数据或系统变更。", "A mistaken command can cause irreversible data or system changes."))
	if err := w.p.Err(); err != nil {
		return "", err
	}
	confirm, err := w.cb.ReadLine(ctx, w.p.text("逐字输入 unrestricted 继续: ", "Type unrestricted exactly to continue: "))
	if err != nil {
		return "", err
	}
	if confirm != "unrestricted" {
		return "", ErrUnrestrictedConfirmation
	}
	return SafetyUnrestricted, nil
}

var _ io.Writer = callbackWriter{}
