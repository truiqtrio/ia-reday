// Package adapters 每端一个适配器,实现统一接口 Detect/Plan/Validate/Apply/Probe,
// 差异收敛于 Detect 与 Probe。
// 硬约束:适配器只产出 Plan/ChangeSet,禁止自行 IO 写;一切写入经由 txn 引擎。
package adapters

import (
	"context"
	"errors"
	"runtime"

	"relay-install/internal/contract"
	"relay-install/internal/probe"
	"relay-install/internal/secret"
	"relay-install/internal/txn"
)

// ErrNotImplemented remains for planning/probing and disabled desktop targets.
var ErrNotImplemented = errors.New("adapters: 未实现(骨架)")

// DetectResult 检测结果
type DetectResult struct {
	Client    contract.ClientID
	Installed bool
	Version   string
	Path      string // 二进制或目标文件解析后路径
	Running   bool
	Detail    string // 分支说明(如 ccswitch 三级分支、平台裁剪原因)
}

// PlanRequest plan 输入;密钥只带脱敏引用,明文不出 secret 包
type PlanRequest struct {
	BaseURL string // 已校验的 HTTPS 根地址
	Profile string
	Key     secret.Ref
}

// Change 单个写入点的目标态内容(merge 式写入由适配器在 Plan 期合成完整内容)
type Change struct {
	Point              contract.WritePoint
	Content            []byte
	Secret             secret.Key
	AllowedSecretPaths [][]string
	BeforeKnown        bool
	BeforeExisted      bool
	BeforeHash         [32]byte
}

// ChangeSet 一个客户端一次 apply 的全部变更(回滚单元)。文件型客户端使用
// Changes；ccswitch 使用结构化导入目标，SQL 在 Apply 前重新生成，拒绝任意 SQL。
type ChangeSet struct {
	Client   contract.ClientID
	Changes  []Change
	CCSwitch *CCSwitchChange
}

// Plan 只读计划;脱敏 diff 在实施期补全
type Plan struct {
	Client contract.ClientID
	Set    ChangeSet
	Notes  []string
}

// Adapter 统一接口(候选定义)
type Adapter interface {
	ID() contract.ClientID
	Detect(ctx context.Context) (DetectResult, error)
	Plan(ctx context.Context, req PlanRequest) (Plan, error)
	Validate(ctx context.Context, set ChangeSet) error
	// Apply 只许把 ChangeSet 交 txn 执行；禁用端仍返回不可写错误。
	Apply(ctx context.Context, set ChangeSet) (txn.Result, error)
	Probe(ctx context.Context) (probe.Result, error)
}

// All 全部已注册适配器
func All() []Adapter {
	return []Adapter{
		NewCodex(),
		NewCodexApp(),
		NewClaudeCode(),
		NewClaudeDesktop(),
		NewCCSwitch(),
	}
}

// DetectTargets 参与检测遍历的适配器。
// 平台裁剪(owner 裁定):Linux(含 WSL2)不检测、不显示 Claude Desktop 与 Codex App。
func DetectTargets() []Adapter {
	var out []Adapter
	for _, a := range All() {
		if runtime.GOOS == "linux" {
			switch a.ID() {
			case contract.ClientCodexApp, contract.ClientClaudeDesktop:
				continue
			}
		}
		out = append(out, a)
	}
	return out
}
