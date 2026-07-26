package adapters

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"relay-install/internal/contract"
)

var (
	ErrCCSwitchTemplateNotFound = errors.New("ccswitch: 未找到可安全克隆的同类结构模板")
	ErrCCSwitchTemplateInvalid  = errors.New("ccswitch: 同类结构模板不可安全克隆")
)

// CCSwitchTemplate contains SQL literals in live schema order. Keeping the
// database's own quoted values lets generation clone every unmanaged column
// without inventing defaults or converting SQLite value types in Go.
type CCSwitchTemplate struct {
	AppType        string
	BaseHost       string
	ProviderValues []string
	SettingsConfig string
}

type ccswitchExpectedProvider struct {
	ID             string
	AppType        string
	BaseURL        string
	ProviderValues []string
}

type ccswitchImport struct {
	SQL       string
	Providers []ccswitchExpectedProvider
}

func readCCSwitchTemplates(ctx context.Context, bin, dbPath string, schema CCSwitchSchema, providers []CCSwitchProvider) ([]CCSwitchTemplate, error) {
	seen := make(map[string]struct{})
	templates := make([]CCSwitchTemplate, 0, len(providers))
	for _, provider := range providers {
		host, err := ccswitchBaseHost(provider.BaseURL)
		if err != nil {
			return nil, err
		}
		key := provider.AppType + "\x00" + host
		if _, ok := seen[key]; ok {
			continue
		}
		template, err := readCCSwitchTemplate(ctx, bin, dbPath, schema, provider.AppType, host)
		if err != nil {
			return nil, err
		}
		templates = append(templates, template)
		seen[key] = struct{}{}
	}
	return templates, nil
}

func readCCSwitchTemplate(ctx context.Context, bin, dbPath string, schema CCSwitchSchema, appType, host string) (CCSwitchTemplate, error) {
	encoded := make([]string, len(schema.ProviderCols))
	for i, column := range schema.ProviderCols {
		encoded[i] = "hex(quote(" + sqlIdentifier(column) + "))"
	}
	selectPrefix := fmt.Sprintf(`SELECT json_array(%s), hex(settings_config)
FROM providers
WHERE app_type=%s
	  AND typeof(settings_config)='text'
	  AND id NOT LIKE 'relay-intelalloc-%%'`, strings.Join(encoded, ", "), sqlQuote(appType))
	order := `
ORDER BY CASE WHEN lower(id)='default' OR lower(name)='default' THEN 0 ELSE 1 END,
	         sort_index, id;`

	// Prefer an existing row for the target host. If none of those rows has the
	// adopted export structure, fall back to any structurally compatible row of
	// the same app_type. Every unmanaged column still comes from that live row.
	queries := []string{
		selectPrefix + fmt.Sprintf("\n  AND instr(lower(settings_config), lower(%s)) > 0", sqlQuote(host)) + order,
		selectPrefix + order,
	}
	for _, query := range queries {
		candidates, err := readCCSwitchTemplateCandidates(ctx, bin, dbPath, schema, appType, host, query)
		if err != nil {
			return CCSwitchTemplate{}, err
		}
		for _, candidate := range candidates {
			if ccswitchTemplateStructurallyCompatible(candidate.SettingsConfig, appType) {
				return candidate, nil
			}
		}
	}
	return CCSwitchTemplate{}, fmt.Errorf("%w: app_type=%s host=%s (无共同结构候选)", ErrCCSwitchTemplateNotFound, appType, host)
}

func readCCSwitchTemplateCandidates(ctx context.Context, bin, dbPath string, schema CCSwitchSchema, appType, host, query string) ([]CCSwitchTemplate, error) {
	out, err := queryCCSwitch(ctx, bin, dbPath, query)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(out, "\r\n")
	if trimmed == "" {
		return nil, nil
	}
	candidates := make([]CCSwitchTemplate, 0, strings.Count(trimmed, "\n")+1)
	for _, line := range strings.Split(trimmed, "\n") {
		parts := strings.SplitN(strings.TrimSuffix(line, "\r"), "\x1f", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: 模板查询结果格式错误", ErrCCSwitchTemplateInvalid)
		}
		var valueHex []string
		if err := json.Unmarshal([]byte(parts[0]), &valueHex); err != nil || len(valueHex) != len(schema.ProviderCols) {
			return nil, fmt.Errorf("%w: 模板列数量错误", ErrCCSwitchTemplateInvalid)
		}
		values := make([]string, len(valueHex))
		for i, encodedValue := range valueHex {
			decoded, err := hex.DecodeString(encodedValue)
			if err != nil {
				return nil, fmt.Errorf("%w: 模板列编码错误", ErrCCSwitchTemplateInvalid)
			}
			values[i] = string(decoded)
		}
		settings, err := hex.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("%w: settings_config 编码错误", ErrCCSwitchTemplateInvalid)
		}
		if _, err := contract.ParseOrderedJSONObject(settings); err != nil {
			continue
		}
		candidates = append(candidates, CCSwitchTemplate{
			AppType:        appType,
			BaseHost:       host,
			ProviderValues: values,
			SettingsConfig: string(settings),
		})
	}
	return candidates, nil
}

