// Package contract 各端写入契约的结构化表达:写入点、受管字段、黑名单字段、解析边界。
// 本包是唯一允许引用具体客户端字段名/路径的包,也是唯一随上游漂移改动的包。
package contract

// ClientID 客户端标识(v1 范围五端)
type ClientID string

const (
	ClientCodex         ClientID = "codex"
	ClientCodexApp      ClientID = "codexapp"
	ClientClaudeCode    ClientID = "claudecode"
	ClientClaudeDesktop ClientID = "claudedesktop"
	ClientCCSwitch      ClientID = "ccswitch"
)

// ParserKind 解析器种类:契约层只认这几种格式边界
type ParserKind string

const (
	ParserTOML     ParserKind = "toml"     // Codex CLI
	ParserJSON     ParserKind = "json"     // Claude Code / ccswitch live 文件
	ParserPlist    ParserKind = "plist"    // macOS Claude Desktop(契约缺口 A1 未关闭)
	ParserRegistry ParserKind = "registry" // Windows Claude Desktop(HKCU)
)

// Parser 解析边界:所有格式解析只允许经此接口,要求无损往返(Parse∘Marshal 语义相等)。
// 骨架不绑定具体 TOML/JSON 库,实施期注入;三层校验的第一层依赖此往返。
type Parser interface {
	Kind() ParserKind
	Parse(data []byte) (any, error)
	Marshal(v any) ([]byte, error)
}

// WritePoint 一个写入点(文件 / 注册表键 / CLI 通道)
type WritePoint struct {
	Client    ClientID
	Kind      ParserKind
	PathHint  string   // 路径模板;$CODEX_HOME / ~ 在 adapter Detect 期解析
	Perm      uint32   // 目标权限;密钥配置一律 0600,注册表为 0
	Managed   []string // 受管字段:显式枚举,禁止前缀匹配
	Blacklist []string // 黑名单字段:写入前必须确认不存在
}

// CodexBlacklist Codex 三个废弃字段(0.145.0 严格解析拒绝),
// 任何写入前的黑名单扫描必须确认三者不存在。
var CodexBlacklist = []string{
	"disable_response_storage",
	"network_access",
	"windows_wsl_setup_acknowledged",
}

const (
	CodexManagedHeader                = "# relay-install:managed v1\n"
	CodexFieldModelProvider           = "model_provider"
	CodexFieldModel                   = "model"
	CodexFieldReviewModel             = "review_model"
	CodexFieldModelReasoningEffort    = "model_reasoning_effort"
	CodexFieldPlanModeReasoningEffort = "plan_mode_reasoning_effort"
	CodexFieldApprovalPolicy          = "approval_policy"
	CodexFieldSandboxMode             = "sandbox_mode"
	CodexFieldModelProviders          = "model_providers"
	CodexProviderIntelalloc           = "intelalloc"
	CodexProviderFieldName            = "name"
	CodexProviderFieldBaseURL         = "base_url"
	CodexProviderFieldWireAPI         = "wire_api"
	CodexProviderFieldBearerToken     = "experimental_bearer_token"
	CodexWireAPIResponses             = "responses"
	CodexApprovalOnRequest            = "on-request"
	CodexApprovalNever                = "never"
	CodexSandboxWorkspaceWrite        = "workspace-write"
	CodexSandboxDangerFullAccess      = "danger-full-access"
	CodexConfigFilenamePrefix         = "intelalloc-"
	CodexConfigFilenameSuffix         = ".config.toml"
)

func CodexConfigFilename(alias string) string {
	return CodexConfigFilenamePrefix + alias + CodexConfigFilenameSuffix
}

// CodexManagedFields 是 relay-install 生成的 Codex profile 字段全集。
var CodexManagedFields = []string{
	CodexFieldModelProvider, CodexFieldModel, CodexFieldReviewModel, CodexFieldModelReasoningEffort,
	CodexFieldPlanModeReasoningEffort, CodexFieldApprovalPolicy, CodexFieldSandboxMode,
	CodexFieldModelProviders + "." + CodexProviderIntelalloc + "." + CodexProviderFieldName,
	CodexFieldModelProviders + "." + CodexProviderIntelalloc + "." + CodexProviderFieldBaseURL,
	CodexFieldModelProviders + "." + CodexProviderIntelalloc + "." + CodexProviderFieldWireAPI,
	CodexFieldModelProviders + "." + CodexProviderIntelalloc + "." + CodexProviderFieldBearerToken,
}

