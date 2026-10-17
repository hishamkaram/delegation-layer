//go:build linux || darwin

package control

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

const (
	privateDaemonHelperMode = "PUEUE_PRIVATE_DAEMON_HELPER_MODE"
	privateDaemonMarker     = "PUEUE_PRIVATE_DAEMON_MARKER"
	privateDaemonReady      = "PUEUE_PRIVATE_DAEMON_READY"
)

func TestPrivateDaemonLaunchHelper(t *testing.T) {
	mode := os.Getenv(privateDaemonHelperMode)
	if mode == "" {
		return
	}
	base := os.Getenv("PUEUE_PRIVATE_DAEMON_BASE")
	executable := os.Getenv("PUEUE_PRIVATE_DAEMON_EXECUTABLE")
	digest := os.Getenv("PUEUE_PRIVATE_DAEMON_SHA256")
	config := os.Getenv("PUEUE_PRIVATE_DAEMON_CONFIG")
	childMode := os.Getenv("PUEUE_PRIVATE_DAEMON_CHILD_MODE")
	if base == "" || executable == "" || digest == "" || config == "" || childMode == "" {
		t.Fatal("private daemon helper environment is incomplete")
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	if expected := os.Getenv("PUEUE_PRIVATE_DAEMON_EXPECTED_LIMIT"); expected != "" {
		if err := os.WriteFile(expected, []byte(strconv.FormatUint(limit.Cur, 10)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	environment := replacePrivateDaemonHelperEnvironment(os.Environ(), privateDaemonHelperMode, childMode)
	if err := startDaemon(executable, digest, config, base, Options{Environment: environment}); err != nil {
		t.Fatal(err)
	}
	waitForPrivateDaemonTestFile(t, os.Getenv(privateDaemonMarker), 3*time.Second)
	if mode == "session-launcher" {
		if err := os.WriteFile(os.Getenv(privateDaemonReady), []byte("ready\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
	}
}

func TestPrivateDaemonInheritsAvailableDescriptorLimit(t *testing.T) {
	base := t.TempDir()
	actual := filepath.Join(base, "daemon-limit")
	expected := filepath.Join(base, "delegate-limit")
	script := writePrivateDaemonTestExecutable(t, base, "pueued-limit", `#!/bin/sh
set -eu
ulimit -n >"$PUEUE_PRIVATE_DAEMON_MARKER"
`)
	config := writePrivateDaemonTestConfig(t, base)
	digest, err := hashExecutable(script)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", "-c", `ulimit -S -n 256; exec "$1" -test.run '^TestPrivateDaemonLaunchHelper$'`, "sh", os.Args[0])
	command.Env = append(os.Environ(),
		privateDaemonHelperMode+"=limit-launcher",
		"PUEUE_PRIVATE_DAEMON_CHILD_MODE=limit-child",
		"PUEUE_PRIVATE_DAEMON_BASE="+base,
		"PUEUE_PRIVATE_DAEMON_EXECUTABLE="+script,
		"PUEUE_PRIVATE_DAEMON_SHA256="+digest,
		"PUEUE_PRIVATE_DAEMON_CONFIG="+config,
		"PUEUE_PRIVATE_DAEMON_EXPECTED_LIMIT="+expected,
		privateDaemonMarker+"="+actual,
	)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("descriptor-limit helper failed: %v: %s", runErr, output)
	}
	want := strings.TrimSpace(readPrivateDaemonTestFile(t, expected))
	got := strings.TrimSpace(readPrivateDaemonTestFile(t, actual))
	if want == "256" {
		t.Fatal("Go runtime did not raise the helper descriptor limit above its startup value")
	}
	if got != want {
		t.Fatalf("private daemon descriptor limit=%s, want Delegate limit %s", got, want)
	}
}

func TestPrivateDaemonSurvivesCallerGroupInterruption(t *testing.T) {
	base := t.TempDir()
	pidMarker := filepath.Join(base, "daemon-pid")
	ready := filepath.Join(base, "launcher-ready")
	survived := filepath.Join(base, "daemon-survived")
	script := writePrivateDaemonTestExecutable(t, base, "pueued-session", `#!/bin/sh
set -eu
trap 'exit 99' HUP INT TERM
printf '%s\n' "$$" >"$PUEUE_PRIVATE_DAEMON_MARKER"
sleep 1
printf 'survived\n' >"$PUEUE_PRIVATE_DAEMON_SURVIVED"
`)
	config := writePrivateDaemonTestConfig(t, base)
	digest, err := hashExecutable(script)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run", "^TestPrivateDaemonLaunchHelper$")
	command.Env = append(os.Environ(),
		privateDaemonHelperMode+"=session-launcher",
		"PUEUE_PRIVATE_DAEMON_CHILD_MODE=session-child",
		"PUEUE_PRIVATE_DAEMON_BASE="+base,
		"PUEUE_PRIVATE_DAEMON_EXECUTABLE="+script,
		"PUEUE_PRIVATE_DAEMON_SHA256="+digest,
		"PUEUE_PRIVATE_DAEMON_CONFIG="+config,
		privateDaemonMarker+"="+pidMarker,
		privateDaemonReady+"="+ready,
		"PUEUE_PRIVATE_DAEMON_SURVIVED="+survived,
	)
	if err = lifetime.ConfigureIndependentSession(command); err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			if waitErr := command.Wait(); waitErr != nil {
				t.Logf("cleanup wait for caller helper: %v", waitErr)
			}
		}
	})
	waitForPrivateDaemonTestFile(t, ready, 3*time.Second)
	signal := exec.Command("/bin/sh", "-c", `kill -TERM -"$1"`, "sh", strconv.Itoa(command.Process.Pid))
	if output, signalErr := signal.CombinedOutput(); signalErr != nil {
		t.Fatalf("interrupt caller process group: %v: %s", signalErr, output)
	}
	if waitErr := command.Wait(); waitErr == nil {
		t.Fatal("caller helper survived its process-group interruption")
	}
	waited = true
	waitForPrivateDaemonTestFile(t, survived, 3*time.Second)
}

func replacePrivateDaemonHelperEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func writePrivateDaemonTestExecutable(t *testing.T, directory, name, source string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(source), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writePrivateDaemonTestConfig(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "pueue.yml")
	if err := os.WriteFile(path, []byte("private fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForPrivateDaemonTestFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	if path == "" {
		t.Fatal("private daemon test marker is empty")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("private daemon test marker was not written: %s", path)
}

func readPrivateDaemonTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
