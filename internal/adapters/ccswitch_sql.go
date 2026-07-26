package adapters

// ccswitch SQL 导入方案(owner 新裁定,取代原先的 CLI/live 文件分支)。
// 核心是纯函数 GenerateCCSwitchSQL:输入目标 schema + provider 定义,输出官方导出格式 SQL。
// 生成的 SQL 为含密物料:0600 暂存、不入 journal、不入日志(明文 key 仅经 secret.Reveal 注入)。

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"regexp"
	"strings"

	"relay-install/internal/contract"
	"relay-install/internal/secret"
)

var errCCSwitchReadbackMismatch = errors.New("ccswitch: readback mismatch")

// ---- 目标 schema 注册表 ----

// CCSwitchSchema 目标 schema 描述(列清单/版本)
type CCSwitchSchema struct {
	UserVersion  int
	ProviderCols []string
	EndpointCols []string
}

// v11 实证列清单(本机 live DB,18 列)
var ccswitchProviderColsV11 = []string{
	"id", "app_type", "name", "settings_config", "website_url", "category",
	"created_at", "sort_index", "notes", "icon", "icon_color", "meta",
	"is_current", "in_failover_queue", "cost_multiplier",
	"limit_daily_usd", "limit_monthly_usd", "provider_type",
}

var ccswitchEndpointColsV11 = []string{"id", "provider_id", "app_type", "url", "added_at"}

// ccswitchSchemaFor 按 user_version 选 schema;超上限拒绝。
// TODO(contract):v16(owner Windows 导出)列清单暂按 v11 处理,差异待导出核实;
// 显式列名 INSERT 对"只增列"的漂移天然容忍。
func ccswitchSchemaFor(userVersion int) (CCSwitchSchema, error) {
	if userVersion > contract.CCSwitchMaxSchemaVersion {
		return CCSwitchSchema{}, fmt.Errorf("ccswitch: schema v%d 超已核实上限 v%d,拒绝(UNCONFIRMED)",
			userVersion, contract.CCSwitchMaxSchemaVersion)
	}
	if userVersion <= 11 {
		return CCSwitchSchema{UserVersion: 11, ProviderCols: ccswitchProviderColsV11, EndpointCols: ccswitchEndpointColsV11}, nil
	}
	return CCSwitchSchema{UserVersion: 16, ProviderCols: ccswitchProviderColsV11, EndpointCols: ccswitchEndpointColsV11}, nil
}

func readCCSwitchLiveSchema(ctx context.Context, bin, dbPath string, userVersion int) (CCSwitchSchema, error) {
	template, err := ccswitchSchemaFor(userVersion)
	if err != nil {
		return CCSwitchSchema{}, err
	}
	providerCols, err := readCCSwitchTableColumns(ctx, bin, dbPath, "providers")
	if err != nil {
		return CCSwitchSchema{}, err
	}
	endpointCols, err := readCCSwitchTableColumns(ctx, bin, dbPath, "provider_endpoints")
	if err != nil {
		return CCSwitchSchema{}, err
	}
	if err := requireCCSwitchColumns("providers", providerCols, template.ProviderCols); err != nil {
		return CCSwitchSchema{}, err
	}
	if err := requireCCSwitchColumns("provider_endpoints", endpointCols, template.EndpointCols); err != nil {
		return CCSwitchSchema{}, err
	}
	return CCSwitchSchema{
		UserVersion:  userVersion,
		ProviderCols: providerCols,
		EndpointCols: endpointCols,
	}, nil
}

func readCCSwitchTableColumns(ctx context.Context, bin, dbPath, table string) ([]string, error) {
	query := fmt.Sprintf("SELECT name FROM pragma_table_info(%s) ORDER BY cid;", sqlQuote(table))
	out, err := queryCCSwitch(ctx, bin, dbPath, query)
	if err != nil {
		return nil, err
	}
	var columns []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			columns = append(columns, line)
		}
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("ccswitch: live table %s has no columns", table)
	}
	return columns, nil
}

func requireCCSwitchColumns(table string, live, required []string) error {
	seen := make(map[string]struct{}, len(live))
	allowed := make(map[string]struct{}, len(required))
	for _, column := range required {
		allowed[column] = struct{}{}
	}
	for _, column := range live {
		seen[column] = struct{}{}
		if _, ok := allowed[column]; !ok {
			return fmt.Errorf("ccswitch: live table %s has unsupported column %s", table, column)
		}
	}
	for _, column := range required {
		if _, ok := seen[column]; !ok {
			return fmt.Errorf("ccswitch: live table %s missing required column %s", table, column)
		}
	}
	return nil
}

