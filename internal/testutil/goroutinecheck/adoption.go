package goroutinecheck

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const fixtureMode = "DELEGATE_GOROUTINECHECK_FIXTURE"

// VerifyInstalled proves the calling package checks normal test completion by
// observing clean and deliberately leaking executions of this same test.
func VerifyInstalled(t *testing.T) {
	t.Helper()
	switch os.Getenv(fixtureMode) {
	case "clean":
		return
	case "leak":
		started := make(chan struct{})
		go deliberateLeak(started)
		<-started
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		mode string
		code int
	}{{mode: "clean"}, {mode: "leak", code: 1}} {
		// This is a fixture hang watchdog, not a runtime latency requirement.
		command := exec.Command(executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.timeout=15s")
		command.Env = append(os.Environ(), fixtureMode+"="+fixture.mode)
		output, runErr := command.CombinedOutput()
		code := 0
		if runErr != nil {
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) {
				t.Fatalf("run %s leak-check fixture: %v", fixture.mode, runErr)
			}
			code = exit.ExitCode()
		}
		if code != fixture.code {
			t.Fatalf("%s leak-check fixture exit=%d want=%d: %s", fixture.mode, code, fixture.code, output)
		}
		if fixture.mode == "leak" && !strings.Contains(string(output), "goroutinecheck.deliberateLeak") {
			t.Fatalf("leak-check fixture did not report its owned goroutine: %s", output)
		}
	}
}

func deliberateLeak(started chan<- struct{}) {
	close(started)
	select {}
}
