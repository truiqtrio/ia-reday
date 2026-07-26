package adapters

// ccswitch SQL 导入测试:全部用临时目录 fixture,不碰真实 ~/.cc-switch/cc-switch.db。
// fixture 经 sqlite3 CLI 现场建库(v11 / v16 两套)。

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"relay-install/internal/contract"
	"relay-install/internal/secret"
)

const ccswitchCanary = "sk-canary-XXXXyyyyZZZZaaaaBBBBccccDDDDeeeeFFFF1234567890abcd"

func requireSQLite3(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("环境无 sqlite3,跳过 ccswitch SQL 测试")
	}
	return bin
}

// makeCCSwitchFixture 建 v11 形态的 fixture 库(18 列 providers + endpoints + 零写入表),
// user_version 可指定;takeover=true 时 proxy_config.live_takeover_active 置位。
func makeCCSwitchFixture(t *testing.T, bin string, userVersion int, takeover bool) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "cc-switch.db")
	tk := 0
	if takeover {
		tk = 1
	}
	ddl := `
CREATE TABLE providers (id TEXT PRIMARY KEY, app_type TEXT, name TEXT, settings_config TEXT,
  website_url TEXT, category TEXT, created_at INTEGER, sort_index INTEGER, notes TEXT,
  icon TEXT, icon_color TEXT, meta TEXT NOT NULL DEFAULT '{}', is_current INTEGER, in_failover_queue INTEGER,
  cost_multiplier REAL, limit_daily_usd REAL, limit_monthly_usd REAL, provider_type TEXT);
CREATE TABLE provider_endpoints (id INTEGER PRIMARY KEY AUTOINCREMENT, provider_id TEXT, app_type TEXT, url TEXT, added_at INTEGER);
CREATE TABLE proxy_config (id INTEGER PRIMARY KEY, app_type TEXT, live_takeover_active INTEGER, listen_addr TEXT);
CREATE TABLE model_pricing (id INTEGER PRIMARY KEY, model TEXT, price REAL);
INSERT INTO proxy_config VALUES (1, 'codex', ` + itoa(tk) + `, '127.0.0.1:15721');
INSERT INTO model_pricing VALUES (1, 'gpt-5.6-luna', 0.5);
INSERT INTO providers VALUES
  ('default-claude', 'claude', 'default',
   '{"env":{"ANTHROPIC_BASE_URL":"https://backend.intelalloc.com","ANTHROPIC_AUTH_TOKEN":"original-claude","KEEP_ENV":"yes"},"permissions":{"allow":["Read"]}}',
   'https://intelalloc.com/claude', 'official-claude', 1700000000001, 7, 'keep-claude',
   'claude-icon', '#102030', '{"origin":"official-claude"}', 1, 1, 1.75, 12.5, 34.5, 'official');
	INSERT INTO providers VALUES
	  ('default-codex', 'codex', 'default',
	   '{"auth":{"OPENAI_API_KEY":"original-codex","keep_auth":"yes"},"config":"model = \"old-model\"\ndisable_response_storage = true\nnetwork_access = \"enabled\"\nwindows_wsl_setup_acknowledged = true\n[model_providers.intelalloc]\nbase_url = \"https://legacy.example.invalid/v1\"\nexperimental_bearer_token = \"original-codex\"\nwire_api = \"responses\"\n","mcp":{"keep":true}}',
   'https://intelalloc.com/codex', 'official-codex', 1700000000002, 8, 'keep-codex',
   'codex-icon', '#203040', '{"origin":"official-codex"}', 0, 0, 2.25, 22.5, 44.5, 'official');
INSERT INTO providers VALUES
  ('default-desktop', 'claude-desktop', 'default',
   '{"env":{"ANTHROPIC_BASE_URL":"https://backend.intelalloc.com","ANTHROPIC_AUTH_TOKEN":"original-desktop","KEEP_ENV":"desktop"},"desktop":{"keep":true}}',
   'https://intelalloc.com/desktop', 'official-desktop', 1700000000003, 9, 'keep-desktop',
   'desktop-icon', '#304050', '{"origin":"official-desktop"}', 0, 1, 3.25, 32.5, 54.5, 'official');
INSERT INTO provider_endpoints(provider_id, app_type, url, added_at) VALUES
  ('default-claude', 'claude', 'https://backend.intelalloc.com', 1700000000001),
  ('default-codex', 'codex', 'https://backend.intelalloc.com', 1700000000002),
  ('default-desktop', 'claude-desktop', 'https://backend.intelalloc.com', 1700000000003);
PRAGMA user_version = ` + itoa(userVersion) + `;
`
	cmd := exec.Command(bin, db)
	cmd.Stdin = strings.NewReader(ddl)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("建 fixture 失败: %v: %s", err, out)
	}
	return db
}