// ---- provider 定义与 settings_config 构造 ----

// CCSwitchProvider 一枚 provider 定义(幂等键 = id)
type CCSwitchProvider struct {
	ID        string // 稳定 id(如 "relay-intelalloc-claude")
	AppType   string // contract.CCSwitchApp* 三端之一
	Name      string
	BaseURL   string
	Model     string     // codex 端模型 ID(owner 运行时提供);其余端忽略
	Key       secret.Key // 明文仅在 Reveal 回调内进入含密生成物
	SortIndex int
	CreatedAt int64 // 毫秒时间戳;编排层 Plan 期应读现有行回填,保证跨日幂等(TODO 接线)
	IsCurrent bool
}

// GenerateCCSwitchSQL is pure: callers must first load explicit live templates.
// The returned SQL contains credentials and must remain inside the txn action.
func GenerateCCSwitchSQL(s CCSwitchSchema, providers []CCSwitchProvider, templates []CCSwitchTemplate) (string, error) {
	plan, err := buildCCSwitchImport(s, providers, templates)
	if err != nil {
		return "", err
	}
	return plan.SQL, nil
}

func validateCCSwitchGeneratedSecrets(sql string, providers []CCSwitchProvider) error {
	for _, provider := range providers {
		var got, want int
		provider.Key.Reveal(func(plaintext string) {
			got = strings.Count(sql, plaintext)
			for _, other := range providers {
				other.Key.Reveal(func(otherPlaintext string) {
					if plaintext == otherPlaintext {
						want++
						if other.AppType == contract.CCSwitchAppCodex {
							want++
						}
					}
				})
			}
		})
		if got != want {
			return fmt.Errorf("ccswitch: selected key appears %d times; want %d permitted settings_config locations", got, want)
		}
	}
	for _, field := range contract.CodexBlacklist {
		if strings.Contains(sql, field) {
			return fmt.Errorf("ccswitch: generated SQL contains blacklisted field %q", field)
		}
	}
	return nil
}

func validateCCSwitchProvider(p CCSwitchProvider) error {
	switch {
	case p.ID == "":
		return errors.New("ccswitch: provider id 为空")
	case p.Name == "":
		return errors.New("ccswitch: provider name 为空")
	case p.BaseURL == "":
		return errors.New("ccswitch: provider base_url 为空")
	case p.Key.Ref().Len == 0:
		return errors.New("ccswitch: provider key 为空")
	case p.AppType != contract.CCSwitchAppClaude && p.AppType != contract.CCSwitchAppCodex && p.AppType != contract.CCSwitchAppClaudeDesktop:
		return errors.New("ccswitch: 未知 app_type")
	}
	return nil
}

// String keeps accidental fmt logging from disclosing the key.
func (p CCSwitchProvider) String() string {
	return fmt.Sprintf("CCSwitchProvider{ID:%q AppType:%q Name:%q BaseURL:%q Model:%q Key:%s SortIndex:%d CreatedAt:%d IsCurrent:%t}",
		p.ID, p.AppType, p.Name, p.BaseURL, p.Model, p.Key.Ref().Masked(), p.SortIndex, p.CreatedAt, p.IsCurrent)
}

func (p CCSwitchProvider) GoString() string { return p.String() }

// sqlQuote SQL 字符串字面量转义(单引号双写)
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ---- 执行路径:sqlite3 单事务 → readback ----
// RunAction is the sole compensation owner: it creates before-images before
// this callback and performs journaled rollback on every returned error.

// execCCSwitchSQL 经 sqlite3 CLI 执行 SQL(stdin 喂入,.bail on 遇错即停);
// 调用方负责把 SQL 以 0600 暂存(此处走内存,不产生临时明文文件)
func execCCSwitchSQL(ctx context.Context, bin, dbPath, sql string) error {
	cmd := exec.CommandContext(ctx, bin, dbPath)
	cmd.Stdin = strings.NewReader(".bail on\n" + sql)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// stderr 可能回显含凭据的语句片段:先脱敏再入错误,宁少勿泄
		return fmt.Errorf("ccswitch: sqlite3 执行失败: %w: %s", err, sanitizeSQLiteStderr(stderr.String()))
	}
	return nil
}

