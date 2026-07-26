package adapters

import (
	"context"

	"relay-install/internal/contract"
	"relay-install/internal/probe"
	"relay-install/internal/txn"
)

// claudeDesktopAdapter Claude Desktop:仅 Win/macOS 两个写入实现注册到同一适配器
// (owner 裁定:Linux 桌面端移出 v1,不检测、不显示、不写入)。
// 不碰 claude_desktop_config.json,不尝试 1P 重定向;Probe 不可能,恒 RESTART_REQUIRED。
// 发布门槛:macOS 3P artifact 契约缺口 A1 未关闭前,该路径默认禁用并报 UNCONFIRMED。
type claudeDesktopAdapter struct{}

func NewClaudeDesktop() Adapter { return claudeDesktopAdapter{} }

func (claudeDesktopAdapter) ID() contract.ClientID { return contract.ClientClaudeDesktop }

// Detect 骨架占位:仅 Win/macOS 参与遍历(Linux 由 DetectTargets 裁剪)。
// TODO:Windows 查 HKCU\SOFTWARE\Policies\Claude;macOS artifact 待 A1 关闭。
func (claudeDesktopAdapter) Detect(context.Context) (DetectResult, error) {
	return DetectResult{
		Client: contract.ClientClaudeDesktop,
		Detail: "检测未实现(骨架);Linux 不在 v1 范围,仅 Win/macOS",
	}, nil
}

func (claudeDesktopAdapter) Plan(context.Context, PlanRequest) (Plan, error) {
	return Plan{}, ErrNotImplemented
}

func (claudeDesktopAdapter) Validate(context.Context, ChangeSet) error {
	return ErrNotImplemented
}

func (claudeDesktopAdapter) Apply(context.Context, ChangeSet) (txn.Result, error) {
	return txn.Result{}, ErrNotImplemented
}

// Probe 不可能:桌面端无探针通道,恒 RESTART_REQUIRED
func (claudeDesktopAdapter) Probe(context.Context) (probe.Result, error) {
	return probe.Result{}, ErrNotImplemented
}
