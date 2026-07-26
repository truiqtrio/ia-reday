package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"relay-install/internal/adapters"
	"relay-install/internal/contract"
	"relay-install/internal/probe"
	"relay-install/internal/secret"
	"relay-install/internal/txn"
	"relay-install/internal/ui"
)

// applyOrder 阶段 B 固定执行顺序;codexapp/claudedesktop 无受管写入点,不在其中。
var applyOrder = []contract.ClientID{
	contract.ClientCodex,
	contract.ClientClaudeCode,
	contract.ClientCCSwitch,
}

// Apply 两阶段:阶段 A 收集凭据(TTY wizard / --key-stdin)并分组绑定;阶段 B
// 逐客户端走 txn(备份→暂存→三层校验→提交),失败按 txn 语义回滚,后续端跳过。
// codex 端 BLOCKED(自动安装失败)不算整体失败;确定性写入失败返回错误。
func (e *Engine) Apply(ctx context.Context, opts Options, emit func(Event)) error {
	opts = opts.withDefaults()

	detected, detectErrs := e.detectAll(ctx)

	// ---- 阶段 A:凭据与分组(权限只问一次,由 wizard 收口;stdin 无需确认)----
	keys, err := e.collectKeys(ctx, opts, detected, emit)
	if err != nil {
		return err
	}
	binding := bindKeys(keys)
	if binding.gpt == nil && binding.anthropic == nil && len(binding.china) == 0 {
		return errNoConfirmedKeys
	}

	// ---- Codex 保底:授权后、写入前;CLI 缺失时优先 npm 自动安装,
	// 否则人工指引 + codex 端 BLOCKED(不算整体失败,其余端继续)----
	codexBlocked := e.ensureCodexCLI(ctx, detected, detectErrs, emit)

	// ---- 阶段 B:逐端执行 ----
	finals := make(map[contract.ClientID]ui.Status, len(applyOrder))
	hardFailed := false
	var firstErr error
	for _, id := range applyOrder {
		adapter := e.adapterByID(id)
		if adapter == nil {
			continue
		}
		if hardFailed {
			finals[id] = ui.StatusUnconfirmed
			emit(Event{Kind: EventStepSkipped, Client: id, Status: ui.StatusUnconfirmed,
				Message: "前端失败,本端跳过"})
			continue
		}
		var status ui.Status
		var ferr error
		switch id {
		case contract.ClientCodex:
			status, ferr = e.applyCodex(ctx, opts, adapter, binding.gpt, codexBlocked, detected, detectErrs, emit)
		case contract.ClientClaudeCode:
			status, ferr = e.applyClaudeCode(ctx, opts, adapter, binding.anthropic, emit)
		case contract.ClientCCSwitch:
			status, ferr = e.applyCCSwitch(ctx, opts, adapter, binding, detected, detectErrs, emit)
		}
		finals[id] = status
		if ferr != nil {
			hardFailed = true
			firstErr = ferr
		}
	}

	// ---- 语义探针裁决(--skip-live 关闭)----
	if !opts.SkipLive && !hardFailed {
		e.runFinalProbes(ctx, opts, binding, finals, emit)
	}

	if firstErr != nil {
		return firstErr
	}
	return nil
}

