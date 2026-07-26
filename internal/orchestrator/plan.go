package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"

	"relay-install/internal/adapters"
	"relay-install/internal/contract"
	"relay-install/internal/secret"
	"relay-install/internal/ui"
)

// Plan 纯只读:不建锁、不访问网络、不执行任何写入;首尾各取一次目标哈希,
// 变化即报 STALE。输出经事件驱动四节:检测(EventDetectDone)/ 将执行
// (EventPlanWillDo)/ 不会触碰(EventPlanWontTouch)/ 下一步(EventPlanNext)。
func (e *Engine) Plan(ctx context.Context, opts Options, emit func(Event)) error {
	opts = opts.withDefaults()
	targets, err := e.planTargetPaths(opts)
	if err != nil {
		return err
	}
	before, err := hashTargets(targets)
	if err != nil {
		return err
	}

	// ---- 检测节:遍历 DetectTargets;单端错误降级为卡片,不中断遍历 ----
	detected := make(map[contract.ClientID]adapters.DetectResult, len(e.deps.Adapters))
	detectErrs := make(map[contract.ClientID]error, len(e.deps.Adapters))
	for _, a := range e.deps.Adapters {
		res, derr := a.Detect(ctx)
		detected[a.ID()] = res
		if derr != nil {
			detectErrs[a.ID()] = derr
		}
		status, msg := detectStatus(res, derr)
		emit(Event{Kind: EventDetectDone, Client: a.ID(), Status: status, Message: msg, Path: res.Path})
	}

	// ---- 各适配器 Plan(占位 secret.Ref;plan 阶段不需要真实 key)----
	// 适配器 Plan 尚未实现的端回落到契约合成条目(ErrNotImplemented 不算失败)。
	placeholder := secret.Ref{Name: "(apply 时提供)"}
	notes := make(map[contract.ClientID][]string)
	for _, a := range e.deps.Adapters {
		plan, perr := a.Plan(ctx, adapters.PlanRequest{BaseURL: opts.BaseURL, Profile: opts.Profile, Key: placeholder})
		switch {
		case errors.Is(perr, adapters.ErrNotImplemented):
			// 骨架端:用契约写入点合成,见 willDoItems。
		case perr != nil:
			notes[a.ID()] = append(notes[a.ID()], "plan 生成失败: "+perr.Error())
		default:
			notes[a.ID()] = append(notes[a.ID()], plan.Notes...)
		}
	}

	// ---- 将执行 / 不会触碰 / 下一步 ----
	for _, item := range e.willDoItems(opts, detected, detectErrs, notes) {
		emit(Event{Kind: EventPlanWillDo, Message: item})
	}
	for _, item := range e.wontTouchItems(detected) {
		emit(Event{Kind: EventPlanWontTouch, Message: item})
	}
	emit(Event{Kind: EventPlanNext, Message: nextGuidance(detectErrs)})

	after, err := hashTargets(targets)
	if err != nil {
		return err
	}
	if after != before {
		return ErrStale
	}
	return nil
}

// planTargetPaths STALE 监视的写入目标(codex profile / settings.json / ccswitch DB)。
func (e *Engine) planTargetPaths(opts Options) ([]string, error) {
	var targets []string
	if e.adapterByID(contract.ClientCodex) != nil {
		home, err := e.codexHome()
		if err != nil {
			return nil, err
		}
		path, err := adapters.CodexConfigPath(home, codexAlias(opts))
		if err != nil {
			return nil, err
		}
		targets = append(targets, path)
	}
	if e.adapterByID(contract.ClientClaudeCode) != nil {
		path, err := e.claudeCodeSettingsPath()
		if err != nil {
			return nil, err
		}
		targets = append(targets, path)
	}
	if e.adapterByID(contract.ClientCCSwitch) != nil {
		path, err := e.ccswitchDBPath()
		if err != nil {
			return nil, err
		}
		targets = append(targets, path)
	}
	return targets, nil
}

func codexAlias(opts Options) string {
	if opts.Profile != "" {
		return opts.Profile
	}
	return "default"
}

