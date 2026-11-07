package testsupport

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestWriteExecutableReleasesForkGuardOnError(t *testing.T) {
	const helperMode = "DELEGATE_TEST_FIXTURE_WRITE_ERROR"
	const released = "fork guard released after write failure"
	if os.Getenv(helperMode) == "1" {
		t.Cleanup(func() {
			if !syscall.ForkLock.TryLock() {
				t.Error("fork remains blocked after the executable write failed")
				return
			}
			syscall.ForkLock.Unlock()
			t.Log(released)
		})
		WriteExecutable(t, t.TempDir(), "cannot replace a directory")
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestWriteExecutableReleasesForkGuardOnError$")
	command.Env = append(os.Environ(), helperMode+"=1")
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !bytes.Contains(output, []byte(released)) {
		t.Fatalf("failed write did not release its fork guard: err=%v, output=%s", err, output)
	}
}