func ccswitchTemplateStructurallyCompatible(settings, appType string) bool {
	root, err := contract.ParseOrderedJSONObject([]byte(settings))
	if err != nil {
		return false
	}
	stringField := func(object contract.OrderedJSONObject, name string) bool {
		raw, ok := object.Get(name)
		if !ok {
			return false
		}
		var value string
		return json.Unmarshal(raw, &value) == nil
	}
	switch appType {
	case contract.CCSwitchAppClaude, contract.CCSwitchAppClaudeDesktop:
		rawEnv, ok := root.Get("env")
		if !ok {
			return false
		}
		env, err := contract.ParseOrderedJSONObject(rawEnv)
		return err == nil && stringField(env, "ANTHROPIC_BASE_URL") && stringField(env, "ANTHROPIC_AUTH_TOKEN")
	case contract.CCSwitchAppCodex:
		rawAuth, ok := root.Get("auth")
		if !ok {
			return false
		}
		auth, err := contract.ParseOrderedJSONObject(rawAuth)
		if err != nil || !stringField(auth, "OPENAI_API_KEY") {
			return false
		}
		rawConfig, ok := root.Get("config")
		if !ok {
			return false
		}
		var config string
		if json.Unmarshal(rawConfig, &config) != nil {
			return false
		}
		for _, key := range []string{"base_url", "model", "experimental_bearer_token"} {
			if _, err := replaceTOMLStringAssignment(config, key, "relay-install-structure-check"); err != nil {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func buildCCSwitchImport(schema CCSwitchSchema, providers []CCSwitchProvider, templates []CCSwitchTemplate) (ccswitchImport, error) {
	if len(providers) == 0 {
		return ccswitchImport{}, errors.New("ccswitch: 空 provider 列表")
	}
	templateByKey := make(map[string]CCSwitchTemplate, len(templates))
	for _, template := range templates {
		templateByKey[template.AppType+"\x00"+template.BaseHost] = template
	}

	var b strings.Builder
	b.WriteString("PRAGMA foreign_keys=OFF;\nBEGIN TRANSACTION;\n")
	plan := ccswitchImport{Providers: make([]ccswitchExpectedProvider, 0, len(providers))}
	for _, provider := range providers {
		if err := validateCCSwitchProvider(provider); err != nil {
			return ccswitchImport{}, err
		}
		host, err := ccswitchBaseHost(provider.BaseURL)
		if err != nil {
			return ccswitchImport{}, err
		}
		template, ok := templateByKey[provider.AppType+"\x00"+host]
		if !ok {
			return ccswitchImport{}, fmt.Errorf("%w: app_type=%s host=%s", ErrCCSwitchTemplateNotFound, provider.AppType, host)
		}
		if len(template.ProviderValues) != len(schema.ProviderCols) {
			return ccswitchImport{}, fmt.Errorf("%w: app_type=%s 列数量不一致", ErrCCSwitchTemplateInvalid, provider.AppType)
		}

		values := append([]string(nil), template.ProviderValues...)
		settings := ""
		var settingsErr error
		provider.Key.Reveal(func(plaintext string) {
			settings, settingsErr = cloneCCSwitchSettings(template.SettingsConfig, provider, plaintext)
		})
		if settingsErr != nil {
			return ccswitchImport{}, settingsErr
		}
		for column, value := range map[string]string{
			"id":              sqlQuote(provider.ID),
			"name":            sqlQuote(provider.Name),
			"settings_config": sqlQuote(settings),
		} {
			index, ok := columnIndex(schema.ProviderCols, column)
			if !ok {
				return ccswitchImport{}, fmt.Errorf("%w: providers 缺少 %s", ErrCCSwitchTemplateInvalid, column)
			}
			values[index] = value
		}
		createdAtIndex, ok := columnIndex(schema.ProviderCols, "created_at")
		if !ok {
			return ccswitchImport{}, fmt.Errorf("%w: providers 缺少 created_at", ErrCCSwitchTemplateInvalid)
		}

		fmt.Fprintf(&b, "INSERT OR REPLACE INTO providers (%s) VALUES (%s);\n",
			joinSQLIdentifiers(schema.ProviderCols), strings.Join(values, ", "))
		fmt.Fprintf(&b, "UPDATE provider_endpoints SET url=%s, added_at=%s WHERE provider_id=%s AND app_type=%s;\n",
			sqlQuote(provider.BaseURL), values[createdAtIndex], sqlQuote(provider.ID), sqlQuote(provider.AppType))
		endpointValues, err := endpointInsertValues(schema.EndpointCols, provider, values[createdAtIndex])
		if err != nil {
			return ccswitchImport{}, err
		}
		fmt.Fprintf(&b, "INSERT INTO provider_endpoints (%s) SELECT %s WHERE NOT EXISTS (SELECT 1 FROM provider_endpoints WHERE provider_id=%s AND app_type=%s);\n",
			joinSQLIdentifiers(schema.EndpointCols), strings.Join(endpointValues, ", "), sqlQuote(provider.ID), sqlQuote(provider.AppType))
		plan.Providers = append(plan.Providers, ccswitchExpectedProvider{
			ID: provider.ID, AppType: provider.AppType, BaseURL: provider.BaseURL, ProviderValues: values,
		})
	}
	b.WriteString("COMMIT;\n")
	plan.SQL = b.String()
	if err := validateCCSwitchGeneratedSecrets(plan.SQL, providers); err != nil {
		return ccswitchImport{}, err
	}
	return plan, nil
}

func cloneCCSwitchSettings(template string, provider CCSwitchProvider, plaintext string) (string, error) {
	if plaintext == "" {
		return "", errors.New("ccswitch: 空 key 拒绝生成")
	}
	root, err := contract.ParseOrderedJSONObject([]byte(template))
	if err != nil {
		return "", fmt.Errorf("%w: settings_config JSON: %v", ErrCCSwitchTemplateInvalid, err)
	}
	setString := func(object *contract.OrderedJSONObject, name, value string) error {
		if _, ok := object.Get(name); !ok {
			return fmt.Errorf("%w: settings_config 缺少 %s", ErrCCSwitchTemplateInvalid, name)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		object.Set(name, encoded)
		return nil
	}

	switch provider.AppType {
	case contract.CCSwitchAppClaude, contract.CCSwitchAppClaudeDesktop:
		rawEnv, ok := root.Get("env")
		if !ok {
			return "", fmt.Errorf("%w: settings_config 缺少 env", ErrCCSwitchTemplateInvalid)
		}
		env, err := contract.ParseOrderedJSONObject(rawEnv)
		if err != nil {
			return "", fmt.Errorf("%w: env 不是 JSON 对象", ErrCCSwitchTemplateInvalid)
		}
		if err := setString(&env, "ANTHROPIC_BASE_URL", provider.BaseURL); err != nil {
			return "", err
		}
		if err := setString(&env, "ANTHROPIC_AUTH_TOKEN", plaintext); err != nil {
			return "", err
		}
		root.Set("env", env.Marshal())
	case contract.CCSwitchAppCodex:
		if provider.Model == "" {
			return "", fmt.Errorf("%w: codex 模型为空", ErrCCSwitchTemplateInvalid)
		}
		rawAuth, ok := root.Get("auth")
		if !ok {
			return "", fmt.Errorf("%w: settings_config 缺少 auth", ErrCCSwitchTemplateInvalid)
		}
		auth, err := contract.ParseOrderedJSONObject(rawAuth)
		if err != nil {
			return "", fmt.Errorf("%w: auth 不是 JSON 对象", ErrCCSwitchTemplateInvalid)
		}
		if err := setString(&auth, "OPENAI_API_KEY", plaintext); err != nil {
			return "", err
		}
		rawConfig, ok := root.Get("config")
		if !ok {
			return "", fmt.Errorf("%w: settings_config 缺少 config", ErrCCSwitchTemplateInvalid)
		}
		var config string
		if err := json.Unmarshal(rawConfig, &config); err != nil {
			return "", fmt.Errorf("%w: config 不是字符串", ErrCCSwitchTemplateInvalid)
		}
		config = removeTOMLAssignments(config, contract.CodexBlacklist)
		for _, replacement := range []struct{ key, value string }{
			{"base_url", provider.BaseURL},
			{"model", provider.Model},
			{"experimental_bearer_token", plaintext},
		} {
			config, err = replaceTOMLStringAssignment(config, replacement.key, replacement.value)
			if err != nil {
				return "", err
			}
		}
		for _, field := range contract.CodexBlacklist {
			if strings.Contains(config, field) {
				return "", fmt.Errorf("%w: codex 模板含黑名单字段 %s", ErrCCSwitchTemplateInvalid, field)
			}
		}
		encodedConfig, err := json.Marshal(config)
		if err != nil {
			return "", err
		}
		root.Set("auth", auth.Marshal())
		root.Set("config", encodedConfig)
	default:
		return "", fmt.Errorf("ccswitch: 未知 app_type %q", provider.AppType)
	}

	merged := root.Marshal()
	var compact bytes.Buffer
	if err := json.Compact(&compact, merged); err != nil {
		return "", fmt.Errorf("%w: settings_config 压缩失败", ErrCCSwitchTemplateInvalid)
	}
	merged = compact.Bytes()
	wantCount := 1
	if provider.AppType == contract.CCSwitchAppCodex {
		wantCount = 2
	}
	count, err := contract.CountJSONStringsContaining(merged, plaintext)
	if err != nil {
		return "", fmt.Errorf("%w: settings_config 泄漏扫描失败", ErrCCSwitchTemplateInvalid)
	}
	if count != wantCount {
		return "", fmt.Errorf("ccswitch: selected key appears %d times; want %d permitted settings_config locations", count, wantCount)
	}
	return string(merged), nil
}

func replaceTOMLStringAssignment(config, key, value string) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	lines := strings.SplitAfter(config, "\n")
	matches := 0
	for i, line := range lines {
		body, newline := line, ""
		if strings.HasSuffix(body, "\n") {
			body, newline = strings.TrimSuffix(body, "\n"), "\n"
		}
		carriage := ""
		if strings.HasSuffix(body, "\r") {
			body, carriage = strings.TrimSuffix(body, "\r"), "\r"
		}
		trimmed := strings.TrimLeft(body, " \t")
		if !strings.HasPrefix(trimmed, key) {
			continue
		}
		rest := trimmed[len(key):]
		if len(rest) > 0 && rest[0] != ' ' && rest[0] != '\t' && rest[0] != '=' {
			continue
		}
		equal := strings.IndexByte(rest, '=')
		if equal < 0 {
			continue
		}
		valueStart := len(body) - len(rest) + equal + 1
		for valueStart < len(body) && (body[valueStart] == ' ' || body[valueStart] == '\t') {
			valueStart++
		}
		if valueStart >= len(body) || body[valueStart] != '"' {
			return "", fmt.Errorf("%w: codex config 的 %s 不是基础字符串", ErrCCSwitchTemplateInvalid, key)
		}
		valueEnd, err := tomlStringEnd(body, valueStart)
		if err != nil {
			return "", fmt.Errorf("%w: codex config 的 %s 无法解析", ErrCCSwitchTemplateInvalid, key)
		}
		lines[i] = body[:valueStart] + string(encoded) + body[valueEnd:] + carriage + newline
		matches++
	}
	if matches != 1 {
		return "", fmt.Errorf("%w: codex config 的 %s 出现 %d 次", ErrCCSwitchTemplateInvalid, key, matches)
	}
	return strings.Join(lines, ""), nil
}

func removeTOMLAssignments(config string, keys []string) string {
	remove := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		remove[key] = struct{}{}
	}
	lines := strings.SplitAfter(config, "\n")
	kept := lines[:0]
	for _, line := range lines {
		body := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		trimmed := strings.TrimLeft(body, " \t")
		equal := strings.IndexByte(trimmed, '=')
		if equal >= 0 {
			key := strings.TrimSpace(trimmed[:equal])
			if _, ok := remove[key]; ok {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "")
}

func tomlStringEnd(line string, start int) (int, error) {
	escaped := false
	for i := start + 1; i < len(line); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch line[i] {
		case '\\':
			escaped = true
		case '"':
			return i + 1, nil
		}
	}
	return 0, errors.New("unterminated string")
}

func endpointInsertValues(columns []string, provider CCSwitchProvider, addedAt string) ([]string, error) {
	values := make([]string, len(columns))
	for i, column := range columns {
		switch column {
		case "id":
			values[i] = "NULL"
		case "provider_id":
			values[i] = sqlQuote(provider.ID)
		case "app_type":
			values[i] = sqlQuote(provider.AppType)
		case "url":
			values[i] = sqlQuote(provider.BaseURL)
		case "added_at":
			values[i] = addedAt
		default:
			return nil, fmt.Errorf("%w: 未知 endpoint 列 %s", ErrCCSwitchTemplateInvalid, column)
		}
	}
	return values, nil
}

func ccswitchBaseHost(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("ccswitch: base URL 缺少 host")
	}
	return strings.ToLower(parsed.Hostname()), nil
}

func columnIndex(columns []string, want string) (int, bool) {
	for i, column := range columns {
		if column == want {
			return i, true
		}
	}
	return 0, false
}

func sqlIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func joinSQLIdentifiers(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = sqlIdentifier(name)
	}
	return strings.Join(quoted, ", ")
}
