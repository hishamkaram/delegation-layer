//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

const cliStartupHelperMode = "DELEGATE_TEST_CLI_STARTUP_MODE"

// TestCLIStartupHelper is the body of the child process started by
// TestCLIStartupDoesNotReadHostCPUStatistics. It is inert in a normal run.
func TestCLIStartupHelper(t *testing.T) {
	mode := os.Getenv(cliStartupHelperMode)
	if mode == "" {
		t.Skip("runs only as the CLI startup child process")
	}
	os.Exit(run([]string{mode}, io.Discard, io.Discard))
}

// TestCLIStartupDoesNotReadHostCPUStatistics proves help and version do not
// depend on resource metrics: no package initializer or command path opens
// the CPU statistics file under an inherited HOST_PROC before or during the
// command. The watch is registered before each child starts, and its queued
// events are read only after the child has exited.
func TestCLIStartupDoesNotReadHostCPUStatistics(t *testing.T) {
	hostProc := t.TempDir()
	stat := filepath.Join(hostProc, "stat")
	if err := os.WriteFile(stat, []byte("cpu  10 0 10 1000 0 0 0 0 0 0\ncpu0 10 0 10 1000 0 0 0 0 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	watch, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := unix.Close(watch); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, err = unix.InotifyAddWatch(watch, stat, unix.IN_OPEN|unix.IN_ACCESS); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"help", "version"} {
		child := exec.Command(executable, "-test.run=^TestCLIStartupHelper$")
		child.Env = append(os.Environ(), cliStartupHelperMode+"="+mode, "HOST_PROC="+hostProc)
		if output, runErr := child.CombinedOutput(); runErr != nil {
			t.Fatalf("CLI %s child failed: %v output=%q", mode, runErr, output)
		}
		if events := queuedInotifyBytes(t, watch); events != 0 {
			t.Fatalf("CLI %s opened or read HOST_PROC/stat (%d event bytes)", mode, events)
		}
	}
}

func queuedInotifyBytes(t *testing.T, watch int) int {
	t.Helper()
	buffer := make([]byte, 4096)
	total := 0
	for {
		n, err := unix.Read(watch, buffer)
		if errors.Is(err, unix.EAGAIN) {
			return total
		}
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return total
		}
		total += n
	}
}
