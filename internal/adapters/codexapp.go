package adapters

import (
	"context"
	"errors"

	"relay-install/internal/contract"
	"relay-install/internal/probe"
	"relay-install/internal/txn"
)

// codexAppAdapter Codex App:只检测不写入(owner 裁定),复用同一 profile,
// 结果恒 RESTART_REQUIRED(提示完全退出并重启)。
type codexAppAdapter struct{}

func NewCodexApp() Adapter { return codexAppAdapter{} }

func (codexAppAdapter) ID() contract.ClientID { return contract.ClientCodexApp }

// errNoApply Codex App 不实现 Apply;实施期以接口拆分做到编译期保证
var errNoApply = errors.New("codexapp: 只检测不写入(owner 裁定)")

// Detect 安装与运行状态(仅 Win/macOS 参与遍历;Linux 由 DetectTargets 裁剪)。
// TODO:实现各平台安装路径与运行状态检测;Windows 版是否读同一 ~/.codex 无实证。
func (codexAppAdapter) Detect(context.Context) (DetectResult, error) {
	return DetectResult{
		Client: contract.ClientCodexApp,
		Detail: "检测未实现(骨架);Linux 不在 v1 范围",
	}, nil
}

func (codexAppAdapter) Plan(context.Context, PlanRequest) (Plan, error) {
	return Plan{}, errNoApply
}

func (codexAppAdapter) Validate(context.Context, ChangeSet) error {
	return errNoApply
}

func (codexAppAdapter) Apply(context.Context, ChangeSet) (txn.Result, error) {
	return txn.Result{}, errNoApply
}

// Probe 不可能:桌面端无法语义级探针,结果恒 RESTART_REQUIRED
func (codexAppAdapter) Probe(context.Context) (probe.Result, error) {
	return probe.Result{}, errNoApply
}
