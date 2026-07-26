package adapters

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexDetectVersionGate(t *testing.T) {
	cases := []struct {
		name    string
		version string
		wantErr bool
	}{
		{"supported_plain", "0.145.0", false},
		{"supported_prefixed", "codex-cli 0.145.0", false},
		{"too_old", "codex-cli 0.133.9", true},
		{"minimum_prerelease", "codex-cli 0.134.0-rc.1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := codexAdapter{
				path:      "/test/codex",
				codexHome: t.TempDir(),
				commandOutput: func(context.Context, string, ...string) ([]byte, error) {
					return []byte(tc.version + "\n"), nil
				},
			}
			res, err := a.Detect(context.Background())
			if tc.wantErr {
				if !errors.Is(err, ErrCodexVersionTooOld) {
					t.Fatalf("Detect error = %v, want ErrCodexVersionTooOld", err)
				}
				return
			}
			if err != nil || res.Version != "0.145.0" {
				t.Fatalf("Detect = %#v, %v", res, err)
			}
		})
	}
}

func TestCodexDetectDefaultsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", home)
	a := codexAdapter{
		path: "/test/codex",
		commandOutput: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("0.145.0\n"), nil
		},
	}
	res, err := a.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "CODEX_HOME=" + filepath.Join(home, ".codex")
	if !strings.Contains(res.Detail, want) {
		t.Fatalf("Detect Detail = %q, want %q", res.Detail, want)
	}
}

func TestCodexDetectReportsCODEXHOME(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	a := codexAdapter{
		path: "/test/codex",
		commandOutput: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("codex-cli 0.145.0\n"), nil
		},
	}
	res, err := a.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Detail, "CODEX_HOME="+os.Getenv("CODEX_HOME")) {
		t.Fatalf("Detect 未报告 CODEX_HOME: %q", res.Detail)
	}
}

func TestCodexDetectRejectsUnparseableVersion(t *testing.T) {
	a := codexAdapter{
		path:      "/test/codex",
		codexHome: t.TempDir(),
		commandOutput: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("codex-cli development build\n"), nil
		},
	}
	_, err := a.Detect(context.Background())
	if !errors.Is(err, ErrCodexVersionParse) {
		t.Fatalf("Detect error = %v, want ErrCodexVersionParse", err)
	}
}

type strictConfigRunnerStub struct {
	called bool
	err    error
	binary string
	path   string
}

func (s *strictConfigRunnerStub) StrictConfig(_ context.Context, binary, path string) error {
	s.called = true
	s.binary = binary
	s.path = path
	return s.err
}

func TestCodexValidateUsesInjectableStrictConfigRunner(t *testing.T) {
	content, err := GenerateCodexConfig(testCodexConfig(t, "alpha", CodexProfileGuarded))
	if err != nil {
		t.Fatal(err)
	}
	runner := &strictConfigRunnerStub{}
	a := codexAdapter{path: "/test/codex", codexHome: t.TempDir(), strictConfigRunner: runner}
	change, err := a.PlanCodexConfig(testCodexConfig(t, "alpha", CodexProfileGuarded))
	if err != nil {
		t.Fatal(err)
	}
	change.Content = content
	if err := a.Validate(context.Background(), ChangeSet{Client: a.ID(), Changes: []Change{change}}); err != nil {
		t.Fatal(err)
	}
	if !runner.called {
		t.Fatal("strict-config runner 未被调用")
	}
	if runner.binary != "/test/codex" || runner.path != change.Point.PathHint {
		t.Fatalf("strict runner 参数 = (%q, %q), want (%q, %q)", runner.binary, runner.path, "/test/codex", change.Point.PathHint)
	}
}
