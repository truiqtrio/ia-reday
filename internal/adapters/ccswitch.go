package adapters

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"relay-install/internal/contract"
	"relay-install/internal/probe"
	"relay-install/internal/secret"
	"relay-install/internal/txn"
)

// ccswitchAdapter ccswitch:SQL 导入方案(owner 新裁定,取代原先的 CLI/live 文件分支)。
// Detect 三道闸门(顺序固定):
//  1. takeover 痕迹(proxy_config.live_takeover_active 置位 / 127.0.0.1:15721 监听)→ BLOCKED;
//  2. cc-switch 进程在运行 → 拒绝执行错误;
//  3. live DB user_version > 16 → 超已核实上限,拒绝(UNCONFIRMED)。
//
// 写入只经 providers/provider_endpoints 的 INSERT OR REPLACE;proxy_*/model_pricing 零写入。
type ccswitchAdapter struct {
	dbPath         string // live DB 路径;空 → 默认 ~/.cc-switch/cc-switch.db
	sqlite3        string // sqlite3 二进制;空 → PATH 查找
	engine         txn.Engine
	applyPreflight func(context.Context) error // test seam; production uses Detect
	commandRunner  ccSwitchCommandRunner       // test seam; nil uses exec.CommandContext
}

// ccSwitchCommandRunner isolates the platform command checks from process execution.
// It returns combined stdout/stderr, matching exec.Cmd.CombinedOutput.
type ccSwitchCommandRunner func(context.Context, string, ...string) ([]byte, error)

// CCSwitchChange is the structured desired state accepted by Apply. SQL is
// regenerated after the version gate so callers cannot inject unrelated writes.
type CCSwitchChange struct {
	Providers []CCSwitchProvider
}

func NewCCSwitch() Adapter { return ccswitchAdapter{} }

func (ccswitchAdapter) ID() contract.ClientID { return contract.ClientCCSwitch }

// Detect 错误哨兵:orchestrator 据此映射 BLOCKED / 拒绝 / UNCONFIRMED
var (
	ErrCCSwitchTakeover          = errors.New("ccswitch: 检测到代理接管痕迹(BLOCKED)")
	ErrCCSwitchRunning           = errors.New("ccswitch: cc-switch 进程运行中,拒绝执行")
	ErrCCSwitchSchemaTooNew      = errors.New("ccswitch: schema 版本超已核实上限(UNCONFIRMED)")
	ErrCCSwitchSafetyUnsupported = errors.New("ccswitch: 当前平台缺少可靠的进程/端口安全检测,拒绝执行")
	ErrCCSwitchJournalMode       = errors.New("ccswitch: SQLite journal_mode 不支持安全导入")
)

func (a ccswitchAdapter) resolvePaths() (db, bin string, err error) {
	db = a.dbPath
	if db == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", "", herr
		}
		db = filepath.Join(home, ".cc-switch", "cc-switch.db")
	}
	bin = a.sqlite3
	if bin == "" {
		bin, err = exec.LookPath("sqlite3")
		if err != nil {
			return "", "", fmt.Errorf("ccswitch: 找不到 sqlite3: %w", err)
		}
	}
	return db, bin, nil
}

