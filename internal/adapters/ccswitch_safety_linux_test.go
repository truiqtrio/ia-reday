//go:build linux

package adapters

import (
	"context"
	"fmt"
	"testing"
)

func TestCCSwitchLinuxSafetyChecksDoNotUseCommandRunner(t *testing.T) {
	called := false
	runner := func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, fmt.Errorf("Linux must not invoke the command runner")
	}
	if _, err := port15721ListeningForOS(context.Background(), "linux", runner); err != nil {
		t.Fatalf("Linux port check failed: %v", err)
	}
	if _, err := ccSwitchProcessRunningForOS(context.Background(), "linux", runner); err != nil {
		t.Fatalf("Linux process check failed: %v", err)
	}
	if called {
		t.Fatal("Linux safety checks invoked the command runner")
	}
}