// ensureCodexCLI codex 保底:缺失且有 npm → 自动安装并复查;无 npm → 人工指引,
// codex 端标 BLOCKED,继续其他端。返回 codex 是否 BLOCKED。
func (e *Engine) ensureCodexCLI(ctx context.Context, detected map[contract.ClientID]adapters.DetectResult, detectErrs map[contract.ClientID]error, emit func(Event)) bool {
	res, ok := detected[contract.ClientCodex]
	if !ok {
		return false
	}
	if err := detectErrs[contract.ClientCodex]; err != nil {
		_, msg := detectStatus(res, err)
		emit(Event{Kind: EventFinal, Client: contract.ClientCodex, Status: ui.StatusBlocked, Message: msg})
		return true
	}
	if res.Installed {
		return false
	}
	if _, err := e.deps.LookPath("npm"); err != nil {
		emit(Event{Kind: EventFinal, Client: contract.ClientCodex, Status: ui.StatusBlocked,
			Message: "codex CLI 缺失且未找到 npm;请手动执行 npm i -g @openai/codex 后重试(其余端继续)"})
		return true
	}
	emit(Event{Kind: EventStepStart, Client: contract.ClientCodex,
		Message: "codex CLI 缺失;执行 npm i -g @openai/codex"})
	if _, err := e.deps.RunCommand(ctx, "npm", "i", "-g", "@openai/codex"); err != nil {
		emit(Event{Kind: EventFinal, Client: contract.ClientCodex, Status: ui.StatusBlocked,
			Message: "codex 自动安装失败;请手动执行 npm i -g @openai/codex 后重试(其余端继续)"})
		return true
	}
	// 复查:重新 Detect,版本门槛(≥0.134)由适配器把关。
	adapter := e.adapterByID(contract.ClientCodex)
	if adapter == nil {
		return true
	}
	res, err := adapter.Detect(ctx)
	if err != nil || !res.Installed {
		emit(Event{Kind: EventFinal, Client: contract.ClientCodex, Status: ui.StatusBlocked,
			Message: "codex 安装后复查未通过;请手动确认安装(其余端继续)"})
		return true
	}
	detected[contract.ClientCodex] = res
	emit(Event{Kind: EventStepDone, Client: contract.ClientCodex, Status: ui.StatusNoOp,
		Message: "codex CLI 安装完成 " + res.Version})
	return false
}

// applyCodex 写入受管 profile;GPT 组 key 唯一来源。返回(最终状态, 硬失败错误)。
func (e *Engine) applyCodex(ctx context.Context, opts Options, adapter adapters.Adapter, gpt *BoundKey, blocked bool, detected map[contract.ClientID]adapters.DetectResult, detectErrs map[contract.ClientID]error, emit func(Event)) (ui.Status, error) {
	id := contract.ClientCodex
	if blocked {
		// BLOCKED 事件已在 ensureCodexCLI 发出;不算整体失败。
		return ui.StatusBlocked, nil
	}
	if gpt == nil {
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
			Message: "无 GPT 组 key,codex 端跳过(GPT 组只配 codex 侧)"})
		return ui.StatusUnconfirmed, nil
	}
	home, err := e.codexHome()
	if err != nil {
		return ui.StatusFailed, err
	}
	path, err := adapters.CodexConfigPath(home, codexAlias(opts))
	if err != nil {
		return ui.StatusFailed, err
	}
	existing, existed, err := readFileIfExists(path)
	if err != nil {
		return ui.StatusFailed, err
	}
	if existed && !adapters.IsManagedCodexConfig(existing) {
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusBlocked,
			Message: "目标 profile 非 relay-install 管理,拒绝覆盖: " + path})
		return ui.StatusBlocked, nil
	}
	content, err := adapters.GenerateCodexConfig(adapters.CodexConfig{
		Alias:                   codexAlias(opts),
		BaseURL:                 opts.BaseURL,
		Model:                   opts.CodexModel,
		ReviewModel:             opts.CodexReviewModel,
		ModelReasoningEffort:    DefaultCodexEffort,
		PlanModeReasoningEffort: DefaultCodexPlanEffort,
		SafetyProfile:           adapters.CodexProfileGuarded,
		Key:                     gpt.Key,
	})
	if err != nil {
		return ui.StatusFailed, sanitizeKey(gpt.Key, err)
	}
	change := adapters.Change{
		Point: contract.WritePoint{
			Client: id, Kind: contract.ParserTOML, PathHint: path, Perm: 0o600,
			Managed:   append([]string(nil), contract.CodexManagedFields...),
			Blacklist: append([]string(nil), contract.CodexBlacklist...),
		},
		Content: content,
		Secret:  gpt.Key,
		AllowedSecretPaths: [][]string{{
			contract.CodexFieldModelProviders,
			contract.CodexProviderIntelalloc,
			contract.CodexProviderFieldBearerToken,
		}},
	}
	rememberBeforeImage(&change, existing, existed)

	if opts.PrintOnly {
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
			Message: "print-only 未写入;可手贴片段(key 已脱敏):\n" + redactContent(content, gpt.Key)})
		return ui.StatusUnconfirmed, nil
	}

	emit(Event{Kind: EventStepStart, Client: id,
		Message: fmt.Sprintf("备份 → 暂存 → 三层校验 → 提交 %s(key %s)", path, gpt.Key.Ref().Masked())})
	result, err := adapter.Apply(ctx, adapters.ChangeSet{Client: id, Changes: []adapters.Change{change}})
	if err != nil {
		return e.applyFailed(id, result, err, emit)
	}
	if result.Noop {
		emit(Event{Kind: EventStepDone, Client: id, Status: ui.StatusNoOp,
			Message: "配置已一致,无需变更(NO-OP)"})
		return ui.StatusNoOp, nil
	}
	emit(Event{Kind: EventStepDone, Client: id, Status: ui.StatusRestartRequired,
		Message: "已写入并通过校验(strict-config 层);尚未做语义验证"})
	return ui.StatusUnconfirmed, nil
}

