package orchestrator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"relay-install/internal/adapters"
	"relay-install/internal/contract"
	"relay-install/internal/probe"
	"relay-install/internal/secret"
	"relay-install/internal/ui"
)

// BoundKey 已完成分组判定的命名 key;分组归属决定它落到哪些客户端。
// 绑定规则(cc-switch 分析 + 裁定 #11):GPT 组 → codex;Anthropic 组 →
// Claude Code/Desktop;中国大模型组 → ccswitch 独立 provider 条目;
// UNCONFIRMED 不猜、不绑定、跳过。
type BoundKey struct {
	Key       secret.Key
	Group     ui.KeyGroup
	Billing   ui.BillingKind
	Status    ui.Status
	Judgement string
}

// rawKey stdin 一行的解析结果:显式分组前缀或待探针判定的裸 key。
type rawKey struct {
	key           secret.Key
	explicitGroup ui.KeyGroup // 空 → 走 /v1/models 探针判定
}

// stdinGroupPrefixes "分组:key" 可识别前缀(大小写不敏感)。
var stdinGroupPrefixes = map[string]ui.KeyGroup{
	"gpt":       ui.KeyGroupGPT,
	"anthropic": ui.KeyGroupAnthropic,
	"claude":    ui.KeyGroupAnthropic,
	"china":     ui.KeyGroupChina,
}

// parseStdinKeys 逐行解析 --key-stdin 输入:
// 每行 "分组:key"(gpt/anthropic/china)或单枚裸 key(自动探针判定)。
func parseStdinKeys(r io.Reader) ([]rawKey, error) {
	var out []rawKey
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	n := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		n++
		name := fmt.Sprintf("key-%d", n)
		var group ui.KeyGroup
		value := line
		if head, rest, ok := strings.Cut(line, ":"); ok {
			if g, recognized := stdinGroupPrefixes[strings.ToLower(strings.TrimSpace(head))]; recognized && strings.TrimSpace(rest) != "" {
				group = g
				value = strings.TrimSpace(rest)
			}
		}
		key, err := secret.New(name, value)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: stdin 第 %d 行: %w", n, err)
		}
		out = append(out, rawKey{key: key, explicitGroup: group})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("orchestrator: 读取 stdin: %w", err)
	}
	return out, nil
}

// classifyKey 经 /v1/models 探针判定分组;失败一律 UNCONFIRMED,不猜。
func (e *Engine) classifyKey(ctx context.Context, baseURL string, key secret.Key) (ui.KeyGroup, string) {
	prober, err := e.deps.NewModelsProbe(baseURL, key)
	if err != nil {
		return ui.KeyGroupUnconfirmed, "/v1/models 探针不可用;不猜分组"
	}
	res, err := prober.Probe(ctx)
	if err != nil || res.Status != probe.StatusConfirmed {
		return ui.KeyGroupUnconfirmed, "/v1/models 探针失败;不猜分组"
	}
	// 判定优先级 GPT > Anthropic > 中国组;仅按探针返回的模型清单,不推断。
	for _, candidate := range []struct {
		probeGroup probe.ModelGroup
		group      ui.KeyGroup
		label      string
	}{
		{probe.GroupGPT, ui.KeyGroupGPT, "GPT 组"},
		{probe.GroupAnthropic, ui.KeyGroupAnthropic, "Anthropic 组"},
		{probe.GroupChina, ui.KeyGroupChina, "中国大模型组"},
	} {
		if models := res.Groups[candidate.probeGroup]; len(models) > 0 {
			return candidate.group, fmt.Sprintf("识别为%s(%s 等 %d 个模型)", candidate.label, models[0], len(models))
		}
	}
	return ui.KeyGroupUnconfirmed, "/v1/models 未返回可识别分组的模型;不猜分组"
}

