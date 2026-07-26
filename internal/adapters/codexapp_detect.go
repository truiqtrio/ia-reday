package adapters

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// CodexAppDetection is read-only evidence for future Codex App adapter wiring.
// IsolatedCodexHomes are the PATH_* CODEX_HOME directories observed under
// ~/.codex. InstallCandidates are conventional macOS application bundle paths.
type CodexAppDetection struct {
	IsolatedCodexHomes []string
	InstallCandidates  []string
}

// CodexAppDetector discovers evidence without creating, modifying, or starting
// anything. Its function fields are test seams; nil fields use the OS defaults.
type CodexAppDetector struct {
	HomeDir func() (string, error)
	ReadDir func(string) ([]os.DirEntry, error)
	Stat    func(string) (fs.FileInfo, error)
	GOOS    string
}

// Detect finds isolated Codex App homes and conventional macOS app bundles.
// A missing ~/.codex directory or conventional app bundle is an empty result,
// not an error. Any other filesystem failure is returned to the caller.
func (d CodexAppDetector) Detect(ctx context.Context) (CodexAppDetection, error) {
	if err := ctx.Err(); err != nil {
		return CodexAppDetection{}, err
	}
	goos := d.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "darwin" {
		return CodexAppDetection{}, nil
	}

	homeDir := d.HomeDir
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil {
		return CodexAppDetection{}, fmt.Errorf("codexapp: resolve home: %w", err)
	}
	readDir := d.ReadDir
	if readDir == nil {
		readDir = os.ReadDir
	}
	stat := d.Stat
	if stat == nil {
		stat = os.Stat
	}

	result := CodexAppDetection{}
	codexRoot := filepath.Join(home, ".codex")
	entries, err := readDir(codexRoot)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return result, fmt.Errorf("codexapp: read %s: %w", codexRoot, err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return CodexAppDetection{}, err
		}
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "PATH_") {
			result.IsolatedCodexHomes = append(result.IsolatedCodexHomes, filepath.Join(codexRoot, entry.Name()))
		}
	}
	result.IsolatedCodexHomes = sortedUnique(result.IsolatedCodexHomes)

	for _, candidate := range []string{
		"/Applications/Codex.app",
		filepath.Join(home, "Applications", "Codex.app"),
	} {
		if err := ctx.Err(); err != nil {
			return CodexAppDetection{}, err
		}
		info, err := stat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return CodexAppDetection{}, fmt.Errorf("codexapp: stat %s: %w", candidate, err)
		}
		if info.IsDir() {
			result.InstallCandidates = append(result.InstallCandidates, candidate)
		}
	}
	result.InstallCandidates = sortedUnique(result.InstallCandidates)
	return result, nil
}

func sortedUnique(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	out := paths[:0]
	for _, path := range paths {
		if len(out) == 0 || out[len(out)-1] != path {
			out = append(out, path)
		}
	}
	return out
}