func itoa(n int) string { return strconv.Itoa(n) }

// testCCSwitchProviders 三端 provider,key 用 canary
func testCCSwitchProviders() []CCSwitchProvider {
	const base = "https://backend.intelalloc.com"
	key, err := secret.New("ccswitch-test", ccswitchCanary)
	if err != nil {
		panic(err)
	}
	return []CCSwitchProvider{
		{ID: "relay-intelalloc-claude", AppType: contract.CCSwitchAppClaude, Name: "IntelAlloc",
			BaseURL: base, Key: key, SortIndex: 100, CreatedAt: 1753400000000, IsCurrent: true},
		{ID: "relay-intelalloc-codex", AppType: contract.CCSwitchAppCodex, Name: "IntelAlloc Codex",
			BaseURL: base, Model: "gpt-5.6-luna", Key: key, SortIndex: 101, CreatedAt: 1753400000000},
		{ID: "relay-intelalloc-claude-desktop", AppType: contract.CCSwitchAppClaudeDesktop, Name: "IntelAlloc Desktop",
			BaseURL: base, Key: key, SortIndex: 102, CreatedAt: 1753400000000},
	}
}

// sqliteDump 取指定表的 .dump(哈希比对零写入表用)
func sqliteDump(t *testing.T, bin, db, table string) string {
	t.Helper()
	out, err := exec.Command(bin, db, ".dump "+table).CombinedOutput()
	if err != nil {
		t.Fatalf(".dump %s 失败: %v: %s", table, err, out)
	}
	return string(out)
}

func makeCCSwitchImport(t *testing.T, bin, db string, providers []CCSwitchProvider) (CCSwitchSchema, ccswitchImport) {
	t.Helper()
	version, err := readCCSwitchUserVersion(context.Background(), bin, db)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := readCCSwitchLiveSchema(context.Background(), bin, db, version)
	if err != nil {
		t.Fatal(err)
	}
	templates, err := readCCSwitchTemplates(context.Background(), bin, db, schema, providers)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildCCSwitchImport(schema, providers, templates)
	if err != nil {
		t.Fatal(err)
	}
	return schema, plan
}

// 生成 SQL 可执行且 readback 一致(v11 与 v16 两套)
func TestCCSwitchImportV11V16(t *testing.T) {
	bin := requireSQLite3(t)
	for _, v := range []int{11, 16} {
		t.Run("v"+itoa(v), func(t *testing.T) {
			db := makeCCSwitchFixture(t, bin, v, false)
			providers := testCCSwitchProviders()
			schema, plan := makeCCSwitchImport(t, bin, db, providers)
			if err := applyCCSwitchSQL(context.Background(), bin, db, plan, schema); err != nil {
				t.Fatalf("导入+readback 失败: %v", err)
			}
		})
	}
}

// 幂等:二次导入后整库 dump 零差异
func TestCCSwitchImportIdempotent(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	providers := testCCSwitchProviders()
	schema, plan := makeCCSwitchImport(t, bin, db, providers)
	ctx := context.Background()
	if err := applyCCSwitchSQL(ctx, bin, db, plan, schema); err != nil {
		t.Fatal(err)
	}
	dump1 := sqliteDump(t, bin, db, "providers") + sqliteDump(t, bin, db, "provider_endpoints")
	if err := applyCCSwitchSQL(ctx, bin, db, plan, schema); err != nil {
		t.Fatal(err)
	}
	dump2 := sqliteDump(t, bin, db, "providers") + sqliteDump(t, bin, db, "provider_endpoints")
	if dump1 != dump2 {
		t.Error("二次导入后存在差异,幂等性不成立")
	}
}

