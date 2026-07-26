package ui

import "relay-install/internal/secret"

// Card 客户端三行卡片:状态行 / 配置路径行 / 验证备注行。
// 不用表格(窄终端会崩);左侧 2 列缩进树形,客户端名等宽对齐。
type Card struct {
	Status Status
	Name   string // 客户端名
	Detail string // 状态行补充(版本/分支/原因)
	Path   string // 配置路径行,空则显示未提供
	Note   string // 验证/备注行,空则显示未提供
}

// RenderCard 渲染三行卡片
func (p *Printer) RenderCard(c Card) {
	detail := c.Detail
	if detail == "" {
		detail = p.text("无补充信息", "no additional detail")
	}
	path := c.Path
	if path == "" {
		path = p.text("未提供", "not provided")
	}
	note := c.Note
	if note == "" {
		note = p.text("未提供", "not provided")
	}
	p.Line("  %s %-24s %-16s %s", p.Symbol(c.Status), c.Status, c.Name, detail)
	p.Line("    %s: %s", p.text("配置路径", "config path"), path)
	p.Line("    %s: %s", p.text("验证/备注", "verification/note"), note)
}

// PlanKey is the only key representation accepted by the plan renderer. Ref
// guarantees name + last four characters without exposing the plaintext.
type PlanKey struct {
	Ref       secret.Ref
	Group     KeyGroup
	Billing   BillingKind
	Status    Status
	Judgement string
}

// PlanView plan 输出四节布局:检测 / 将执行 / 不会触碰 / 下一步。
// 措辞规则:成功不夸大;只有真实调用返回才说"验证通过"。
type PlanView struct {
	Header         string   // 标题行(含时间)
	Detect         []Card   // 检测节:逐端一张卡片
	WillDo         []string // 将执行(apply 时),编号列表
	WontTouch      []string // 不会触碰
	Next           string   // 下一步指引(→ 前缀由渲染层加)
	Keys           []PlanKey
	LiveCalls      int    // 语义级真实调用次数,0 表示已显式关闭
	CostNotice     string // 可能费用说明;向导预览要求非空
	BackupLocation string // before-image 留存位置;向导预览要求非空
}

// RenderPlan 渲染 plan 四节
func (p *Printer) RenderPlan(v PlanView) {
	p.Line("%s", v.Header)
	p.Line("")
	p.Line("%s", p.text("检测", "Detection"))
	for _, c := range v.Detect {
		p.RenderCard(c)
	}
	p.Line("")
	p.Line("%s", p.text("将执行(apply 时)", "Will run (during apply)"))
	for i, s := range v.WillDo {
		p.Line("  %d. %s", i+1, s)
	}
	if len(v.Keys) > 0 {
		p.Line("  %s", p.text("密钥与分组:", "Keys and groups:"))
		for _, k := range v.Keys {
			status := k.Status
			if status == "" {
				status = StatusUnconfirmed
			}
			p.Line("    %s %s  %s / %s  %s", p.Symbol(status), k.Ref.Masked(), k.Group.Display(p.lang), k.Billing.Display(p.lang), k.Judgement)
		}
	}
	p.Line("  %s: %d", p.text("语义级真实调用", "Semantic live calls"), v.LiveCalls)
	cost := v.CostNotice
	if cost == "" {
		cost = p.text("真实调用可能产生费用;以提供方计费为准", "Live calls may incur provider charges")
	}
	p.Line("  %s: %s", p.text("可能费用", "Possible cost"), cost)
	backup := v.BackupLocation
	if backup == "" {
		backup = p.text("由执行器在 apply 前确定", "determined by the executor before apply")
	}
	p.Line("  %s: %s", p.text("备份留存面", "Backup retention"), p.text("备份会复制目标中已有明文密钥,以 0600 留存于 ", "Backups copy existing plaintext keys and retain them with private permissions at ")+backup)
	p.Line("")
	p.Line("%s", p.text("不会触碰", "Will not touch"))
	for _, s := range v.WontTouch {
		p.Line("  %s", s)
	}
	p.Line("")
	p.Line("%s %s", p.guide(), v.Next)
}