// applyClaudeCode merge 式写 settings.json env 块;Anthropic 组 key 唯一来源。
func (e *Engine) applyClaudeCode(ctx context.Context, opts Options, adapter adapters.Adapter, anthropic *BoundKey, emit func(Event)) (ui.Status, error) {
	id := contract.ClientClaudeCode
	if anthropic == nil {
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
			Message: "无 Anthropic 组 key,claude code 端跳过"})
		return ui.StatusUnconfirmed, nil
	}
	path, err := e.claudeCodeSettingsPath()
	if err != nil {
		return ui.StatusFailed, err
	}
	existing, existed, err := readFileIfExists(path)
	if err != nil {
		return ui.StatusFailed, err
	}
	change, err := adapters.GenerateClaudeCodeChange(path, existing, opts.BaseURL, anthropic.Key, adapters.ClaudeCodeModels{
		Opus:   opts.ClaudeOpusModel,
		Sonnet: opts.ClaudeSonnetModel,
		Haiku:  opts.ClaudeHaikuModel,
	})
	if err != nil {
		return ui.StatusFailed, sanitizeKey(anthropic.Key, err)
	}
	if !existed {
		// GenerateClaudeCodeChange 以 nil existing 视为新建;显式记录 before-image。
		rememberBeforeImage(&change, nil, false)
	}

	if opts.PrintOnly {
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
			Message: "print-only 未写入;可手贴片段(key 已脱敏):\n" + redactContent(change.Content, anthropic.Key)})
		return ui.StatusUnconfirmed, nil
	}

	emit(Event{Kind: EventStepStart, Client: id,
		Message: fmt.Sprintf("备份 → 暂存 → 校验 → 合并 %s(key %s)", path, anthropic.Key.Ref().Masked())})
	result, err := adapter.Apply(ctx, adapters.ChangeSet{Client: id, Changes: []adapters.Change{change}})
	if err != nil {
		return e.applyFailed(id, result, err, emit)
	}
	if result.Noop {
		emit(Event{Kind: EventStepDone, Client: id, Status: ui.StatusNoOp,
			Message: "配置已一致,无需变更(NO-OP)"})
		return ui.StatusNoOp, nil
	}
	emit(Event{Kind: EventStepDone, Client: id, Status: ui.StatusRestartRequired,
		Message: "已合并并通过校验;Claude Code 需完全退出并重启后生效"})
	return ui.StatusUnconfirmed, nil
}