func TestCCSwitchTemplateClonePreservesUnmanagedProviderColumns(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	providers := testCCSwitchProviders()
	schema, plan := makeCCSwitchImport(t, bin, db, providers)
	if err := applyCCSwitchSQL(context.Background(), bin, db, plan, schema); err != nil {
		t.Fatal(err)
	}

	templateID := map[string]string{
		contract.CCSwitchAppClaude:        "default-claude",
		contract.CCSwitchAppCodex:         "default-codex",
		contract.CCSwitchAppClaudeDesktop: "default-desktop",
	}
	unmanaged := []string{
		"app_type", "website_url", "category", "created_at", "sort_index", "notes",
		"icon", "icon_color", "meta", "is_current", "in_failover_queue", "cost_multiplier",
		"limit_daily_usd", "limit_monthly_usd", "provider_type",
	}
	encoded := make([]string, len(unmanaged))
	for i, column := range unmanaged {
		encoded[i] = "hex(quote(" + sqlIdentifier(column) + "))"
	}
	for _, provider := range providers {
		query := func(id string) string {
			out, err := queryCCSwitch(context.Background(), bin, db,
				fmt.Sprintf("SELECT json_array(%s) FROM providers WHERE id=%s;", strings.Join(encoded, ", "), sqlQuote(id)))
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(out)
		}
		if got, want := query(provider.ID), query(templateID[provider.AppType]); got != want {
			t.Fatalf("unmanaged columns drifted for %s\ngot  %s\nwant %s", provider.AppType, got, want)
		}
	}
}

func TestCCSwitchEndpointUpdatePreservesAutoincrementID(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	provider := testCCSwitchProviders()[1]
	if out, err := exec.Command(bin, db, `INSERT INTO provider_endpoints(id,provider_id,app_type,url,added_at)
VALUES(700,'relay-intelalloc-codex','codex','https://old.invalid',1);`).CombinedOutput(); err != nil {
		t.Fatalf("seed endpoint: %v: %s", err, out)
	}
	schema, plan := makeCCSwitchImport(t, bin, db, []CCSwitchProvider{provider})
	if strings.Contains(plan.SQL, "DELETE FROM provider_endpoints") {
		t.Fatal("template import still deletes endpoint rows")
	}
	if err := applyCCSwitchSQL(context.Background(), bin, db, plan, schema); err != nil {
		t.Fatal(err)
	}
	out, err := queryCCSwitch(context.Background(), bin, db,
		`SELECT json_array(id,url,added_at) FROM provider_endpoints WHERE provider_id='relay-intelalloc-codex' AND app_type='codex';`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != `[700,"https://backend.intelalloc.com",1700000000002]` {
		t.Fatalf("updated endpoint = %s", strings.TrimSpace(out))
	}
}

func TestCCSwitchNoTemplateReturnsUnconfirmedErrorWithoutWrite(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	if out, err := exec.Command(bin, db, `DELETE FROM provider_endpoints; DELETE FROM providers;`).CombinedOutput(); err != nil {
		t.Fatalf("remove templates: %v: %s", err, out)
	}
	before := sqliteDump(t, bin, db, "providers") + sqliteDump(t, bin, db, "provider_endpoints")
	engine, opts := adapterTestEngine(t)
	adapter := ccswitchAdapter{
		dbPath: db, sqlite3: bin, engine: engine,
		applyPreflight: func(context.Context) error { return nil },
	}
	result, err := adapter.Apply(context.Background(), ChangeSet{
		Client: contract.ClientCCSwitch,
		CCSwitch: &CCSwitchChange{
			Providers: testCCSwitchProviders(),
		},
	})
	if !errors.Is(err, ErrCCSwitchTemplateNotFound) {
		t.Fatalf("Apply error = %v, want template-not-found", err)
	}
	if result.TxnID != "" || result.BackupDir != "" {
		t.Fatalf("template failure entered txn: %#v", result)
	}
	after := sqliteDump(t, bin, db, "providers") + sqliteDump(t, bin, db, "provider_endpoints")
	if after != before {
		t.Fatal("template failure changed database")
	}
	if got := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCCSwitch)); got != 0 {
		t.Fatalf("template failure created %d backups", got)
	}
}

func TestCCSwitchNoCompatibleStructureReturnsUnconfirmedWithoutWrite(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	if out, err := exec.Command(bin, db, `DELETE FROM provider_endpoints WHERE app_type='codex';
DELETE FROM providers WHERE app_type='codex';
INSERT INTO providers VALUES
  ('default-codex-broken','codex','default','{"config":"model = \"old\""}',
   'https://example.invalid','broken',1,1,'broken','broken','#000000','{}',0,0,1.0,0.0,0.0,'official');`).CombinedOutput(); err != nil {
		t.Fatalf("seed incompatible template: %v: %s", err, out)
	}
	before := sqliteDump(t, bin, db, "providers") + sqliteDump(t, bin, db, "provider_endpoints")
	engine, opts := adapterTestEngine(t)
	adapter := ccswitchAdapter{
		dbPath: db, sqlite3: bin, engine: engine,
		applyPreflight: func(context.Context) error { return nil },
	}
	provider := testCCSwitchProviders()[1]
	result, err := adapter.Apply(context.Background(), ChangeSet{
		Client: contract.ClientCCSwitch,
		CCSwitch: &CCSwitchChange{
			Providers: []CCSwitchProvider{provider},
		},
	})
	if !errors.Is(err, ErrCCSwitchTemplateNotFound) {
		t.Fatalf("Apply error = %v, want template-not-found", err)
	}
	if result.TxnID != "" || result.BackupDir != "" {
		t.Fatalf("incompatible template entered txn: %#v", result)
	}
	after := sqliteDump(t, bin, db, "providers") + sqliteDump(t, bin, db, "provider_endpoints")
	if after != before {
		t.Fatal("incompatible template changed database")
	}
	if got := backupTransactionCount(t, opts.BackupRoot, string(contract.ClientCCSwitch)); got != 0 {
		t.Fatalf("incompatible template created %d backups", got)
	}
}