// collectKeysStdin 非交互通道:解析 → (显式分组信任 / 裸 key 探针判定) → 入会话清单。
func (e *Engine) collectKeysStdin(ctx context.Context, opts Options, emit func(Event)) ([]BoundKey, error) {
	raw, err := parseStdinKeys(e.deps.Stdin)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, ui.ErrNoKeys
	}
	var out []BoundKey
	for _, entry := range raw {
		bound := BoundKey{Key: entry.key, Billing: ui.BillingSubscription, Status: ui.StatusConfirmed}
		if entry.explicitGroup != "" {
			bound.Group = entry.explicitGroup
			bound.Judgement = "stdin 显式指定分组(未走探针)"
		} else {
			bound.Group, bound.Judgement = e.classifyKey(ctx, opts.BaseURL, entry.key)
			if bound.Group == ui.KeyGroupUnconfirmed {
				bound.Status = ui.StatusUnconfirmed
			}
		}
		out = append(out, bound)
		e.deps.Store.Add(entry.key)
		e.keys = append(e.keys, bound)
		emit(Event{
			Kind:    EventStepDone,
			Status:  bound.Status,
			Message: fmt.Sprintf("key %s → %s / %s  %s", entry.key.Ref().Masked(), bound.Group.Display(ui.Language(opts.Lang)), bound.Billing.Display(ui.Language(opts.Lang)), bound.Judgement),
		})
	}
	return out, nil
}

// keyBinding 分组 → 客户端的绑定结果(每组取第一枚已确认 key)。
type keyBinding struct {
	gpt       *BoundKey
	anthropic *BoundKey
	china     []*BoundKey
}

// bindKeys 只绑定已确认分组;UNCONFIRMED key 跳过(不落到任何端)。
func bindKeys(keys []BoundKey) keyBinding {
	var binding keyBinding
	for i := range keys {
		k := &keys[i]
		if k.Status != ui.StatusConfirmed {
			continue
		}
		switch k.Group {
		case ui.KeyGroupGPT:
			if binding.gpt == nil {
				binding.gpt = k
			}
		case ui.KeyGroupAnthropic:
			if binding.anthropic == nil {
				binding.anthropic = k
			}
		case ui.KeyGroupChina:
			binding.china = append(binding.china, k)
		}
	}
	return binding
}

// collectKeys 阶段 A 凭据收集:--key-stdin 或 TTY wizard;其余拒绝。
func (e *Engine) collectKeys(ctx context.Context, opts Options, detected map[contract.ClientID]adapters.DetectResult, emit func(Event)) ([]BoundKey, error) {
	if opts.KeyStdin {
		return e.collectKeysStdin(ctx, opts, emit)
	}
	if e.deps.StdinIsTTY {
		return e.collectKeysWizard(ctx, opts, detected, emit)
	}
	return nil, fmt.Errorf("%w(非交互模式必须 --key-stdin)", ui.ErrInteractiveTTYRequired)
}

// ListKeys keys 子命令:列出本次会话已命名 key(名称+末4位+分组+计费性质)。
// TODO:secret.Store 目前只有内存能力,无持久化;跨会话清单待 secret 包扩展后接线。
func (e *Engine) ListKeys(ctx context.Context, opts Options, emit func(Event)) error {
	opts = opts.withDefaults()
	if opts.KeyStdin {
		if _, err := e.collectKeysStdin(ctx, opts, emit); err != nil {
			return err
		}
	}
	if len(e.keys) == 0 {
		emit(Event{Kind: EventStepDone, Status: ui.StatusUnconfirmed,
			Message: "本次会话暂无已命名 key(key 清单仅存内存,持久化 TODO;--key-stdin 可录入并列出)"})
		return nil
	}
	lang := ui.Language(opts.Lang)
	for _, k := range e.keys {
		status := k.Status
		if status == "" {
			status = ui.StatusUnconfirmed
		}
		emit(Event{
			Kind:    EventFinal,
			Status:  status,
			Message: fmt.Sprintf("%s  %s / %s  %s", k.Key.Ref().Masked(), k.Group.Display(lang), k.Billing.Display(lang), k.Judgement),
		})
	}
	return nil
}

var errNoConfirmedKeys = errors.New("orchestrator: 无已确认分组的 key(全部 UNCONFIRMED),中止执行")
