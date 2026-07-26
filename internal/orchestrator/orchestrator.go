// Package orchestrator 编排 plan/apply/verify/recover 流程:
// 调 adapters 收集检测与 Plan,经 txn 执行 ChangeSet,经 probe 裁决最终状态。
// 通过事件契约与 ui 解耦:orchestrator 不直接 print(硬约束)。
package orchestrator

import (
	"context"

	"relay-install/internal/contract"
	"relay-install/internal/ui"
)

// EventKind 事件种类(apply 进度区与 plan 四节均由事件驱动)
type EventKind int

const (
	EventDetectDone  EventKind = iota // 单端检测完成
	EventStepStart                    // 执行步骤开始(备份/暂存/提交/探针)
	EventStepDone                     // 执行步骤完成
	EventStepSkipped                  // 失败后后续步骤跳过
	EventRollback                     // 回滚步骤
	EventFinal                        // 单端最终状态(七态之一)
	EventPlanWillDo                   // plan"将执行(apply 时)"条目
	EventPlanWontTouch                // plan"不会触碰"条目
	EventPlanNext                     // plan"下一步"指引
)

// Event 输出事件:ui 订阅渲染,orchestrator 不感知终端
type Event struct {
	Kind    EventKind
	Client  contract.ClientID
	Status  ui.Status
	Message string // 已脱敏;密钥只出现 名称+末4位
	Path    string // 相关路径(检测卡片/写入点),可空
}

// Options 全局 flag 的编排侧投影(与 cmd 的 flag 一一对应)
type Options struct {
	BaseURL   string // HTTPS 根地址,无 query/fragment/userinfo;最终 URL 在 plan 脱敏展示
	Profile   string
	KeyStdin  bool // 非交互唯一密钥通道
	SkipLive  bool // 关闭语义级真实调用
	PrintOnly bool // 冲突/权限不足/校验失败时输出可手贴片段而非硬写
	Lang      string

	// 模型默认值(owner 裁定 2026-07-25 #7);留空由 withDefaults 落默认值。
	// sonnet/haiku 仅作可选扩展项,默认路径不出现。
	CodexModel        string
	CodexReviewModel  string
	ClaudeOpusModel   string
	ClaudeSonnetModel string
	ClaudeHaikuModel  string
}

// 默认值(owner 裁定 #7 与任务约束):IntelAlloc 生产端点与各端默认模型。
const (
	DefaultBaseURL           = "https://backend.intelalloc.com"
	DefaultCodexModel        = "gpt-5.6-sol-high"
	DefaultCodexReviewModel  = "gpt-5.6-sol"
	DefaultCodexEffort       = "high"
	DefaultCodexPlanEffort   = "xhigh"
	DefaultClaudeOpusModel   = "claude-opus-5"
	DefaultClaudeSonnetModel = "claude-sonnet-5[1M]"
	DefaultClaudeHaikuModel  = "claude-fable-5"
)

// withDefaults 落地全部默认值;档位恒为 guarded(小白默认路径,裁定 #11)。
func (o Options) withDefaults() Options {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	if o.CodexModel == "" {
		o.CodexModel = DefaultCodexModel
	}
	if o.CodexReviewModel == "" {
		o.CodexReviewModel = DefaultCodexReviewModel
	}
	if o.ClaudeOpusModel == "" {
		o.ClaudeOpusModel = DefaultClaudeOpusModel
	}
	if o.ClaudeSonnetModel == "" {
		o.ClaudeSonnetModel = DefaultClaudeSonnetModel
	}
	if o.ClaudeHaikuModel == "" {
		o.ClaudeHaikuModel = DefaultClaudeHaikuModel
	}
	return o
}

// Orchestrator 流程编排接口骨架
type Orchestrator interface {
	// Plan 纯只读:不建锁、不跑客户端、不访问网络;首尾各取一次哈希,变化即报 STALE
	Plan(ctx context.Context, opts Options, emit func(Event)) error
	// Apply 两阶段:先凭据与档位(权限只问一次),后执行;失败按 txn 语义回滚或 UNCONFIRMED
	Apply(ctx context.Context, opts Options, emit func(Event)) error
	// Verify 事后单独重跑探针,闭环"重启后确认"的时间差
	Verify(ctx context.Context, opts Options, emit func(Event)) error
	// Recover 依据 journal 收尾崩溃残留
	Recover(ctx context.Context, emit func(Event)) error
}
