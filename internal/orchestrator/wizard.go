package orchestrator

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"relay-install/internal/adapters"
	"relay-install/internal/contract"
	"relay-install/internal/secret"
	"relay-install/internal/ui"
)

// collectKeysWizard TTY 交互收集(裁定 #11 小白默认路径):key(可连续添加)、
// 安装位置(默认值回车接受)、一次授权确认;分组判定自动,计费默认订阅,档位 guarded。
// wizard 的全部终端/探针/计划效果经回调注入,本函数只做接线。
func (e *Engine) collectKeysWizard(ctx context.Context, opts Options, detected map[contract.ClientID]adapters.DetectResult, emit func(Event)) ([]BoundKey, error) {
	lang := ui.LanguageZH
	if opts.Lang == string(ui.LanguageEN) {
		lang = ui.LanguageEN
	}
	reader := bufio.NewReader(e.deps.TTY)
	var detectErrs map[contract.ClientID]error // 预览复用 plan 合成;检测错误此处不再重现

	wizard, err := ui.NewWizard(ui.WizardOptions{
		TTY:            true,
		Language:       lang,
		DefaultInstall: e.defaultInstallDisplay(),
		Clients:        wizardClientChoices(detected),
		Advanced:       false, // 高级层(unrestricted/多 alias/模型覆盖)不收进默认路径
	}, ui.WizardCallbacks{
		Output: func(text string) error {
			_, err := io.WriteString(e.deps.Output, text)
			return err
		},
		ReadLine: func(_ context.Context, prompt string) (string, error) {
			if _, err := io.WriteString(e.deps.Output, prompt); err != nil {
				return "", err
			}
			line, err := reader.ReadString('\n')
			if err != nil && line == "" {
				return "", err
			}
			return strings.TrimRight(line, "\r\n"), nil
		},
		ReadKey: func(ctx context.Context, prompt ui.KeyPrompt) (ui.KeyInput, error) {
			if _, err := io.WriteString(e.deps.Output, prompt.Prompt); err != nil {
				return ui.KeyInput{}, err
			}
			key, err := secret.ReadTTY(fmt.Sprintf("key-%d", prompt.Sequence), e.deps.TTY)
			if err != nil {
				return ui.KeyInput{}, err
			}
			input := ui.KeyInput{Key: key, Billing: ui.BillingSubscription}
			answer, err := wizardReadLine(ctx, e.deps.Output, reader, "再添加一枚 key?[y/N]: ")
			if err != nil {
				return ui.KeyInput{}, err
			}
			input.AddAnother = strings.EqualFold(strings.TrimSpace(answer), "y")
			return input, nil
		},
		ClassifyKey: func(ctx context.Context, key secret.Key) (ui.KeyClassification, error) {
			group, judgement := e.classifyKey(ctx, opts.BaseURL, key)
			status := ui.StatusConfirmed
			if group == ui.KeyGroupUnconfirmed {
				status = ui.StatusUnconfirmed
			}
			return ui.KeyClassification{Group: group, Status: status, Detail: judgement}, nil
		},
		BuildPlan: func(context.Context, ui.WizardResult) (ui.PlanView, error) {
			return e.buildPlanView(opts, detected, detectErrs), nil
		},
	})
	if err != nil {
		return nil, err
	}
	result, err := wizard.Run(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]BoundKey, 0, len(result.Keys))
	for _, k := range result.Keys {
		bound := BoundKey{
			Key:       k.Key,
			Group:     k.Group,
			Billing:   k.Billing,
			Status:    k.Status,
			Judgement: k.Judgement,
		}
		if bound.Billing == "" {
			bound.Billing = ui.BillingSubscription
		}
		out = append(out, bound)
		e.deps.Store.Add(k.Key)
		e.keys = append(e.keys, bound)
	}
	return out, nil
}

func wizardReadLine(_ context.Context, output io.Writer, reader *bufio.Reader, prompt string) (string, error) {
	if _, err := io.WriteString(output, prompt); err != nil {
		return "", err
	}
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// wizardClientChoices 仅已检测端进入默认全选清单。
func wizardClientChoices(detected map[contract.ClientID]adapters.DetectResult) []ui.ClientChoice {
	ids := make([]string, 0, len(detected))
	for id := range detected {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	choices := make([]ui.ClientChoice, 0, len(ids))
	for _, id := range ids {
		res := detected[contract.ClientID(id)]
		choices = append(choices, ui.ClientChoice{ID: id, Name: id, Detected: res.Installed})
	}
	return choices
}

func (e *Engine) defaultInstallDisplay() string {
	home, err := e.codexHome()
	if err != nil {
		return "默认各端标准路径"
	}
	return fmt.Sprintf("%s 与 ~/.claude(各端标准路径)", home)
}