// applyCCSwitch 结构化 SQL 导入:每组 key 成独立 provider 条目
// (GPT→codex,Anthropic→claude/claude-desktop,中国组→独立 claude 条目)。
func (e *Engine) applyCCSwitch(ctx context.Context, opts Options, adapter adapters.Adapter, binding keyBinding, detected map[contract.ClientID]adapters.DetectResult, detectErrs map[contract.ClientID]error, emit func(Event)) (ui.Status, error) {
	id := contract.ClientCCSwitch
	res := detected[id]
	if err := detectErrs[id]; err != nil {
		status, msg := detectStatus(res, err)
		emit(Event{Kind: EventFinal, Client: id, Status: status, Message: msg})
		return status, nil // BLOCKED / UNCONFIRMED 均不算整体失败
	}
	if !res.Installed {
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
			Message: "未发现 live DB,ccswitch 端跳过"})
		return ui.StatusUnconfirmed, nil
	}
	providers := e.buildCCSwitchProviders(opts, binding)
	if len(providers) == 0 {
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
			Message: "无已确认分组的 key,ccswitch 端跳过"})
		return ui.StatusUnconfirmed, nil
	}
	if opts.PrintOnly {
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
			Message: fmt.Sprintf("print-only 未导入;%d 条 provider 待导入(SQL 含密钥,不提供手贴片段)", len(providers))})
		return ui.StatusUnconfirmed, nil
	}
	emit(Event{Kind: EventStepStart, Client: id,
		Message: fmt.Sprintf("备份 → 版本门 → SQL 导入 %d 条 provider(proxy_*/model_pricing 零写入)", len(providers))})
	result, err := adapter.Apply(ctx, adapters.ChangeSet{
		Client:   id,
		CCSwitch: &adapters.CCSwitchChange{Providers: providers},
	})
	if err != nil {
		if errors.Is(err, adapters.ErrCCSwitchTemplateNotFound) || errors.Is(err, adapters.ErrCCSwitchTemplateInvalid) {
			emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
				Message: "未找到可安全克隆的原版 IntelAlloc 配置;数据库未写入。请先在 CC Switch GUI 导入原版配置,再重试"})
			return ui.StatusUnconfirmed, nil
		}
		return e.applyFailed(id, result, err, emit)
	}
	if result.Noop {
		emit(Event{Kind: EventStepDone, Client: id, Status: ui.StatusNoOp,
			Message: "provider 已一致,无需变更(NO-OP)"})
		return ui.StatusNoOp, nil
	}
	// SQL 导入后由 CC Switch 自身接管调用;语义级探针在适配器侧仍是 TODO,
	// 因此只声明 READY_FOR_IMPORT(已写待导入),实测前不算确认。
	emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusReadyForImport,
		Message: "已导入并 readback 校验;重启 CC Switch 后生效,实测前不算确认"})
	return ui.StatusReadyForImport, nil
}

// buildCCSwitchProviders 按组构造独立 provider 条目;IsCurrent 每 app_type 一条。
func (e *Engine) buildCCSwitchProviders(opts Options, binding keyBinding) []adapters.CCSwitchProvider {
	var providers []adapters.CCSwitchProvider
	now := e.deps.Now().UnixMilli()
	sortIndex := 90
	next := func() int { sortIndex++; return sortIndex - 1 }
	if binding.gpt != nil {
		providers = append(providers, adapters.CCSwitchProvider{
			ID: "relay-intelalloc-codex", AppType: contract.CCSwitchAppCodex,
			Name: "IntelAlloc (codex)", BaseURL: opts.BaseURL, Model: opts.CodexModel,
			Key: binding.gpt.Key, SortIndex: next(), CreatedAt: now, IsCurrent: true,
		})
	}
	if binding.anthropic != nil {
		providers = append(providers,
			adapters.CCSwitchProvider{
				ID: "relay-intelalloc-claude", AppType: contract.CCSwitchAppClaude,
				Name: "IntelAlloc (claude)", BaseURL: opts.BaseURL,
				Key: binding.anthropic.Key, SortIndex: next(), CreatedAt: now, IsCurrent: true,
			},
			adapters.CCSwitchProvider{
				ID: "relay-intelalloc-claude-desktop", AppType: contract.CCSwitchAppClaudeDesktop,
				Name: "IntelAlloc (claude-desktop)", BaseURL: opts.BaseURL,
				Key: binding.anthropic.Key, SortIndex: next(), CreatedAt: now, IsCurrent: true,
			},
		)
	}
	for i, china := range binding.china {
		providers = append(providers, adapters.CCSwitchProvider{
			ID: fmt.Sprintf("relay-intelalloc-china-%d", i+1), AppType: contract.CCSwitchAppClaude,
			Name: fmt.Sprintf("IntelAlloc 中国大模型 %d", i+1), BaseURL: opts.BaseURL,
			Key: china.Key, SortIndex: next(), CreatedAt: now, IsCurrent: false,
		})
	}
	return providers
}

