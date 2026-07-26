// relay-install 入口:仅注册子命令与全局 flag,业务逻辑全在 internal 包。
// 命令集为候选裁定:plan / apply / verify / recover / keys;不带子命令等价 plan(只读)。
// 输出默认纯文本(owner 指示不做符号美化):强制走 ui 的非 TTY/plain 路径。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"relay-install/internal/contract"
	"relay-install/internal/orchestrator"
	"relay-install/internal/ui"
)

// globalFlags 全局 flag 面(候选裁定),透传为 orchestrator.Options
type globalFlags struct {
	baseURL   string // IntelAlloc base URL:HTTPS 根地址,无 query/fragment/userinfo
	profile   string // 指定 profile(如 Codex profile 名)
	keyStdin  bool   // 从 stdin 读密钥;非交互唯一通道,禁止 argv/env 传明文
	skipLive  bool   // 关闭语义级真实调用(烧钱操作必须可关)
	printOnly bool   // 降级模式:只输出可手贴片段,不写入
	lang      string // 界面语言 zh/en,技术名词不翻译

	// 模型覆盖(可选扩展项;默认值见 orchestrator.Default*,owner 裁定 #7)
	codexModel        string
	codexReviewModel  string
	claudeOpusModel   string
	claudeSonnetModel string
	claudeHaikuModel  string
}

func (gf *globalFlags) options() orchestrator.Options {
	return orchestrator.Options{
		BaseURL:           gf.baseURL,
		Profile:           gf.profile,
		KeyStdin:          gf.keyStdin,
		SkipLive:          gf.skipLive,
		PrintOnly:         gf.printOnly,
		Lang:              gf.lang,
		CodexModel:        gf.codexModel,
		CodexReviewModel:  gf.codexReviewModel,
		ClaudeOpusModel:   gf.claudeOpusModel,
		ClaudeSonnetModel: gf.claudeSonnetModel,
		ClaudeHaikuModel:  gf.claudeHaikuModel,
	}
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	gf := &globalFlags{}
	root := &cobra.Command{
		Use:   "relay-install",
		Short: "IntelAlloc 中转配置安装器",
		// 不带子命令等价 plan(UI 规格 2.1)
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlan(cmd, gf)
		},
	}
	root.PersistentFlags().StringVar(&gf.baseURL, "base-url", orchestrator.DefaultBaseURL, "IntelAlloc base URL(HTTPS 根地址)")
	root.PersistentFlags().StringVar(&gf.profile, "profile", "", "指定 profile(默认 default)")
	root.PersistentFlags().BoolVar(&gf.keyStdin, "key-stdin", false, "从 stdin 读取密钥(每行 \"分组:key\" 或单枚自动判定)")
	root.PersistentFlags().BoolVar(&gf.skipLive, "skip-live", false, "跳过语义级真实调用")
	root.PersistentFlags().BoolVar(&gf.printOnly, "print-only", false, "只输出可手贴片段,不写入")
	root.PersistentFlags().StringVar(&gf.lang, "lang", "zh", "界面语言 (zh|en)")
	root.PersistentFlags().StringVar(&gf.codexModel, "codex-model", orchestrator.DefaultCodexModel, "codex 默认 model")
	root.PersistentFlags().StringVar(&gf.codexReviewModel, "codex-review-model", orchestrator.DefaultCodexReviewModel, "codex review_model")
	root.PersistentFlags().StringVar(&gf.claudeOpusModel, "claude-opus-model", orchestrator.DefaultClaudeOpusModel, "claude 默认 opus 模型")
	root.PersistentFlags().StringVar(&gf.claudeSonnetModel, "claude-sonnet-model", orchestrator.DefaultClaudeSonnetModel, "claude sonnet 模型(可选扩展项)")
	root.PersistentFlags().StringVar(&gf.claudeHaikuModel, "claude-haiku-model", orchestrator.DefaultClaudeHaikuModel, "claude haiku 模型(可选扩展项)")

	root.AddCommand(&cobra.Command{
		Use:   "plan",
		Short: "只读预览:检测 / 将执行 / 不会触碰 / 下一步",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlan(cmd, gf)
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "apply",
		Short: "两阶段执行:先凭据与分组,后逐端 txn 写入 + 探针裁决",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApply(cmd, gf)
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "verify",
		Short: "逐端重跑探针,输出七态(凭据只经 --key-stdin)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd, gf)
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "recover",
		Short: "按 txn journal 收尾崩溃残留",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecover(cmd, gf)
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "keys",
		Short: "列出本次会话已命名 key(名称+末4位+分组+计费性质)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKeys(cmd, gf)
		},
	})
	return root
}