var (
	sqliteKeyPattern  = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{4,}`)
	sqliteLongLiteral = regexp.MustCompile(`'[^']{8,}'`)
)

// sanitizeSQLiteStderr 脱敏 sqlite3 错误输出:屏蔽 key 形态与长字符串字面量,截断长度
func sanitizeSQLiteStderr(s string) string {
	s = sqliteKeyPattern.ReplaceAllString(s, "sk-…")
	s = sqliteLongLiteral.ReplaceAllString(s, "'…'")
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// readbackCCSwitchProviders compares every provider column against the exact
// clone plan. Endpoint ids are database-owned AUTOINCREMENT values: readback
// checks their integer type and the three managed identity/value fields only.
func readbackCCSwitchProviders(ctx context.Context, bin, dbPath string, schema CCSwitchSchema, want []ccswitchExpectedProvider) error {
	encodedColumns := make([]string, len(schema.ProviderCols))
	for i, column := range schema.ProviderCols {
		encodedColumns[i] = "hex(quote(" + sqlIdentifier(column) + "))"
	}
	for _, provider := range want {
		q := fmt.Sprintf("SELECT json_array(%s) FROM providers WHERE id=%s AND app_type=%s;",
			strings.Join(encodedColumns, ", "), sqlQuote(provider.ID), sqlQuote(provider.AppType))
		out, err := queryCCSwitch(ctx, bin, dbPath, q)
		if err != nil {
			return err
		}
		line := strings.TrimRight(out, "\n")
		if line == "" {
			return fmt.Errorf("%w: provider missing", errCCSwitchReadbackMismatch)
		}
		var encodedValues []string
		if err := json.Unmarshal([]byte(line), &encodedValues); err != nil || len(encodedValues) != len(provider.ProviderValues) {
			return fmt.Errorf("%w: provider row format", errCCSwitchReadbackMismatch)
		}
		for i, encodedValue := range encodedValues {
			decoded, err := hex.DecodeString(encodedValue)
			if err != nil || string(decoded) != provider.ProviderValues[i] {
				return fmt.Errorf("%w: provider fields", errCCSwitchReadbackMismatch)
			}
		}
		qe := fmt.Sprintf("SELECT json_array(typeof(id),provider_id,app_type,url) FROM provider_endpoints WHERE provider_id=%s AND app_type=%s;",
			sqlQuote(provider.ID), sqlQuote(provider.AppType))
		oute, err := queryCCSwitch(ctx, bin, dbPath, qe)
		if err != nil {
			return err
		}
		endpointLines := strings.Split(strings.TrimSpace(oute), "\n")
		if len(endpointLines) != 1 || endpointLines[0] == "" {
			return fmt.Errorf("%w: endpoint count", errCCSwitchReadbackMismatch)
		}
		var gotEndpoint []string
		if err := json.Unmarshal([]byte(endpointLines[0]), &gotEndpoint); err != nil {
			return fmt.Errorf("%w: endpoint row format", errCCSwitchReadbackMismatch)
		}
		wantEndpoint := []string{"integer", provider.ID, provider.AppType, provider.BaseURL}
		if !reflect.DeepEqual(gotEndpoint, wantEndpoint) {
			return fmt.Errorf("%w: endpoint", errCCSwitchReadbackMismatch)
		}
	}
	return nil
}

// queryCCSwitch 只读查询。调用方对多字段结果使用 JSON 编码，避免分隔符歧义。
func queryCCSwitch(ctx context.Context, bin, dbPath, q string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, "-readonly", "-noheader", "-list", "-separator", "\x1f", dbPath, q)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ccswitch: 查询失败: %w", err)
	}
	return string(out), nil
}

// applyCCSwitchSQL executes only the client-specific mutation and readback.
// Backup/rollback must remain in the surrounding txn.RunAction boundary.
func applyCCSwitchSQL(ctx context.Context, bin, dbPath string, plan ccswitchImport, schema CCSwitchSchema) error {
	if err := execCCSwitchSQL(ctx, bin, dbPath, plan.SQL); err != nil {
		return err
	}
	if err := readbackCCSwitchProviders(ctx, bin, dbPath, schema, plan.Providers); err != nil {
		return fmt.Errorf("ccswitch: readback 未通过: %w", err)
	}
	return nil
}