const (
	ClaudeCodeFieldEnv              = "env"
	ClaudeCodeEnvBaseURL            = "ANTHROPIC_BASE_URL"
	ClaudeCodeEnvAuthToken          = "ANTHROPIC_AUTH_TOKEN"
	ClaudeCodeEnvDefaultOpusModel   = "ANTHROPIC_DEFAULT_OPUS_MODEL"
	ClaudeCodeEnvDefaultSonnetModel = "ANTHROPIC_DEFAULT_SONNET_MODEL"
	ClaudeCodeEnvDefaultHaikuModel  = "ANTHROPIC_DEFAULT_HAIKU_MODEL"
)

// ClaudeCodeManagedEnv Claude Code settings.json env 块受管变量(显式枚举 5 个,不用前缀匹配)。
var ClaudeCodeManagedEnv = []string{
	ClaudeCodeEnvBaseURL,
	ClaudeCodeEnvAuthToken,
	ClaudeCodeEnvDefaultOpusModel,
	ClaudeCodeEnvDefaultSonnetModel,
	ClaudeCodeEnvDefaultHaikuModel,
}

// WritePoints 各端写入点定义(v1)。
// Codex App 无写入点(只检测不写入);ccswitch 优先 CLI 通道,live 文件仅 fallback。
func WritePoints() []WritePoint {
	return []WritePoint{
		{
			Client:   ClientCodex,
			Kind:     ParserTOML,
			PathHint: "$CODEX_HOME/" + CodexConfigFilename("<alias>"),
			Perm:     0o600,
			// 仅含已验证字段 + provider 段;安全档位由显式 profile 选择。
			Managed:   append([]string(nil), CodexManagedFields...),
			Blacklist: CodexBlacklist,
		},
		{
			Client:   ClientClaudeCode,
			Kind:     ParserJSON,
			PathHint: "~/.claude/settings.json",
			Perm:     0o600,
			Managed:  ClaudeCodeManagedEnv, // merge 式:只覆盖这 5 个,其余键语义保留
		},
		{
			Client:   ClientClaudeDesktop,
			Kind:     ParserRegistry,
			PathHint: `HKCU\SOFTWARE\Policies\Claude`, // Windows 四个 REG_SZ;macOS 走 plist(A1 未关闭)
			// TODO(contract):四个 REG_SZ 名称与 macOS artifact 待契约冻结
			Managed: []string{},
		},
		{
			Client:   ClientCCSwitch,
			Kind:     ParserJSON, // settings_config 的 JSON 形态;落盘经 SQL 导入,不走文件解析器
			PathHint: "~/.cc-switch/cc-switch.db(SQL 导入,官方导出格式;owner 裁定取代 CLI/live 文件分支)",
			Perm:     0o600, // 生成的 SQL 为含密物料:0600 暂存,不入 journal/日志
			Managed:  []string{"providers", "provider_endpoints"},
			// 零写入表见 CCSwitchNoTouchTables;任何情况不直接改 proxy_*/model_pricing
		},
	}
}

// ---- ccswitch SQLite 导入(owner 新裁定,取代 CLI/live 文件分支)----

// CCSwitchMaxSchemaVersion 已核实 schema 版本上限;更高一律拒绝(UNCONFIRMED)
const CCSwitchMaxSchemaVersion = 16

// CCSwitchDefaultDBHint 默认 live DB 路径($HOME 展开在 adapter 做)
const CCSwitchDefaultDBHint = "~/.cc-switch/cc-switch.db"

// ccswitch 三端 app_type(写 providers.app_type / provider_endpoints.app_type)
const (
	CCSwitchAppClaude        = "claude"
	CCSwitchAppCodex         = "codex"
	CCSwitchAppClaudeDesktop = "claude-desktop"
)

// CCSwitchNoTouchTables 零写入表(已知者):导入 SQL 不得出现,执行前后哈希必须不变(测试断言)。
// 规则实为前缀:proxy_* 一律不碰,另加 model_pricing。
var CCSwitchNoTouchTables = []string{
	"proxy_config",
	"model_pricing",
}

// CCSwitchNoTouchPrefix proxy_ 前缀的表一律零写入
const CCSwitchNoTouchPrefix = "proxy_"