func (a ccswitchAdapter) Detect(ctx context.Context) (DetectResult, error) {
	res := DetectResult{Client: contract.ClientCCSwitch}
	db, bin, err := a.resolvePaths()
	if err != nil {
		return res, err
	}
	if _, err := os.Stat(db); errors.Is(err, os.ErrNotExist) {
		res.Detail = "未发现 live DB(未安装)"
		return res, nil
	} else if err != nil {
		return res, err
	}
	res.Installed = true
	res.Path = db
	if !ccSwitchSafetyChecksSupported(runtime.GOOS) {
		res.Detail = "当前平台未实现可靠的 cc-switch 进程/接管端口检测"
		return res, fmt.Errorf("%w: %s", ErrCCSwitchSafetyUnsupported, runtime.GOOS)
	}

	// 闸门 1:takeover 痕迹(端口先行,不触 DB;再查 proxy_config)
	listening, err := port15721Listening(ctx, a.commandRunner)
	if err != nil {
		return res, err
	}
	if listening {
		res.Detail = "127.0.0.1:15721 监听中(代理接管痕迹)"
		return res, ErrCCSwitchTakeover
	}
	takeover, err := liveTakeoverActive(ctx, bin, db)
	if err != nil {
		return res, err
	}
	if takeover {
		res.Detail = "proxy_config.live_takeover_active 置位(代理接管痕迹)"
		return res, ErrCCSwitchTakeover
	}

	// 闸门 2:cc-switch 进程运行中 → 拒绝执行
	running, err := ccSwitchProcessRunning(ctx, a.commandRunner)
	if err != nil {
		return res, err
	}
	if running {
		res.Detail = "cc-switch 进程运行中"
		return res, ErrCCSwitchRunning
	}

	// 闸门 3:schema 版本门(>16 拒绝)
	v, err := readCCSwitchUserVersion(ctx, bin, db)
	if err != nil {
		return res, err
	}
	res.Version = fmt.Sprintf("user_version=%d", v)
	if _, err := readCCSwitchLiveSchema(ctx, bin, db, v); err != nil {
		if v > contract.CCSwitchMaxSchemaVersion {
			res.Detail = fmt.Sprintf("schema v%d 超上限 v%d", v, contract.CCSwitchMaxSchemaVersion)
			return res, ErrCCSwitchSchemaTooNew
		}
		res.Detail = "live schema 与已核实契约不匹配"
		return res, err
	}
	res.Detail = "live DB 可导入(SQL 导入方案)"
	return res, nil
}

// liveTakeoverActive 读 proxy_config.live_takeover_active;表/列不存在视为无痕迹
func liveTakeoverActive(ctx context.Context, bin, db string) (bool, error) {
	q := "SELECT COUNT(*) FROM proxy_config WHERE live_takeover_active IN (1, '1', 'true', 'enabled');"
	cmd := exec.CommandContext(ctx, bin, "-readonly", "-noheader", db, q)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := string(out)
		if strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column") {
			return false, nil // 无该表/列 → 无接管证据
		}
		return false, fmt.Errorf("ccswitch: takeover 检测查询失败: %w: %s", err, strings.TrimSpace(msg))
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return n > 0, err
}

// readCCSwitchUserVersion 只读 PRAGMA user_version
func readCCSwitchUserVersion(ctx context.Context, bin, db string) (int, error) {
	out, err := queryCCSwitch(ctx, bin, db, "PRAGMA user_version;")
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("ccswitch: user_version 解析失败: %w", err)
	}
	return v, nil
}

func readCCSwitchJournalMode(ctx context.Context, bin, db string) (string, error) {
	out, err := queryCCSwitch(ctx, bin, db, "PRAGMA journal_mode;")
	if err != nil {
		return "", err
	}
	mode := strings.ToLower(strings.TrimSpace(out))
	switch mode {
	case "wal", "delete", "truncate", "persist":
		return mode, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrCCSwitchJournalMode, mode)
	}
}

func ccSwitchSafetyChecksSupported(goos string) bool {
	switch goos {
	case "linux", "darwin", "windows":
		return true
	default:
		return false
	}
}

// port15721Listening 检测 127.0.0.1:15721(0x3D69)是否 LISTEN。
// Linux 继续读 /proc/net/tcp{,6};macOS 和 Windows 使用各自的系统命令。
func port15721Listening(ctx context.Context, runner ccSwitchCommandRunner) (bool, error) {
	return port15721ListeningForOS(ctx, runtime.GOOS, runner)
}

func port15721ListeningForOS(ctx context.Context, goos string, runner ccSwitchCommandRunner) (bool, error) {
	switch goos {
	case "linux":
		return port15721ListeningLinux()
	case "darwin":
		out, err := runCCSwitchCommand(ctx, runner, "lsof", "-nP", "-iTCP:15721", "-sTCP:LISTEN")
		if commandHasNoMatches(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("ccswitch: lsof 端口检测失败: %w", err)
		}
		return parseLsofListening(out), nil
	case "windows":
		out, err := runCCSwitchCommand(ctx, runner, "netstat", "-ano")
		if err != nil {
			return false, fmt.Errorf("ccswitch: netstat 端口检测失败: %w", err)
		}
		return parseWindowsNetstatListening(out), nil
	default:
		return false, fmt.Errorf("%w: %s", ErrCCSwitchSafetyUnsupported, goos)
	}
}