// applyFailed 失败统一收口:txn 已按语义回滚,发回滚事件并标 FAILED。
func (e *Engine) applyFailed(id contract.ClientID, result txn.Result, err error, emit func(Event)) (ui.Status, error) {
	if errors.Is(err, txn.ErrManualRecoveryRequired) {
		emit(Event{Kind: EventRollback, Client: id, Status: ui.StatusManualRecoveryRequired,
			Message: "回滚不完整,保留现场: " + result.BackupDir})
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusManualRecoveryRequired,
			Message: err.Error()})
		return ui.StatusManualRecoveryRequired, err
	}
	emit(Event{Kind: EventRollback, Client: id, Status: ui.StatusRolledBack,
		Message: "已按 txn 语义回滚;原因: " + err.Error()})
	emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusFailed, Message: err.Error()})
	return ui.StatusFailed, err
}

// runFinalProbes 语义级真实调用裁决七态;只对已成功写入且持 key 的端。
func (e *Engine) runFinalProbes(ctx context.Context, opts Options, binding keyBinding, finals map[contract.ClientID]ui.Status, emit func(Event)) {
	type probeTarget struct {
		id       contract.ClientID
		key      *BoundKey
		protocol probe.Protocol
		model    string
	}
	var targets []probeTarget
	if finals[contract.ClientCodex] == ui.StatusUnconfirmed && binding.gpt != nil {
		targets = append(targets, probeTarget{contract.ClientCodex, binding.gpt, probe.ProtocolResponses, opts.CodexModel})
	}
	if finals[contract.ClientClaudeCode] == ui.StatusUnconfirmed && binding.anthropic != nil {
		targets = append(targets, probeTarget{contract.ClientClaudeCode, binding.anthropic, probe.ProtocolMessages, opts.ClaudeOpusModel})
	}
	for _, target := range targets {
		emit(Event{Kind: EventStepStart, Client: target.id,
			Message: fmt.Sprintf("语义探针 %s(model=%s,key %s)", target.protocol, target.model, target.key.Key.Ref().Masked())})
		finals[target.id] = e.probeOne(ctx, opts.BaseURL, target.id, target.key.Key, target.protocol, target.model, emit)
	}
}

// probeOne 单端语义探针 → CONFIRMED / UNCONFIRMED(诚实态,非失败态)。
func (e *Engine) probeOne(ctx context.Context, baseURL string, id contract.ClientID, key secret.Key, protocol probe.Protocol, model string, emit func(Event)) ui.Status {
	prober, err := e.deps.NewSemanticProbe(baseURL, key, model)
	if err != nil {
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
			Message: "探针构造失败;配置已生效,relay-install verify 可复查"})
		return ui.StatusUnconfirmed
	}
	result, err := prober.Probe(ctx, protocol)
	if err == nil && result.Status == probe.StatusConfirmed {
		emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusConfirmed,
			Message: fmt.Sprintf("%s 语义验证通过(%dms)", protocol, result.Latency.Milliseconds())})
		return ui.StatusConfirmed
	}
	detail := "探针未证实"
	if err != nil {
		detail = err.Error()
	} else if result.Detail != "" {
		detail = result.Detail
	}
	emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
		Message: fmt.Sprintf("%s UNCONFIRMED(%s);配置已生效,relay-install verify 可复查", protocol, detail)})
	return ui.StatusUnconfirmed
}

// Verify 逐端重跑探针,输出七态。凭据只经 --key-stdin(verify 不开 wizard);
// 无凭据的端如实标 UNCONFIRMED,不猜。
func (e *Engine) Verify(ctx context.Context, opts Options, emit func(Event)) error {
	opts = opts.withDefaults()
	var binding keyBinding
	if opts.KeyStdin {
		keys, err := e.collectKeysStdin(ctx, opts, emit)
		if err != nil {
			return err
		}
		binding = bindKeys(keys)
	}
	detected, detectErrs := e.detectAll(ctx)
	for _, a := range e.deps.Adapters {
		id := a.ID()
		res := detected[id]
		if err := detectErrs[id]; err != nil {
			status, msg := detectStatus(res, err)
			emit(Event{Kind: EventFinal, Client: id, Status: status, Message: msg, Path: res.Path})
			continue
		}
		configured, note := e.managedConfigPresent(id, opts)
		if !configured {
			emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
				Message: "未发现受管配置(" + note + ")", Path: res.Path})
			continue
		}
		switch id {
		case contract.ClientCodex:
			if opts.SkipLive || binding.gpt == nil {
				emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
					Message: "受管配置存在;无凭据或 --skip-live,未做语义验证", Path: res.Path})
				continue
			}
			e.probeOne(ctx, opts.BaseURL, id, binding.gpt.Key, probe.ProtocolResponses, opts.CodexModel, emit)
		case contract.ClientClaudeCode:
			if opts.SkipLive || binding.anthropic == nil {
				emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
					Message: "受管配置存在;无凭据或 --skip-live,未做语义验证", Path: res.Path})
				continue
			}
			e.probeOne(ctx, opts.BaseURL, id, binding.anthropic.Key, probe.ProtocolMessages, opts.ClaudeOpusModel, emit)
		default:
			// ccswitch / 桌面端:语义探针在适配器侧未实现,只报配置存在性。
			emit(Event{Kind: EventFinal, Client: id, Status: ui.StatusUnconfirmed,
				Message: "目标存在;语义探针未实现,请经客户端实测确认", Path: res.Path})
		}
	}
	return nil
}