// hashTargets 目标聚合哈希:路径 + 存在性 + 内容哈希;只读。
func hashTargets(targets []string) (string, error) {
	h := sha256.New()
	for _, target := range targets {
		_, _ = io.WriteString(h, target)
		data, err := os.ReadFile(target)
		if errors.Is(err, os.ErrNotExist) {
			_, _ = h.Write([]byte{0})
			continue
		}
		if err != nil {
			return "", err
		}
		_, _ = h.Write([]byte{1})
		sum := sha256.Sum256(data)
		_, _ = h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// willDoItems "将执行(apply 时)"条目:备份先行,逐端一条,仅覆盖可写端。
func (e *Engine) willDoItems(opts Options, detected map[contract.ClientID]adapters.DetectResult, detectErrs map[contract.ClientID]error, notes map[contract.ClientID][]string) []string {
	var items []string
	items = append(items, fmt.Sprintf("备份目标文件 → %s(0600 留存;备份会复制目标中已有明文 key)", backupRootDisplay()))

	if res, ok := detected[contract.ClientCodex]; ok && res.Installed && detectErrs[contract.ClientCodex] == nil {
		home, err := e.codexHome()
		if err == nil {
			path, err := adapters.CodexConfigPath(home, codexAlias(opts))
			if err == nil {
				items = append(items, fmt.Sprintf("写入 %s(受管 profile %q,0600;model=%s,guarded 档位)", path, codexAlias(opts), opts.CodexModel))
			}
		}
	}
	if res, ok := detected[contract.ClientClaudeCode]; ok && (res.Installed || res.Path != "") {
		path, err := e.claudeCodeSettingsPath()
		if err == nil {
			items = append(items, fmt.Sprintf("合并 %s 的 env 块(仅 5 个受管变量,0600;opus=%s)", path, opts.ClaudeOpusModel))
		}
	}
	if res, ok := detected[contract.ClientCCSwitch]; ok && res.Installed && detectErrs[contract.ClientCCSwitch] == nil {
		items = append(items, "ccswitch:SQL 导入 provider 条目(仅 providers/provider_endpoints;GPT/Anthropic/中国组各成独立条目)")
	}
	items = append(items, fmt.Sprintf("语义级真实调用(可用 --skip-live 关闭):codex→/v1/responses(%s),claude→/v1/messages(%s)", opts.CodexModel, opts.ClaudeOpusModel))

	for _, id := range sortedClientIDs(notes) {
		for _, note := range notes[id] {
			items = append(items, fmt.Sprintf("%s: %s", id, note))
		}
	}
	return items
}

// wontTouchItems "不会触碰"条目:主配置、零写入表、未安装端、平台裁剪端。
func (e *Engine) wontTouchItems(detected map[contract.ClientID]adapters.DetectResult) []string {
	var items []string
	if home, err := e.codexHome(); err == nil {
		items = append(items, home+"/config.toml(codex 主配置逐字不动;只写 intelalloc-<alias>.config.toml)")
	}
	items = append(items, "ccswitch proxy_* / model_pricing 表(零写入,导入前后哈希校验)")
	var missing []string
	for id, res := range detected {
		if !res.Installed {
			missing = append(missing, string(id))
		}
	}
	sort.Strings(missing)
	for _, id := range missing {
		items = append(items, fmt.Sprintf("%s(未安装,不创建、不触碰)", id))
	}
	if runtime.GOOS == "linux" {
		items = append(items, "Claude Desktop / Codex App(Linux 不在 v1 范围,不检测不显示)")
	}
	return items
}

func nextGuidance(detectErrs map[contract.ClientID]error) string {
	next := "确认无误后执行:relay-install apply(TTY 交互收集 key;非交互用 --key-stdin)"
	for _, err := range detectErrs {
		if errors.Is(err, adapters.ErrCCSwitchTakeover) {
			return "检测节存在 BLOCKED(代理接管痕迹);先解除接管再执行 relay-install apply"
		}
	}
	return next
}

func sortedClientIDs(notes map[contract.ClientID][]string) []contract.ClientID {
	ids := make([]contract.ClientID, 0, len(notes))
	for id := range notes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// buildPlanView 向导预览用的四节视图(与 plan 事件同一合成来源)。
func (e *Engine) buildPlanView(opts Options, detected map[contract.ClientID]adapters.DetectResult, detectErrs map[contract.ClientID]error) ui.PlanView {
	cards := make([]ui.Card, 0, len(e.deps.Adapters))
	for _, a := range e.deps.Adapters {
		res := detected[a.ID()]
		status, msg := detectStatus(res, detectErrs[a.ID()])
		cards = append(cards, ui.Card{Status: status, Name: string(a.ID()), Detail: msg, Path: res.Path})
	}
	return ui.PlanView{
		Header:         fmt.Sprintf("relay-install · apply 预览  %s", e.deps.Now().Format("2006-01-02 15:04")),
		Detect:         cards,
		WillDo:         e.willDoItems(opts, detected, detectErrs, nil),
		WontTouch:      e.wontTouchItems(detected),
		Next:           nextGuidance(detectErrs),
		LiveCalls:      liveCallCount(opts),
		CostNotice:     "语义级真实调用可能产生费用,以提供方计费为准;--skip-live 可关闭",
		BackupLocation: backupRootDisplay(),
	}
}

func liveCallCount(opts Options) int {
	if opts.SkipLive {
		return 0
	}
	return 2
}
