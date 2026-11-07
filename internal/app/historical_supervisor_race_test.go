//go:build race

package app

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
)

func TestHistoricalSupervisorFixtureRaceDetector(t *testing.T) {
	t.Setenv("GORACE", "halt_on_error=1 exitcode=73")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode string
		exit int
	}{
		{"clean", 0},
		{"race", 73},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			command := exec.Command(executable, "-test.run=^TestHistoricalSupervisorRaceHelper$", "-test.count=1", "-test.timeout=15s")
			command.Env = append(newHistoricalSupervisorOptions().Environment, "DELEGATE_HISTORICAL_SUPERVISOR_RACE_HELPER="+tc.mode)
			output, runErr := command.CombinedOutput()
			if tc.exit == 0 {
				if runErr != nil {
					t.Fatalf("clean fixture failed: %v\n%s", runErr, output)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != tc.exit {
					t.Fatalf("race fixture exit: %v, want %d\n%s", runErr, tc.exit, output)
				}
				if !bytes.Contains(output, []byte("WARNING: DATA RACE")) {
					t.Fatalf("fixture did not report its deliberate race:\n%s", output)
				}
			}
			if command.ProcessState == nil || !command.ProcessState.Exited() {
				t.Fatal("fixture process was not joined")
			}
		})
	}
}

func TestHistoricalSupervisorRaceHelper(t *testing.T) {
	if os.Getenv("DELEGATE_HISTORICAL_SUPERVISOR_RACE_HELPER") != "race" {
		return
	}
	// Deliberate child-only race proves fixture options keep instrumentation active.
	var value int
	var joined sync.WaitGroup
	start := make(chan struct{})
	joined.Add(2)
	for range 2 {
		go func() {
			defer joined.Done()
			<-start
			value++
		}()
	}
	close(start)
	joined.Wait()
	t.Log(value)
}
