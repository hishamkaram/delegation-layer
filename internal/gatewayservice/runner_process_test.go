package gatewayservice

import (
	"context"
	"fmt"
	"os"
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
