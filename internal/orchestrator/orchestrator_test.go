package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relay-install/internal/adapters"
	"relay-install/internal/contract"
	"relay-install/internal/probe"
	"relay-install/internal/secret"
	"relay-install/internal/txn"
	"relay-install/internal/ui"
)

// ---- mock adapter:记录全部调用,不触文件/网络/txn ----

type mockAdapter struct {
	id        contract.ClientID
	detect    adapters.DetectResult
	detectErr error
	planCalls int
	sets      []adapters.ChangeSet
	applyErr  error
	applyRes  txn.Result
}

func (m *mockAdapter) ID() contract.ClientID { return m.id }

func (m *mockAdapter) Detect(context.Context) (adapters.DetectResult, error) {
	res := m.detect
	res.Client = m.id
	return res, m.detectErr
}

func (m *mockAdapter) Plan(context.Context, adapters.PlanRequest) (adapters.Plan, error) {
	m.planCalls++
	return adapters.Plan{}, adapters.ErrNotImplemented
}

func (m *mockAdapter) Validate(context.Context, adapters.ChangeSet) error { return nil }

func (m *mockAdapter) Apply(_ context.Context, set adapters.ChangeSet) (txn.Result, error) {
	m.sets = append(m.sets, set)
	if m.applyErr != nil {
		return m.applyRes, m.applyErr
	}
	res := m.applyRes
	if res.State == "" {
		res.State = txn.StateCommitted
	}
	return res, nil
}

func (m *mockAdapter) Probe(context.Context) (probe.Result, error) {
	return probe.Result{}, adapters.ErrNotImplemented
}

// ---- mock /v1/models 探针 ----

type mockModelsProber struct {
	res probe.ModelsResult
	err error
}

func (m mockModelsProber) Probe(context.Context) (probe.ModelsResult, error) {
	return m.res, m.err
}

func confirmedModels(groups map[probe.ModelGroup][]string) probe.ModelsResult {
	return probe.ModelsResult{
		Result: probe.Result{Protocol: probe.ProtocolModels, Status: probe.StatusConfirmed, OK: true},
		Groups: groups,
	}
}

// modelsByLast4 按 key 末 4 位路由 mock 探针结果。
func modelsByLast4(t *testing.T, routes map[string]probe.ModelsResult) func(string, secret.Key) (ModelsProber, error) {
	t.Helper()
	return func(_ string, key secret.Key) (ModelsProber, error) {
		res, ok := routes[key.Ref().Last4]
		if !ok {
			return nil, errors.New("unexpected key")
		}
		return mockModelsProber{res: res}, nil
	}
}

func installed(id contract.ClientID) *mockAdapter {
	return &mockAdapter{
		id:     id,
		detect: adapters.DetectResult{Installed: true, Version: "1.0.0", Path: "/mock/" + string(id)},
	}
}

func testDeps(t *testing.T, adaptersList ...adapters.Adapter) Deps {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	return Deps{
		Adapters: adaptersList,
		HomeDir:  home,
		Stdin:    strings.NewReader(""),
		Output:   os.Stdout,
	}
}

func collect(events *[]Event) func(Event) {
	return func(ev Event) { *events = append(*events, ev) }
}

func countKind(events []Event, kind EventKind) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// TestPlanIsReadOnly Plan 纯只读:不调用 Apply、不触探针、不创建任何文件。
func TestPlanIsReadOnly(t *testing.T) {
	codex := installed(contract.ClientCodex)
	claude := installed(contract.ClientClaudeCode)
	ccswitch := &mockAdapter{id: contract.ClientCCSwitch} // 未安装

	deps := testDeps(t, codex, claude, ccswitch)
	deps.NewModelsProbe = func(string, secret.Key) (ModelsProber, error) {
		t.Fatal("plan 不得访问网络探针")
		return nil, nil
	}
	deps.NewSemanticProbe = func(string, secret.Key, string) (probe.Prober, error) {
		t.Fatal("plan 不得访问网络探针")
		return nil, nil
	}

	var events []Event
	err := New(deps).Plan(context.Background(), Options{}, collect(&events))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for _, a := range []*mockAdapter{codex, claude, ccswitch} {
		if a.planCalls == 0 {
			t.Errorf("%s: 未调用适配器 Plan", a.id)
		}
		if len(a.sets) != 0 {
			t.Errorf("%s: plan 调用了 Apply(%d 次)", a.id, len(a.sets))
		}
	}
	if got := countKind(events, EventDetectDone); got != 3 {
		t.Errorf("检测节事件 = %d, want 3", got)
	}
	if countKind(events, EventPlanWillDo) == 0 {
		t.Error("缺少「将执行」节")
	}
	if countKind(events, EventPlanWontTouch) == 0 {
		t.Error("缺少「不会触碰」节")
	}
	if got := countKind(events, EventPlanNext); got != 1 {
		t.Errorf("下一步指引 = %d, want 1", got)
	}
	// 无副作用断言:home 与 CODEX_HOME 下不得创建任何文件
	for _, dir := range []string{deps.HomeDir, os.Getenv("CODEX_HOME")} {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadDir %s: %v", dir, err)
		}
		if len(entries) != 0 {
			t.Errorf("plan 在 %s 留下副作用: %v", dir, entries)
		}
	}
}