// newEngine 以命令的 stdin/stdout 接线生产依赖;输出强制 plain(非 TTY)。
func newEngine(cmd *cobra.Command) *orchestrator.Engine {
	stdinTTY := false
	if f, ok := cmd.InOrStdin().(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			stdinTTY = true
		}
	}
	return orchestrator.New(orchestrator.Deps{
		Stdin:      cmd.InOrStdin(),
		StdinIsTTY: stdinTTY,
		TTY:        os.Stdin,
		Output:     cmd.OutOrStdout(),
	})
}

// newPlainPrinter 纯文本输出器:强制非 TTY,无 ANSI、无 spinner(owner 指示)。
func newPlainPrinter(cmd *cobra.Command, lang string) *ui.Printer {
	p := ui.NewPrinterForTerminal(cmd.OutOrStdout(), false)
	if lang == string(ui.LanguageEN) {
		p.SetLanguage(ui.LanguageEN)
	}
	return p
}

func runPlan(cmd *cobra.Command, gf *globalFlags) error {
	engine := newEngine(cmd)
	p := newPlainPrinter(cmd, gf.lang)
	view := ui.PlanView{
		Header:     fmt.Sprintf("relay-install · plan  %s", nowString()),
		CostNotice: "语义级真实调用可能产生费用,以提供方计费为准;--skip-live 可关闭",
		LiveCalls:  2,
	}
	if gf.skipLive {
		view.LiveCalls = 0
	}
	if dir, err := os.UserConfigDir(); err == nil {
		view.BackupLocation = dir + string(os.PathSeparator) + "relay-install" + string(os.PathSeparator) + "backups"
	}
	err := engine.Plan(context.Background(), gf.options(), func(ev orchestrator.Event) {
		switch ev.Kind {
		case orchestrator.EventDetectDone:
			view.Detect = append(view.Detect, ui.Card{
				Status: ev.Status, Name: string(ev.Client), Detail: ev.Message, Path: ev.Path,
			})
		case orchestrator.EventPlanWillDo:
			view.WillDo = append(view.WillDo, ev.Message)
		case orchestrator.EventPlanWontTouch:
			view.WontTouch = append(view.WontTouch, ev.Message)
		case orchestrator.EventPlanNext:
			view.Next = ev.Message
		}
	})
	if err != nil {
		p.Line("%s plan 失败: %v", p.Mark(ui.StatusFailed), err)
		return err
	}
	p.RenderPlan(view)
	return p.Err()
}

func runApply(cmd *cobra.Command, gf *globalFlags) error {
	engine := newEngine(cmd)
	p := newPlainPrinter(cmd, gf.lang)
	finals := make(map[contract.ClientID]ui.Status)
	var order []contract.ClientID
	rolledBack := ""
	err := engine.Apply(context.Background(), gf.options(), func(ev orchestrator.Event) {
		switch ev.Kind {
		case orchestrator.EventStepStart:
			p.Line("  %s: %s", ev.Client, ev.Message)
		case orchestrator.EventStepDone:
			if ev.Client != "" {
				p.Line("  %s: %s", ev.Client, ev.Message)
			} else {
				p.Line("  %s", ev.Message)
			}
		case orchestrator.EventStepSkipped:
			p.Line("  %s %s: %s", p.Mark(ui.StatusUnconfirmed), ev.Client, ev.Message)
		case orchestrator.EventRollback:
			if rolledBack == "" {
				rolledBack = ev.Message
			}
			p.Line("  %s %s: %s", p.Mark(ev.Status), ev.Client, ev.Message)
		case orchestrator.EventFinal:
			if _, seen := finals[ev.Client]; !seen {
				order = append(order, ev.Client)
			}
			finals[ev.Client] = ev.Status
			p.Line("  %s %s: %s", p.Mark(ev.Status), ev.Client, ev.Message)
		}
	})
	if err == nil {
		err = p.Err()
	}
	renderApplyBanner(p, finals, order, rolledBack)
	if p.Err() != nil && err == nil {
		err = p.Err()
	}
	return err
}

