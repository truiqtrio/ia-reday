package contract

import (
	"strings"
	"testing"
)

func TestValidateManagedCodexTOML(t *testing.T) {
	valid := []byte(CodexManagedHeader +
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
		CodexProviderFieldBearerToken + ` = "sk-canary"` + "\n")
	if err := ValidateManagedCodexTOML(valid); err != nil {
		t.Fatalf("valid managed TOML rejected: %v", err)
	}
	for _, broken := range [][]byte{
		[]byte(CodexManagedHeader + `model = "unterminated` + "\n"),
		[]byte(strings.Replace(string(valid), `model = "runtime-model"`, `model = "unterminated`, 1)),
		append(append([]byte(nil), valid...), []byte("unknown = true\n")...),
		[]byte(strings.Replace(string(valid), `runtime-model`, "runtime\x7fmodel", 1)),
	} {
		if err := ValidateManagedCodexTOML(broken); err == nil {
			t.Fatalf("broken managed TOML accepted: %q", broken)
		}
	}
}

func TestCountJSONStringsContainingIncludesEscapesAndDuplicates(t *testing.T) {
	data := []byte(`{"first":"sk-canary","nested":{"second":"prefix-sk\u002dcanary"}}`)
	count, err := CountJSONStringsContaining(data, "sk-canary")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
}
