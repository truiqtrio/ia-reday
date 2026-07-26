package contract

import (
	"strings"
	"testing"
)

func TestValidateChangeJSONRoundTripBlacklistAndSecret(t *testing.T) {
	const canary = "sk-txn-canary"
	allowed := [][]string{{ClaudeCodeFieldEnv, ClaudeCodeEnvAuthToken}}
	valid := []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-txn-canary","OTHER":{"nested":true}},"permissions":{"allow":["Read"]}}`)
	if err := ValidateChange(ParserJSON, valid, nil, canary, allowed); err != nil {
		t.Fatalf("valid JSON rejected: %v", err)
	}
	for _, invalid := range [][]byte{
		[]byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-txn-canary","OTHER":"sk-txn-canary"}}`),
		[]byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-txn-canary"},"bad":1,"bad":2}`),
		[]byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-txn-canary"},"network_access":true}`),
	} {
		if err := ValidateChange(ParserJSON, invalid, []string{"network_access"}, canary, allowed); err == nil {
			t.Fatalf("invalid JSON accepted: %s", invalid)
		}
	}
}

func TestValidateChangeManagedTOMLSecretAndBlacklist(t *testing.T) {
	const canary = "sk-txn-canary"
	valid := managedTOMLFixture(canary)
	allowed := [][]string{{CodexFieldModelProviders, CodexProviderIntelalloc, CodexProviderFieldBearerToken}}
	if err := ValidateChange(ParserTOML, valid, CodexBlacklist, canary, allowed); err != nil {
		t.Fatalf("valid TOML rejected: %v", err)
	}
	invalid := append(append([]byte(nil), valid...), []byte("network_access = true\n")...)
	if err := ValidateChange(ParserTOML, invalid, CodexBlacklist, canary, allowed); err == nil {
		t.Fatal("blacklisted TOML was accepted")
	}
}

func managedTOMLFixture(token string) []byte {
	return []byte(CodexManagedHeader +
		CodexFieldModelProvider + ` = "intelalloc"` + "\n" +
		CodexFieldModel + ` = "runtime-model"` + "\n" +
		CodexFieldReviewModel + ` = "runtime-review"` + "\n" +
		CodexFieldModelReasoningEffort + ` = "high"` + "\n" +
		CodexFieldPlanModeReasoningEffort + ` = "xhigh"` + "\n" +
		CodexFieldApprovalPolicy + ` = "on-request"` + "\n" +
		CodexFieldSandboxMode + ` = "workspace-write"` + "\n\n[" +
		CodexFieldModelProviders + "." + CodexProviderIntelalloc + "]\n" +
		CodexProviderFieldName + ` = "intelalloc"` + "\n" +
		CodexProviderFieldBaseURL + ` = "https://relay.example.test"` + "\n" +
		CodexProviderFieldWireAPI + ` = "responses"` + "\n" +
		CodexProviderFieldBearerToken + ` = "` + strings.ReplaceAll(token, `"`, `\"`) + `"` + "\n")
}