// TestApplyGroupBinding 分组绑定:GPT key 只落 codex,Anthropic key 只落 claude
// 系,中国组只在 ccswitch 成独立 provider 条目。
func TestApplyGroupBinding(t *testing.T) {
	codex := installed(contract.ClientCodex)
	claude := installed(contract.ClientClaudeCode)
	ccswitch := installed(contract.ClientCCSwitch)

	deps := testDeps(t, codex, claude, ccswitch)
	deps.Stdin = strings.NewReader("sk-gpt-aaaa1111\nsk-ant-bbbb2222\nsk-china-cccc3333\n")
	deps.NewModelsProbe = modelsByLast4(t, map[string]probe.ModelsResult{
		"1111": confirmedModels(map[probe.ModelGroup][]string{probe.GroupGPT: {"gpt-5.6-sol-high"}}),
		"2222": confirmedModels(map[probe.ModelGroup][]string{probe.GroupAnthropic: {"claude-opus-5"}}),
		"3333": confirmedModels(map[probe.ModelGroup][]string{probe.GroupChina: {"glm-5.2"}}),
	})

	var events []Event
	err := New(deps).Apply(context.Background(), Options{KeyStdin: true, SkipLive: true}, collect(&events))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// codex 端:恰好一次写入,内容只含 GPT key
	if len(codex.sets) != 1 || len(codex.sets[0].Changes) != 1 {
		t.Fatalf("codex ChangeSet = %+v", codex.sets)
	}
	codexContent := string(codex.sets[0].Changes[0].Content)
	if !strings.Contains(codexContent, "sk-gpt-aaaa1111") {
		t.Error("codex 配置缺少 GPT key")
	}
	if strings.Contains(codexContent, "sk-ant-bbbb2222") || strings.Contains(codexContent, "sk-china-cccc3333") {
		t.Error("codex 配置混入其他组 key")
	}

	// claudecode 端:恰好一次写入,内容只含 Anthropic key
	if len(claude.sets) != 1 || len(claude.sets[0].Changes) != 1 {
		t.Fatalf("claudecode ChangeSet = %+v", claude.sets)
	}
	claudeContent := string(claude.sets[0].Changes[0].Content)
	if !strings.Contains(claudeContent, "sk-ant-bbbb2222") {
		t.Error("claudecode settings 缺少 Anthropic key")
	}
	if strings.Contains(claudeContent, "sk-gpt-aaaa1111") || strings.Contains(claudeContent, "sk-china-cccc3333") {
		t.Error("claudecode settings 混入其他组 key")
	}

	// ccswitch:四张独立 provider 条目,中国组独立、不落 codex/claude 端
	if len(ccswitch.sets) != 1 || ccswitch.sets[0].CCSwitch == nil {
		t.Fatalf("ccswitch ChangeSet = %+v", ccswitch.sets)
	}
	providers := ccswitch.sets[0].CCSwitch.Providers
	if len(providers) != 4 {
		t.Fatalf("provider 数 = %d, want 4: %v", len(providers), providers)
	}
	type want struct {
		appType  string
		keyPlain string
	}
	wants := []want{
		{contract.CCSwitchAppCodex, "sk-gpt-aaaa1111"},
		{contract.CCSwitchAppClaude, "sk-ant-bbbb2222"},
		{contract.CCSwitchAppClaudeDesktop, "sk-ant-bbbb2222"},
		{contract.CCSwitchAppClaude, "sk-china-cccc3333"},
	}
	for i, w := range wants {
		p := providers[i]
		if p.AppType != w.appType {
			t.Errorf("provider[%d] app_type = %q, want %q", i, p.AppType, w.appType)
		}
		p.Key.Reveal(func(plaintext string) {
			if plaintext != w.keyPlain {
				t.Errorf("provider[%d] key = %q, want %q", i, plaintext, w.keyPlain)
			}
		})
	}
	if !strings.HasPrefix(providers[3].ID, "relay-intelalloc-china") {
		t.Errorf("中国组条目 ID = %q, 应为独立条目", providers[3].ID)
	}
	if countKind(events, EventRollback) != 0 {
		t.Error("出现非预期回滚事件")
	}
}