// renderApplyBanner 结尾横幅永远只有三种之一(完成 / 部分未验证 / 已回滚)。
func renderApplyBanner(p *ui.Printer, finals map[contract.ClientID]ui.Status, order []contract.ClientID, rolledBack string) {
	if rolledBack != "" {
		_ = p.RenderBanner(ui.Banner{Kind: ui.BannerRolledBack, Detail: rolledBack})
		return
	}
	configured := 0
	var unconfirmed []string
	for _, id := range order {
		switch finals[id] {
		case ui.StatusConfirmed, ui.StatusNoOp, ui.StatusReadyForImport:
			configured++
		default:
			unconfirmed = append(unconfirmed, string(id)+"="+string(finals[id]))
		}
	}
	if len(order) > 0 && configured == len(order) {
		_ = p.RenderBanner(ui.Banner{Kind: ui.BannerComplete, Configured: configured, Total: len(order)})
		return
	}
	detail := "无客户端被配置"
	if len(unconfirmed) > 0 {
		detail = "未证实: " + joinComma(unconfirmed)
	}
	_ = p.RenderBanner(ui.Banner{Kind: ui.BannerPartial, Configured: configured, Total: len(order), Detail: detail})
}

func runVerify(cmd *cobra.Command, gf *globalFlags) error {
	engine := newEngine(cmd)
	p := newPlainPrinter(cmd, gf.lang)
	err := engine.Verify(context.Background(), gf.options(), func(ev orchestrator.Event) {
		if ev.Kind == orchestrator.EventFinal {
			p.RenderCard(ui.Card{Status: ev.Status, Name: string(ev.Client), Detail: ev.Message, Path: ev.Path})
			return
		}
		if ev.Message != "" {
			p.Line("  %s", ev.Message)
		}
	})
	if err != nil {
		p.Line("%s verify 失败: %v", p.Mark(ui.StatusFailed), err)
		return err
	}
	return p.Err()
}

func runRecover(cmd *cobra.Command, gf *globalFlags) error {
	engine := newEngine(cmd)
	p := newPlainPrinter(cmd, gf.lang)
	err := engine.Recover(context.Background(), func(ev orchestrator.Event) {
		if ev.Kind == orchestrator.EventFinal {
			p.Line("  %s %s: %s", p.Mark(ev.Status), ev.Client, ev.Message)
			return
		}
		p.Line("  %s", ev.Message)
	})
	if err != nil {
		p.Line("%s recover 失败: %v", p.Mark(ui.StatusFailed), err)
		return err
	}
	return p.Err()
}

func runKeys(cmd *cobra.Command, gf *globalFlags) error {
	engine := newEngine(cmd)
	p := newPlainPrinter(cmd, gf.lang)
	err := engine.ListKeys(context.Background(), gf.options(), func(ev orchestrator.Event) {
		if ev.Kind == orchestrator.EventFinal {
			p.Line("  %s %s", p.Mark(ev.Status), ev.Message)
			return
		}
		p.Line("  %s", ev.Message)
	})
	if err != nil {
		p.Line("%s keys 失败: %v", p.Mark(ui.StatusFailed), err)
		return err
	}
	return p.Err()
}

func joinComma(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ", "
		}
		out += item
	}
	return out
}

func nowString() string {
	return time.Now().Format("2006-01-02 15:04")
}
