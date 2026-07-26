package ui

import (
	"context"
	"strings"
	"testing"

	"relay-install/internal/secret"
)

func TestWizardCanAddMultipleKeysAndKeepsFailedProbeUnconfirmed(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	script := &wizardScript{
		lines: []string{"", "yes"},
		keys: []KeyInput{
			{Key: mustKey(t, "gpt-main", "sk-gpt-AAAA1111"), AddAnother: true},
			{Key: mustKey(t, "metered-spare", "sk-gpt-BBBB2222"), Billing: BillingMetered},
		},
	}
	wizard, err := NewWizard(defaultWizardOptions(false), script.callbacks(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := wizard.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Keys) != 2 {
		t.Fatalf("应连续收集两枚 key,得到 %d", len(got.Keys))
	}
	spare := got.Keys[1]
	if spare.Group != KeyGroupUnconfirmed || spare.Status != StatusUnconfirmed || spare.Billing != BillingMetered {
		t.Fatalf("失败探针不得猜分组,且应保留计费性质:%+v", spare)
	}
	output := script.out.String()
	if !strings.Contains(output, "metered-spare(…2222") || !strings.Contains(output, "分组 UNCONFIRMED / 按量") {
		t.Fatalf("判定结果与计费性质必须明示:\n%s", output)
	}
}

func TestWizardNeverConfirmsAnUnknownGroup(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	script := &wizardScript{
		lines: []string{"", "yes"},
		keys:  []KeyInput{{Key: mustKey(t, "unknown", "sk-unknown-ABCD")}},
	}
	callbacks := script.callbacks(t)
	callbacks.ClassifyKey = func(context.Context, secret.Key) (KeyClassification, error) {
		return KeyClassification{Group: KeyGroup("invented"), Status: StatusConfirmed, Detail: "bad callback result"}, nil
	}
	wizard, err := NewWizard(defaultWizardOptions(false), callbacks)
	if err != nil {
		t.Fatal(err)
	}
	got, err := wizard.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Keys[0].Group != KeyGroupUnconfirmed || got.Keys[0].Status != StatusUnconfirmed {
		t.Fatalf("未知分组必须降为 UNCONFIRMED:%+v", got.Keys[0])
	}
	if strings.Contains(script.out.String(), "● unknown") {
		t.Fatalf("未知分组禁止绿色成功表达:%s", script.out.String())
	}
}