// Linux 读 /proc/net/tcp{,6};未能读取 IPv4 内核表时失败关闭。
func port15721ListeningLinux() (bool, error) {
	ipv4Readable := false
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if f == "/proc/net/tcp" {
			ipv4Readable = true
		}
		for i, line := range strings.Split(string(data), "\n") {
			if i == 0 || line == "" {
				continue // 表头
			}
			fields := strings.Fields(line)
			// fields[1]=local_address(XXXX:PORT),fields[3]=st(0A=LISTEN)
			if len(fields) > 3 && strings.HasSuffix(strings.ToUpper(fields[1]), ":3D69") && fields[3] == "0A" {
				return true, nil
			}
		}
	}
	if !ipv4Readable {
		return false, errors.New("ccswitch: 无法读取 IPv4 /proc 网络表,拒绝执行")
	}
	return false, nil
}

// ccSwitchProcessRunning 扫 /proc/*/comm 找二进制名 cc-switch
// (本机 /usr/local/bin/ccswitch 只是其启动器,进程特征以 cc-switch 为准)。
func ccSwitchProcessRunning(ctx context.Context, runner ccSwitchCommandRunner) (bool, error) {
	return ccSwitchProcessRunningForOS(ctx, runtime.GOOS, runner)
}

func ccSwitchProcessRunningForOS(ctx context.Context, goos string, runner ccSwitchCommandRunner) (bool, error) {
	switch goos {
	case "linux":
		return ccSwitchProcessRunningLinux()
	case "darwin":
		out, err := runCCSwitchCommand(ctx, runner, "pgrep", "-fl", "cc-switch")
		if commandHasNoMatches(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("ccswitch: pgrep 进程检测失败: %w", err)
		}
		return parsePgrepCCSwitch(out), nil
	case "windows":
		out, err := runCCSwitchCommand(ctx, runner, "tasklist")
		if err != nil {
			return false, fmt.Errorf("ccswitch: tasklist 进程检测失败: %w", err)
		}
		return parseWindowsTasklistCCSwitch(out), nil
	default:
		return false, fmt.Errorf("%w: %s", ErrCCSwitchSafetyUnsupported, goos)
	}
}

func ccSwitchProcessRunningLinux() (bool, error) {
	comms, err := filepath.Glob("/proc/[0-9]*/comm")
	if err != nil {
		return false, fmt.Errorf("ccswitch: 扫描进程失败: %w", err)
	}
	self := strconv.Itoa(os.Getpid())
	for _, c := range comms {
		// 排除自身(/proc/<pid>/comm)
		if strings.Contains(c, "/proc/"+self+"/") {
			continue
		}
		data, err := os.ReadFile(c)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // 进程可能已退出
			}
			return false, fmt.Errorf("ccswitch: 无法检查进程,拒绝执行: %w", err)
		}
		if strings.TrimSpace(string(data)) == "cc-switch" {
			return true, nil
		}
	}
	return false, nil
}

func runCCSwitchCommand(ctx context.Context, runner ccSwitchCommandRunner, name string, args ...string) ([]byte, error) {
	if runner != nil {
		return runner(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func commandHasNoMatches(err error) bool {
	type exitCoder interface{ ExitCode() int }
	var exitErr exitCoder
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

func parseLsofListening(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "COMMAND ") {
			return true
		}
	}
	return false
}

func parsePgrepCCSwitch(out []byte) bool {
	return strings.TrimSpace(string(out)) != ""
}

func parseWindowsNetstatListening(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.EqualFold(fields[0], "TCP") || !strings.EqualFold(fields[3], "LISTENING") {
			continue
		}
		if windowsEndpointHasPort(fields[1], "15721") {
			return true
		}
	}
	return false
}

func windowsEndpointHasPort(endpoint, port string) bool {
	index := strings.LastIndex(endpoint, ":")
	return index >= 0 && endpoint[index+1:] == port
}

func parseWindowsTasklistCCSwitch(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && (strings.EqualFold(fields[0], "cc-switch.exe") || strings.EqualFold(fields[0], "cc-switch")) {
			return true
		}
	}
	return false
}

func (ccswitchAdapter) Plan(context.Context, PlanRequest) (Plan, error) {
	// TODO:读 live DB 现有行回填 CreatedAt(幂等)、构造三端 CCSwitchProvider、
	// key 经 secret.Reveal 注入、GenerateCCSwitchSQL 生成脱敏 diff(明文不出现)
	return Plan{}, ErrNotImplemented
}