func TestCCSwitchCodexTemplateKeepsUnknownSettingsShape(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	provider := testCCSwitchProviders()[1]
	schema, plan := makeCCSwitchImport(t, bin, db, []CCSwitchProvider{provider})
	if err := applyCCSwitchSQL(context.Background(), bin, db, plan, schema); err != nil {
		t.Fatal(err)
	}
	out, err := queryCCSwitch(context.Background(), bin, db,
		`SELECT hex(settings_config) FROM providers WHERE id='relay-intelalloc-codex';`)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := hex.DecodeString(strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(settings, &decoded); err != nil {
		t.Fatal(err)
	}
	mcp, ok := decoded["mcp"].(map[string]any)
	if !ok || mcp["keep"] != true {
		t.Fatalf("unknown MCP settings not preserved: %#v", decoded["mcp"])
	}
	auth, ok := decoded["auth"].(map[string]any)
	if !ok || auth["keep_auth"] != "yes" {
		t.Fatalf("unknown auth settings not preserved: %#v", decoded["auth"])
	}
	config, _ := decoded["config"].(string)
	if !strings.Contains(config, `model = "gpt-5.6-luna"`) ||
		!strings.Contains(config, `base_url = "https://backend.intelalloc.com"`) {
		t.Fatalf("managed TOML values not replaced: %q", config)
	}
	for _, field := range contract.CodexBlacklist {
		if strings.Contains(config, field) {
			t.Fatalf("blacklisted field survived clone: %s", field)
		}
	}
}

// 超版本拒绝:user_version=17 时 Detect 与 schema 选择都拒绝
func TestCCSwitchOverVersionRejected(t *testing.T) {
	bin := requireSQLite3(t)
	if _, err := ccswitchSchemaFor(contract.CCSwitchMaxSchemaVersion + 1); err == nil {
		t.Error("ccswitchSchemaFor 应拒绝超上限版本")
	}
	db := makeCCSwitchFixture(t, bin, 17, false)
	a := ccswitchAdapter{dbPath: db, sqlite3: bin}
	_, err := a.Detect(context.Background())
	if !errorsIs(err, ErrCCSwitchSchemaTooNew) {
		t.Errorf("Detect 应返回 ErrCCSwitchSchemaTooNew,得到 %v", err)
	}
}

func TestCCSwitchLiveSchemaRejectsMissingRequiredColumn(t *testing.T) {
	bin := requireSQLite3(t)
	db := filepath.Join(t.TempDir(), "cc-switch.db")
	cmd := exec.Command(bin, db)
	cmd.Stdin = strings.NewReader(`CREATE TABLE providers (id TEXT); CREATE TABLE provider_endpoints (id TEXT); PRAGMA user_version=11;`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	if _, err := readCCSwitchLiveSchema(context.Background(), bin, db, 11); err == nil {
		t.Fatal("live schema missing required columns was accepted")
	}
}

// takeover fixture(proxy_config 置位)→ BLOCKED,零写入
func TestCCSwitchTakeoverFlagBlocked(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, true)
	before := sqliteDump(t, bin, db, "providers")
	a := ccswitchAdapter{dbPath: db, sqlite3: bin}
	_, err := a.Detect(context.Background())
	if !errorsIs(err, ErrCCSwitchTakeover) {
		t.Errorf("Detect 应返回 ErrCCSwitchTakeover,得到 %v", err)
	}
	if after := sqliteDump(t, bin, db, "providers"); after != before {
		t.Error("takeover 阻断后 providers 表被改动")
	}
}

// takeover(127.0.0.1:15721 监听)→ BLOCKED
func TestCCSwitchTakeoverPortBlocked(t *testing.T) {
	bin := requireSQLite3(t)
	ln, err := net.Listen("tcp", "127.0.0.1:15721")
	if err != nil {
		t.Skipf("无法监听 15721(可能已被占用): %v", err)
	}
	defer ln.Close()
	db := makeCCSwitchFixture(t, bin, 11, false)
	a := ccswitchAdapter{dbPath: db, sqlite3: bin}
	if _, err := a.Detect(context.Background()); !errorsIs(err, ErrCCSwitchTakeover) {
		t.Errorf("端口监听下 Detect 应返回 ErrCCSwitchTakeover,得到 %v", err)
	}
}

// proxy_*/model_pricing 零写入:导入前后 dump 不变
func TestCCSwitchNoTouchTablesUnchanged(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	tables := []string{"proxy_config", "model_pricing"}
	before := map[string]string{}
	for _, tb := range tables {
		before[tb] = sqliteDump(t, bin, db, tb)
	}
	providers := testCCSwitchProviders()
	schema, plan := makeCCSwitchImport(t, bin, db, providers)
	if err := applyCCSwitchSQL(context.Background(), bin, db, plan, schema); err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		if after := sqliteDump(t, bin, db, tb); after != before[tb] {
			t.Errorf("零写入表 %s 被改动", tb)
		}
	}
	// 生成的 SQL 文本本身不得提及零写入表/前缀
	for _, tb := range contract.CCSwitchNoTouchTables {
		if strings.Contains(plan.SQL, tb) {
			t.Errorf("生成 SQL 提及零写入表 %s", tb)
		}
	}
	if strings.Contains(plan.SQL, contract.CCSwitchNoTouchPrefix) {
		t.Error("生成 SQL 提及 proxy_ 前缀")
	}
}

