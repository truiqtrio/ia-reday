package orchestrator

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"relay-install/internal/adapters"
	"relay-install/internal/contract"
	"relay-install/internal/probe"
	"relay-install/internal/secret"
	"relay-install/internal/txn"
	"relay-install/internal/ui"
)

// ErrStale plan 首尾哈希不一致:目标在只读遍历期间被外部改动。
var ErrStale = errors.New("orchestrator: plan 期间目标发生变化(STALE),请重试")

// ModelsProber 抽象 /v1/models 探针,便于测试注入;生产实现为 probe.ModelsProbe。
type ModelsProber interface {
	Probe(ctx context.Context) (probe.ModelsResult, error)
}

// Deps 全部外部效果的注入点:适配器、探针、txn 引擎、终端、命令执行。
// 零值字段由 New 落生产默认;测试整体替换,不改其他包。
type Deps struct {
	Adapters []adapters.Adapter // nil → adapters.DetectTargets()(Linux 自动裁剪桌面端)

	// NewModelsProbe 分组判定探针(/v1/models)。
	NewModelsProbe func(baseURL string, key secret.Key) (ModelsProber, error)
	// NewSemanticProbe 语义级探针(responses/messages),model 由调用方按端指定。
	NewSemanticProbe func(baseURL string, key secret.Key, model string) (probe.Prober, error)
	// NewTxnEngine recover 用 journal 引擎(apply 写入路径由各适配器内部默认引擎承担,路径一致)。
	NewTxnEngine func() txn.Engine

	Stdin      io.Reader // --key-stdin 来源;nil → os.Stdin
	StdinIsTTY bool      // stdin 是否交互终端(决定走 wizard 还是要求 --key-stdin)
	TTY        *os.File  // wizard 交互输入;nil → os.Stdin
	Output     io.Writer // wizard 等交互输出(经 ui 回调,业务不直接 print);nil → io.Discard

	LookPath   func(string) (string, error)
	RunCommand func(ctx context.Context, name string, args ...string) ([]byte, error)

	HomeDir string          // 测试覆盖;空 → os.UserHomeDir
	Store   *secret.Store   // 命名 key 清单(内存);nil → 新建
	Now     func() time.Time // nil → time.Now
}

// Engine 编排实现,满足 Orchestrator 接口。
type Engine struct {
	deps Deps
	keys []BoundKey // 本次会话已收集的命名 key(keys 子命令清单来源;持久化见 TODO)
}

var _ Orchestrator = (*Engine)(nil)

// New 构造编排引擎并落生产默认依赖。
func New(deps Deps) *Engine {
	if deps.Adapters == nil {
		deps.Adapters = adapters.DetectTargets()
	}
	if deps.NewModelsProbe == nil {
		deps.NewModelsProbe = func(baseURL string, key secret.Key) (ModelsProber, error) {
			client, err := probe.NewClient(baseURL, key)
			if err != nil {
				return nil, err
			}
			return probe.NewModelsProbe(client), nil
		}
	}
	if deps.NewSemanticProbe == nil {
		deps.NewSemanticProbe = func(baseURL string, key secret.Key, model string) (probe.Prober, error) {
			client, err := probe.NewClient(baseURL, key)
			if err != nil {
				return nil, err
			}
			return probe.NewHTTPProber(client, model)
		}
	}
	if deps.NewTxnEngine == nil {
		deps.NewTxnEngine = func() txn.Engine { return txn.NewFileEngine(txn.Options{}) }
	}
	if deps.Stdin == nil {
		deps.Stdin = os.Stdin
	}
	if deps.TTY == nil {
		deps.TTY = os.Stdin
	}
	if deps.Output == nil {
		deps.Output = io.Discard
	}
	if deps.LookPath == nil {
		deps.LookPath = exec.LookPath
	}
	if deps.RunCommand == nil {
		deps.RunCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	if deps.Store == nil {
		deps.Store = secret.NewStore()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Engine{deps: deps}
}

func (e *Engine) homeDir() (string, error) {
	if e.deps.HomeDir != "" {
		return e.deps.HomeDir, nil
	}
	return os.UserHomeDir()
}

// codexHome 与 codex 适配器同一解析顺序:$CODEX_HOME 优先,否则 ~/.codex。
func (e *Engine) codexHome() (string, error) {
	if fromEnv := os.Getenv("CODEX_HOME"); fromEnv != "" {
		return fromEnv, nil
	}
	home, err := e.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func (e *Engine) claudeCodeSettingsPath() (string, error) {
	home, err := e.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func (e *Engine) ccswitchDBPath() (string, error) {
	home, err := e.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cc-switch", "cc-switch.db"), nil
}

// backupRootDisplay plan 展示的备份留存面(只读,不创建目录);与 txn 默认一致。
func backupRootDisplay() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "<user-config>/relay-install/backups"
	}
	return filepath.Join(dir, "relay-install", "backups")
}

// sha256Of before-image 哈希辅助。
func sha256Of(data []byte) [32]byte {
	return sha256.Sum256(data)
}

func (e *Engine) adapterByID(id contract.ClientID) adapters.Adapter {
	for _, a := range e.deps.Adapters {
		if a.ID() == id {
			return a
		}
	}
	return nil
}

// detectAll 遍历 DetectTargets 收集检测;单端错误不中断其他端。
func (e *Engine) detectAll(ctx context.Context) (map[contract.ClientID]adapters.DetectResult, map[contract.ClientID]error) {
	results := make(map[contract.ClientID]adapters.DetectResult, len(e.deps.Adapters))
	errs := make(map[contract.ClientID]error, len(e.deps.Adapters))
	for _, a := range e.deps.Adapters {
		res, err := a.Detect(ctx)
		results[a.ID()] = res
		if err != nil {
			errs[a.ID()] = err
		}
	}
	return results, errs
}

// detectStatus 检测结果 → 七态映射(BLOCKED 仅 takeover/版本拒绝等硬阻断)。
func detectStatus(res adapters.DetectResult, err error) (ui.Status, string) {
	msg := res.Detail
	switch {
	case errors.Is(err, adapters.ErrCCSwitchTakeover):
		return ui.StatusBlocked, msg
	case errors.Is(err, adapters.ErrCCSwitchRunning):
		return ui.StatusBlocked, msg
	case errors.Is(err, adapters.ErrCodexVersionTooOld):
		return ui.StatusBlocked, msg
	case errors.Is(err, adapters.ErrCCSwitchSchemaTooNew):
		return ui.StatusUnconfirmed, msg
	case err != nil:
		if msg == "" {
			msg = err.Error()
		} else {
			msg += "; " + err.Error()
		}
		return ui.StatusUnconfirmed, msg
	case !res.Installed:
		if msg == "" {
			msg = "未安装"
		}
		return ui.StatusUnconfirmed, msg
	default:
		if res.Version != "" {
			if msg != "" {
				msg = res.Version + "  " + msg
			} else {
				msg = res.Version
			}
		}
		return ui.StatusNoOp, msg
	}
}
