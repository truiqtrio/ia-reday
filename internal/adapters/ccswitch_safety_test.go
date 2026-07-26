package adapters

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type ccswitchCommandCall struct {
	name string
	args []string
}

type fakeCCSwitchExitError int

func (code fakeCCSwitchExitError) Error() string { return fmt.Sprintf("exit status %d", code) }
func (code fakeCCSwitchExitError) ExitCode() int { return int(code) }

func TestCCSwitchMacSafetyChecksUseExpectedCommands(t *testing.T) {
	var calls []ccswitchCommandCall
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, ccswitchCommandCall{name: name, args: append([]string(nil), args...)})
		switch name {
		case "lsof":
			return []byte("COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME\ncc-switch 42 user 3u IPv4 0x0 0t0 TCP 127.0.0.1:15721 (LISTEN)\n"), nil
		case "pgrep":
			return []byte("42 cc-switch\n"), nil
		default:
			return nil, fmt.Errorf("unexpected command %q", name)
		}
	}

	listening, err := port15721ListeningForOS(context.Background(), "darwin", runner)
	if err != nil || !listening {
		t.Fatalf("macOS port check = %v, %v; want true, nil", listening, err)
	}
	running, err := ccSwitchProcessRunningForOS(context.Background(), "darwin", runner)
	if err != nil || !running {
		t.Fatalf("macOS process check = %v, %v; want true, nil", running, err)
	}
	if got, want := formatCCSwitchCalls(calls), "lsof -nP -iTCP:15721 -sTCP:LISTEN\npgrep -fl cc-switch"; got != want {
		t.Fatalf("commands = %q; want %q", got, want)
	}
}

func TestCCSwitchMacNoMatchesAreSafe(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, fakeCCSwitchExitError(1)
	}
	if listening, err := port15721ListeningForOS(context.Background(), "darwin", runner); err != nil || listening {
		t.Fatalf("macOS no-listener check = %v, %v", listening, err)
	}
	if running, err := ccSwitchProcessRunningForOS(context.Background(), "darwin", runner); err != nil || running {
		t.Fatalf("macOS no-process check = %v, %v", running, err)
	}
}

func TestCCSwitchWindowsSafetyChecksParseRunnerOutput(t *testing.T) {
	var calls []ccswitchCommandCall
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, ccswitchCommandCall{name: name, args: append([]string(nil), args...)})
		switch name {
		case "netstat":
			return []byte("  TCP    [::1]:15721         [::]:0              LISTENING       4242\r\n"), nil
		case "tasklist":
			return []byte("Image Name                     PID Session Name        Session#    Mem Usage\r\ncc-switch.exe                 4242 Console                    1     12,000 K\r\n"), nil
		default:
			return nil, fmt.Errorf("unexpected command %q", name)
		}
	}

	listening, err := port15721ListeningForOS(context.Background(), "windows", runner)
	if err != nil || !listening {
		t.Fatalf("Windows port check = %v, %v; want true, nil", listening, err)
	}
	running, err := ccSwitchProcessRunningForOS(context.Background(), "windows", runner)
	if err != nil || !running {
		t.Fatalf("Windows process check = %v, %v; want true, nil", running, err)
	}
	if got, want := formatCCSwitchCalls(calls), "netstat -ano\ntasklist"; got != want {
		t.Fatalf("commands = %q; want %q", got, want)
	}
}

func TestCCSwitchSafetyOutputParsersRejectNearMatches(t *testing.T) {
	if parseLsofListening([]byte("COMMAND PID USER\n")) {
		t.Fatal("lsof header alone reported a listener")
	}
	if parsePgrepCCSwitch(nil) {
		t.Fatal("empty pgrep output reported a process")
	}
	if parseWindowsNetstatListening([]byte("  TCP    127.0.0.1:157210   0.0.0.0:0   LISTENING   9\n")) {
		t.Fatal("near-matching Windows port reported a listener")
	}
	if parseWindowsNetstatListening([]byte("  TCP    127.0.0.1:15721    0.0.0.0:0   ESTABLISHED 9\n")) {
		t.Fatal("non-listening Windows socket reported a listener")
	}
	if parseWindowsTasklistCCSwitch([]byte("cc-switch-helper.exe 123 Console 1 10 K\n")) {
		t.Fatal("near-matching Windows process reported as cc-switch")
	}
}

func TestCCSwitchSafetyChecksRejectUnknownPlatform(t *testing.T) {
	_, err := port15721ListeningForOS(context.Background(), "freebsd", nil)
	if err == nil || !strings.Contains(err.Error(), "freebsd") {
		t.Fatalf("unknown-platform port check error = %v", err)
	}
	_, err = ccSwitchProcessRunningForOS(context.Background(), "freebsd", nil)
	if err == nil || !strings.Contains(err.Error(), "freebsd") {
		t.Fatalf("unknown-platform process check error = %v", err)
	}
}

func formatCCSwitchCalls(calls []ccswitchCommandCall) string {
	lines := make([]string, 0, len(calls))
	for _, call := range calls {
		lines = append(lines, strings.TrimSpace(call.name+" "+strings.Join(call.args, " ")))
	}
	return strings.Join(lines, "\n")
}