// canary key 只出现在许可的 settings_config 字段位置:
// claude 1 处(AUTH_TOKEN)+ codex 2 处(OPENAI_API_KEY + experimental_bearer_token)+ desktop 1 处 = 4;
// 且每个含 canary 的行必须是 providers 的 INSERT 行;三个废弃字段不得出现。
func TestCCSwitchCanaryOnlyInPermittedPositions(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	_, plan := makeCCSwitchImport(t, bin, db, testCCSwitchProviders())
	sql := plan.SQL
	count := keyOccurrences(sql, testCCSwitchProviders()[0].Key)
	if count != 4 {
		t.Errorf("canary 应恰好出现 4 次(许可位置),实际 %d 次", count)
	}
	testCCSwitchProviders()[0].Key.Reveal(func(plaintext string) {
		for _, line := range strings.Split(sql, "\n") {
			if strings.Contains(line, plaintext) && !strings.HasPrefix(line, "INSERT OR REPLACE INTO providers") {
				t.Errorf("canary 出现在非许可行: %.60s...", line)
			}
		}
	})
	for _, f := range contract.CodexBlacklist {
		if strings.Contains(sql, f) {
			t.Errorf("生成 SQL 含 Codex 废弃字段 %s", f)
		}
	}
}

func TestCCSwitchGenerationRejectsCanaryOutsideSettingsConfig(t *testing.T) {
	bin := requireSQLite3(t)
	db := makeCCSwitchFixture(t, bin, 11, false)
	providers := testCCSwitchProviders()
	schema, err := readCCSwitchLiveSchema(context.Background(), bin, db, 11)
	if err != nil {
		t.Fatal(err)
	}
	templates, err := readCCSwitchTemplates(context.Background(), bin, db, schema, providers)
	if err != nil {
		t.Fatal(err)
	}
	providers[0].Key.Reveal(func(plaintext string) { providers[0].Name = "leak-" + plaintext })
	_, err = GenerateCCSwitchSQL(schema, providers, templates)
	if err == nil {
		t.Fatal("key outside settings_config was accepted")
	}
	providers[0].Key.Reveal(func(plaintext string) {
		if strings.Contains(err.Error(), plaintext) {
			t.Fatalf("error leaked canary: %v", err)
		}
	})
}

func TestCCSwitchProviderFormattingDoesNotLeakKey(t *testing.T) {
	provider := testCCSwitchProviders()[0]
	formatted := fmt.Sprintf("%+v", provider)
	provider.Key.Reveal(func(plaintext string) {
		if strings.Contains(formatted, plaintext) {
			t.Fatalf("fmt leaked key: %s", formatted)
		}
	})
}

func keyOccurrences(text string, key secret.Key) int {
	count := 0
	key.Reveal(func(plaintext string) { count = strings.Count(text, plaintext) })
	return count
}

// errorsIs 即 errors.Is 的本地别名(哨兵错误断言)
func errorsIs(err, target error) bool { return errors.Is(err, target) }