// managedConfigPresent 受管配置存在性(只读):codex 看受管 profile 头,
// claudecode 看 settings.json,ccswitch 看 live DB。
func (e *Engine) managedConfigPresent(id contract.ClientID, opts Options) (bool, string) {
	switch id {
	case contract.ClientCodex:
		home, err := e.codexHome()
		if err != nil {
			return false, err.Error()
		}
		path, err := adapters.CodexConfigPath(home, codexAlias(opts))
		if err != nil {
			return false, err.Error()
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return false, path + " 不存在"
		}
		if !adapters.IsManagedCodexConfig(data) {
			return false, path + " 非 relay-install 管理"
		}
		return true, path
	case contract.ClientClaudeCode:
		path, err := e.claudeCodeSettingsPath()
		if err != nil {
			return false, err.Error()
		}
		if _, err := os.Stat(path); err != nil {
			return false, path + " 不存在"
		}
		return true, path
	case contract.ClientCCSwitch:
		path, err := e.ccswitchDBPath()
		if err != nil {
			return false, err.Error()
		}
		if _, err := os.Stat(path); err != nil {
			return false, path + " 不存在"
		}
		return true, path
	default:
		return false, "无受管写入点"
	}
}

// Recover 调 txn journal 扫描收尾崩溃残留;逐条 journal 发事件。
func (e *Engine) Recover(ctx context.Context, emit func(Event)) error {
	entries, err := e.deps.NewTxnEngine().Recover(ctx)
	for _, entry := range entries {
		status := ui.StatusRolledBack
		if entry.State == txn.StateManualRecoveryRequired {
			status = ui.StatusManualRecoveryRequired
		} else if entry.State == txn.StateCommitted {
			status = ui.StatusNoOp
		}
		emit(Event{
			Kind:    EventFinal,
			Client:  contract.ClientID(entry.Client),
			Status:  status,
			Message: fmt.Sprintf("journal %s → %s(%d 个目标)", entry.TxnID, entry.State, len(entry.Targets)),
		})
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		emit(Event{Kind: EventStepDone, Status: ui.StatusNoOp, Message: "无待收尾 journal"})
	}
	return nil
}

func readFileIfExists(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// rememberBeforeImage 记录 before-image,供 txn Precondition 做 STALE 复检。
func rememberBeforeImage(change *adapters.Change, content []byte, existed bool) {
	change.BeforeKnown = true
	change.BeforeExisted = existed
	if existed {
		change.BeforeHash = sha256Of(content)
	}
}

// redactContent print-only 脱敏:明文 key 替换为 名称+末4位 引用。
func redactContent(content []byte, key secret.Key) string {
	redacted := string(content)
	key.Reveal(func(plaintext string) {
		if plaintext != "" {
			redacted = strings.ReplaceAll(redacted, plaintext, "<"+key.Ref().Masked()+">")
		}
	})
	return redacted
}

func sanitizeKey(key secret.Key, err error) error {
	if err == nil {
		return nil
	}
	leaks := false
	key.Reveal(func(plaintext string) {
		leaks = plaintext != "" && strings.Contains(err.Error(), plaintext)
	})
	if leaks {
		return errors.New("orchestrator: 操作失败(输出已脱敏)")
	}
	return err
}