// TestApplyProbeFailureSkipsKey 探针失败的 key 标 UNCONFIRMED 并跳过,
// 不落到任何端;全部未确认时整体中止且不写任何端。
func TestApplyProbeFailureSkipsKey(t *testing.T) {
	codex := installed(contract.ClientCodex)
	claude := installed(contract.ClientClaudeCode)
	ccswitch := installed(contract.ClientCCSwitch)

	deps := testDeps(t, codex, claude, ccswitch)
	deps.Stdin = strings.NewReader("sk-mystery-9999\n")
	deps.NewModelsProbe = modelsByLast4(t, map[string]probe.ModelsResult{
		"9999": {Result: probe.Result{Protocol: probe.ProtocolModels, Status: probe.StatusUnconfirmed, Unconfirmed: true}},
	})

	var events []Event
	err := New(deps).Apply(context.Background(), Options{KeyStdin: true, SkipLive: true}, collect(&events))
	if err == nil {
		t.Fatal("全部 key UNCONFIRMED 时 Apply 应返回错误")
	}
	for _, a := range []*mockAdapter{codex, claude, ccswitch} {
		if len(a.sets) != 0 {
			t.Errorf("%s: UNCONFIRMED key 不应触发写入", a.id)
		}
	}
	unconfirmedSeen := false
	for _, ev := range events {
		if ev.Status == ui.StatusUnconfirmed && strings.Contains(ev.Message, "UNCONFIRMED") ||
			ev.Status == ui.StatusUnconfirmed && strings.Contains(ev.Message, "不猜分组") {
			unconfirmedSeen = true
		}
	}
	if !unconfirmedSeen {
		t.Error("缺少 key UNCONFIRMED 判定事件")
	}
}

// TestApplyExplicitGroupSkipsProbe stdin 显式 "分组:key" 不走探针,直接绑定。
func TestApplyExplicitGroupSkipsProbe(t *testing.T) {
	codex := installed(contract.ClientCodex)
	claude := installed(contract.ClientClaudeCode)
	ccswitch := installed(contract.ClientCCSwitch)

	deps := testDeps(t, codex, claude, ccswitch)
	deps.Stdin = strings.NewReader("gpt:sk-gpt-aaaa1111\n")
	deps.NewModelsProbe = func(string, secret.Key) (ModelsProber, error) {
		t.Fatal("显式分组不应触发探针")
		return nil, nil
	}

	var events []Event
	err := New(deps).Apply(context.Background(), Options{KeyStdin: true, SkipLive: true}, collect(&events))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(codex.sets) != 1 {
		t.Fatalf("codex 写入次数 = %d, want 1", len(codex.sets))
	}
	if len(claude.sets) != 0 {
		t.Error("无 Anthropic key 时 claudecode 不应写入")
	}
	if len(ccswitch.sets) != 1 || len(ccswitch.sets[0].CCSwitch.Providers) != 1 {
		t.Fatalf("ccswitch 应只导入 codex 条目: %+v", ccswitch.sets)
	}
}

func TestApplyCCSwitchMissingTemplateIsUnconfirmedWithoutRollback(t *testing.T) {
	codex := installed(contract.ClientCodex)
	ccswitch := installed(contract.ClientCCSwitch)
	ccswitch.applyErr = adapters.ErrCCSwitchTemplateNotFound

	deps := testDeps(t, codex, ccswitch)
	deps.Stdin = strings.NewReader("gpt:sk-gpt-aaaa1111\n")
	var events []Event
	if err := New(deps).Apply(context.Background(), Options{KeyStdin: true, SkipLive: true}, collect(&events)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	found := false
	for _, event := range events {
		if event.Client == contract.ClientCCSwitch && event.Kind == EventFinal &&
			event.Status == ui.StatusUnconfirmed && strings.Contains(event.Message, "CC Switch GUI") {
			found = true
		}
		if event.Client == contract.ClientCCSwitch && event.Kind == EventRollback {
			t.Fatalf("missing template emitted rollback: %#v", event)
		}
	}
	if !found {
		t.Fatalf("missing template did not emit GUI UNCONFIRMED guidance: %#v", events)
	}
}

// TestRecoverJournalScan Recover 走 txn journal 扫描收尾。
func TestRecoverJournalScan(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := `{"txn_id":"t1","client":"codex","state":"COMMITTED","targets":[],"changes":[]}`
	if err := os.WriteFile(filepath.Join(stateDir, "t1.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := testDeps(t)
	deps.NewTxnEngine = func() txn.Engine {
		return txn.NewFileEngine(txn.Options{
			StateDir:   stateDir,
			BackupRoot: filepath.Join(dir, "backups"),
			LockPath:   filepath.Join(dir, "txn.lock"),
		})
	}

	var events []Event
	if err := New(deps).Recover(context.Background(), collect(&events)); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	found := false
	for _, ev := range events {
		if strings.Contains(ev.Message, "t1") {
			found = true
		}
	}
	if !found {
		t.Errorf("缺少 journal t1 的收尾事件: %v", events)
	}
}

// TestRecoverEmpty 无 journal 时如实报告。
func TestRecoverEmpty(t *testing.T) {
	dir := t.TempDir()
	deps := testDeps(t)
	deps.NewTxnEngine = func() txn.Engine {
		return txn.NewFileEngine(txn.Options{
			StateDir:   filepath.Join(dir, "state"),
			BackupRoot: filepath.Join(dir, "backups"),
			LockPath:   filepath.Join(dir, "txn.lock"),
		})
	}
	var events []Event
	if err := New(deps).Recover(context.Background(), collect(&events)); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(events) != 1 || !strings.Contains(events[0].Message, "无待收尾") {
		t.Errorf("事件 = %v, want 单条无待收尾", events)
	}
}
