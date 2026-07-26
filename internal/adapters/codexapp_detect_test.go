package adapters

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCodexAppDetectorNoMatches(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	detector := CodexAppDetector{
		HomeDir: func() (string, error) { return home, nil },
		GOOS:    "darwin",
	}

	got, err := detector.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, CodexAppDetection{}) {
		t.Fatalf("Detect = %#v, want no evidence", got)
	}
}

func TestCodexAppDetectorFindsSortedIsolatedHomesAndIgnoresNonDirectories(t *testing.T) {
	home := t.TempDir()
	codexRoot := filepath.Join(home, ".codex")
	for _, name := range []string{"PATH_zulu", "PATH_alpha", "not-a-path"} {
		if err := os.MkdirAll(filepath.Join(codexRoot, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(codexRoot, "PATH_file"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	detector := CodexAppDetector{
		HomeDir: func() (string, error) { return home, nil },
		GOOS:    "darwin",
	}
	got, err := detector.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(codexRoot, "PATH_alpha"),
		filepath.Join(codexRoot, "PATH_zulu"),
	}
	if !reflect.DeepEqual(got.IsolatedCodexHomes, want) {
		t.Fatalf("IsolatedCodexHomes = %#v, want %#v", got.IsolatedCodexHomes, want)
	}
	if got.InstallCandidates != nil {
		t.Fatalf("InstallCandidates = %#v, want nil without an app bundle", got.InstallCandidates)
	}
}

func TestCodexAppDetectorDoesNothingOutsideMacOS(t *testing.T) {
	detector := CodexAppDetector{
		GOOS: "linux",
		HomeDir: func() (string, error) {
			t.Fatal("non-macOS detection must not inspect the home directory")
			return "", nil
		},
	}
	got, err := detector.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, CodexAppDetection{}) {
		t.Fatalf("Detect = %#v, want no desktop evidence", got)
	}
}

func TestCodexAppDetectorFindsMacOSInstallCandidates(t *testing.T) {
	home := t.TempDir()
	userApp := filepath.Join(home, "Applications", "Codex.app")
	if err := os.MkdirAll(userApp, 0o700); err != nil {
		t.Fatal(err)
	}
	systemApp := "/Applications/Codex.app"
	detector := CodexAppDetector{
		HomeDir: func() (string, error) { return home, nil },
		GOOS:    "darwin",
		Stat: func(path string) (fs.FileInfo, error) {
			if path == systemApp {
				return os.Stat(userApp)
			}
			return os.Stat(path)
		},
	}

	got, err := detector.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{systemApp, userApp}
	if !reflect.DeepEqual(got.InstallCandidates, want) {
		t.Fatalf("InstallCandidates = %#v, want %#v", got.InstallCandidates, want)
	}
}

func TestCodexAppDetectorHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	detector := CodexAppDetector{
		HomeDir: func() (string, error) {
			return "", errors.New("HomeDir must not be called after cancellation")
		},
	}
	_, err := detector.Detect(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Detect error = %v, want context.Canceled", err)
	}
}