func (ccswitchAdapter) Validate(_ context.Context, set ChangeSet) error {
	if set.Client != contract.ClientCCSwitch {
		return fmt.Errorf("ccswitch: 错误的 ChangeSet 客户端 %q", set.Client)
	}
	if set.CCSwitch == nil || len(set.CCSwitch.Providers) == 0 || len(set.Changes) != 0 {
		return errors.New("ccswitch: ChangeSet 必须且只能包含结构化 SQL 导入目标")
	}
	for _, provider := range set.CCSwitch.Providers {
		if err := validateCCSwitchProvider(provider); err != nil {
			return err
		}
	}
	return nil
}

func (a ccswitchAdapter) Apply(ctx context.Context, set ChangeSet) (txn.Result, error) {
	if err := a.Validate(ctx, set); err != nil {
		return txn.Result{}, err
	}
	providers := append([]CCSwitchProvider(nil), set.CCSwitch.Providers...)
	txnSecrets := make([]secret.Key, 0, len(providers))
	for _, provider := range providers {
		txnSecrets = append(txnSecrets, provider.Key)
	}
	sanitize := func(err error) error { return sanitizeKeysError(txnSecrets, err) }
	db, bin, err := a.resolvePaths()
	if err != nil {
		return txn.Result{}, sanitize(err)
	}
	if err := validateCCSwitchJournalMetadata(db, providers); err != nil {
		return txn.Result{}, sanitize(err)
	}
	if a.applyPreflight != nil {
		if err := a.applyPreflight(ctx); err != nil {
			return txn.Result{}, sanitize(err)
		}
	} else {
		result, err := a.Detect(ctx)
		if err != nil {
			return txn.Result{}, sanitize(err)
		}
		if !result.Installed {
			return txn.Result{}, errors.New("ccswitch: live DB 不存在,拒绝导入")
		}
	}

	version, err := readCCSwitchUserVersion(ctx, bin, db)
	if err != nil {
		return txn.Result{}, sanitize(err)
	}
	schema, err := readCCSwitchLiveSchema(ctx, bin, db, version)
	if err != nil {
		return txn.Result{}, sanitize(err)
	}
	journalMode, err := readCCSwitchJournalMode(ctx, bin, db)
	if err != nil {
		return txn.Result{}, sanitize(err)
	}
	templates, err := readCCSwitchTemplates(ctx, bin, db, schema, providers)
	if err != nil {
		return txn.Result{}, sanitize(err)
	}
	plan, err := buildCCSwitchImport(schema, providers, templates)
	if err != nil {
		return txn.Result{}, sanitize(err)
	}
	targets := []string{db, db + "-wal", db + "-shm", db + "-journal"}
	var precreate []string
	if journalMode == "wal" {
		precreate = []string{db + "-wal", db + "-shm"}
	} else {
		precreate = []string{db + "-journal"}
	}
	result, err := defaultTxnEngine(a.engine).RunAction(ctx, txn.ActionRequest{
		Client:           string(contract.ClientCCSwitch),
		Targets:          targets,
		Secrets:          txnSecrets,
		PrecreateMissing: precreate,
		IsNoop: func(ctx context.Context) (bool, error) {
			err := readbackCCSwitchProviders(ctx, bin, db, schema, plan.Providers)
			if err == nil {
				return true, nil
			}
			if errors.Is(err, errCCSwitchReadbackMismatch) {
				return false, nil
			}
			return false, err
		},
		Execute: func(ctx context.Context, _ string) error {
			return applyCCSwitchSQL(ctx, bin, db, plan, schema)
		},
	})
	return result, sanitize(err)
}

func validateCCSwitchJournalMetadata(db string, providers []CCSwitchProvider) error {
	for _, provider := range providers {
		leaks := false
		provider.Key.Reveal(func(plaintext string) {
			leaks = plaintext != "" && (strings.Contains(db, plaintext) || strings.Contains(string(contract.ClientCCSwitch), plaintext))
		})
		if leaks {
			return errors.New("ccswitch: selected key appears in transaction metadata")
		}
	}
	return nil
}

func (ccswitchAdapter) Probe(context.Context) (probe.Result, error) {
	// SQL 导入后由 CC Switch 自身接管调用;语义级探针复用 claude/codex 端通道(TODO)
	return probe.Result{}, ErrNotImplemented
}
