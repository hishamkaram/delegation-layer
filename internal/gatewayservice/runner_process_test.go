package gatewayservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

func TestMain(m *testing.M) {
	if code, handled := execution.RunCommandOwnerMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	if len(os.Args) == 3 && os.Args[1] == "--gateway-service-runner-callback" {
		if os.Args[2] == "large-output" {
			if _, err := io.WriteString(os.Stdout, strings.Repeat("private-print-canary", maxCommandOutput)); err != nil {
				os.Exit(26)
			}
			if _, err := io.WriteString(os.Stderr, "private-stderr-canary"); err != nil {
				os.Exit(27)
			}
			os.Exit(23)
		}
		directory, err := os.Getwd()
		if err != nil {
			os.Exit(24)
		}
		if _, err = fmt.Fprintln(os.Stdout, directory); err != nil {
			os.Exit(25)
		}
		if os.Args[2] == "failure" {
			os.Exit(23)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestNativeRunnerProbeDiscardsLargeOutputAndJoinsExit(t *testing.T) {
	raw, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := verifiedexec.LocatePath(raw)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewNativeRunner(executable, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	args := []string{"--gateway-service-runner-callback", "large-output"}
	result, err := runner.Run(ctx, executable, args)
	if err == nil || result.ExitCode != 23 || len(result.Stdout) != maxCommandOutput {
		t.Fatalf("captured command lost its output bound or joined exit: bytes=%d exit=%d err=%v", len(result.Stdout), result.ExitCode, err)
	}
	code, err := runner.Probe(ctx, executable, args)
	if err != nil || code != 23 {
		t.Fatalf("discarded output caused overflow or lost joined exit: code=%d err=%v", code, err)
	}
	if _, err = runner.Probe(context.Background(), executable, args); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("unbounded probe accepted: %v", err)
	}
	cancel()
	if _, err = runner.Probe(ctx, executable, args); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe accepted: %v", err)
	}
}

func TestNativeRunnerUsesAbsoluteControlDirectoryAndJoinsExit(t *testing.T) {
	rawExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := verifiedexec.LocatePath(rawExecutable)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewNativeRunner(executable, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		exitCode int
	}{{"success", 0}, {"failure", 23}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			result, runErr := runner.Run(ctx, executable, []string{"--gateway-service-runner-callback", tc.name})
			if runErr != nil {
				t.Fatalf("real isolated callback failed: %v", runErr)
			}
			if result.ExitCode != tc.exitCode || string(result.Stdout) != "/\n" {
				t.Fatalf("callback directory/output or joined exit changed: exit=%d stdout=%q", result.ExitCode, result.Stdout)
			}
		})
	}
}
