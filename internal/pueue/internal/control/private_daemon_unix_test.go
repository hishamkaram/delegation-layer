//go:build linux || darwin

package control

import (
	"os"
	"os/exec"
	"os/signal"
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
	privateDaemonRelease    = "PUEUE_PRIVATE_DAEMON_RELEASE"
	privateDaemonInterrupt  = "PUEUE_PRIVATE_DAEMON_INTERRUPTED"
	privateDaemonClosed     = "PUEUE_PRIVATE_DAEMON_CLOSED"
)

// The parent owns the ancestor until helper Wait and daemon Wait are proven.
type privateDaemonTestFixture struct {
	base    string
	command *exec.Cmd
	joined  bool
}

func newPrivateDaemonTestFixture(t *testing.T) *privateDaemonTestFixture {
	t.Helper()
	base, err := os.MkdirTemp("", "private-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &privateDaemonTestFixture{base: base}
	t.Cleanup(func() {
		if fixture.command != nil && fixture.command.Process != nil {
			if !fixture.joined {
				t.Errorf("helper Wait unresolved; retaining fixture %s", base)
				return
			}
			if _, err := os.Stat(filepath.Join(base, "daemon-closed")); err != nil {
				t.Errorf("daemon Wait unresolved; retaining fixture %s: %v", base, err)
				return
			}
		}
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	return fixture
}

type privateDaemonHelperWait struct{ done <-chan struct{} }

func (owned *privateDaemonHelperWait) cleanup(t *testing.T) {
	t.Helper()
	if owned.done == nil {
		return
	}
	if gate := os.Getenv(privateDaemonRelease); gate != "" {
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Error(err)
		}
	}
	select {
	case <-owned.done:
	case <-time.After(3 * time.Second):
		t.Error("daemon Wait did not finish during helper cleanup")
		return
	}
	if err := os.WriteFile(os.Getenv(privateDaemonClosed), nil, 0o600); err != nil {
		t.Error(err)
	}
}

func awaitPrivateDaemonCallerInterruption(t *testing.T, done <-chan struct{}, interrupted <-chan os.Signal) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("daemon exited before the caller group interruption")
	default:
	}
	if err := os.WriteFile(os.Getenv(privateDaemonReady), []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-interrupted:
	case <-done:
		t.Fatal("daemon exited before caller interruption was observed")
	case <-time.After(5 * time.Second):
		t.Fatal("caller group interruption was not observed")
	}
	if err := os.WriteFile(os.Getenv(privateDaemonInterrupt), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateDaemonLaunchHelper(t *testing.T) {
	mode := os.Getenv(privateDaemonHelperMode)
	if mode == "" {
		return
	}
	base := os.Getenv("PUEUE_PRIVATE_DAEMON_BASE")
	executable := os.Getenv("PUEUE_PRIVATE_DAEMON_EXECUTABLE")
	config := os.Getenv("PUEUE_PRIVATE_DAEMON_CONFIG")
	childMode := os.Getenv("PUEUE_PRIVATE_DAEMON_CHILD_MODE")
	if base == "" || executable == "" || config == "" || childMode == "" {
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
	interrupted := make(chan os.Signal, 1)
	if mode == "session-launcher" {
		signal.Notify(interrupted, syscall.SIGTERM)
		defer signal.Stop(interrupted)
	}
	owned := &privateDaemonHelperWait{}
	t.Cleanup(func() { owned.cleanup(t) })
	options := Options{
		PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, Environment: environment,
		DaemonWaitOwner: daemonWaitOwnerFunc(func(_ string, _ int, done <-chan struct{}) error {
			owned.done = done
			return nil
		}),
	}
	if err := startDaemon(t.Context(), base, executable, config, base, options); err != nil {
		t.Fatal(err)
	}
	waitForPrivateDaemonTestFile(t, os.Getenv(privateDaemonMarker), 3*time.Second)
	if mode == "session-launcher" {
		awaitPrivateDaemonCallerInterruption(t, owned.done, interrupted)
	}

	select {
	case <-owned.done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not reach its actual Wait completion")
	}
}

func TestPrivateDaemonInheritsAvailableDescriptorLimit(t *testing.T) {
	fixture := newPrivateDaemonTestFixture(t)
	base := fixture.base
	actual := filepath.Join(base, "daemon-limit")
	expected := filepath.Join(base, "delegate-limit")
	script := writePrivateDaemonTestExecutable(t, base, "pueued-limit", `#!/bin/sh
set -eu
ulimit -n >"$PUEUE_PRIVATE_DAEMON_MARKER"
`)
	config := writePrivateDaemonTestConfig(t, base)
	command := exec.Command("/bin/sh", "-c", `ulimit -S -n 256; exec "$1" -test.run '^TestPrivateDaemonLaunchHelper$'`, "sh", os.Args[0])
	fixture.command = command
	command.Env = append(
		os.Environ(),
		privateDaemonHelperMode+"=limit-launcher",
		"PUEUE_PRIVATE_DAEMON_CHILD_MODE=limit-child",
		"PUEUE_PRIVATE_DAEMON_BASE="+base,
		"PUEUE_PRIVATE_DAEMON_EXECUTABLE="+script,
		"PUEUE_PRIVATE_DAEMON_CONFIG="+config,
		"PUEUE_PRIVATE_DAEMON_EXPECTED_LIMIT="+expected,
		privateDaemonMarker+"="+actual,
		privateDaemonClosed+"="+filepath.Join(base, "daemon-closed"),
	)
	output, runErr := command.CombinedOutput()
	fixture.joined = true
	if runErr != nil {
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

type privateDaemonCallerWait struct {
	done chan struct{}
	err  error
}

func (fixture *privateDaemonTestFixture) startCaller(t *testing.T, release string) *privateDaemonCallerWait {
	t.Helper()
	if err := lifetime.ConfigureIndependentSession(fixture.command); err != nil {
		t.Fatal(err)
	}
	if err := fixture.command.Start(); err != nil {
		t.Fatal(err)
	}
	caller := &privateDaemonCallerWait{done: make(chan struct{})}
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Error(err)
		}
		select {
		case <-caller.done:
			fixture.joined = true
		case <-time.After(6 * time.Second):
			t.Errorf("caller helper did not join; retaining fixture %s", fixture.base)
		}
	})
	go func() {
		caller.err = fixture.command.Wait()
		close(caller.done)
	}()
	return caller
}

func TestPrivateDaemonSurvivesCallerGroupInterruption(t *testing.T) {
	fixture := newPrivateDaemonTestFixture(t)
	base := fixture.base
	pidMarker := filepath.Join(base, "daemon-pid")
	ready := filepath.Join(base, "launcher-ready")
	survived := filepath.Join(base, "daemon-survived")
	release := filepath.Join(base, "daemon-release")
	interrupted := filepath.Join(base, "launcher-interrupted")
	script := writePrivateDaemonTestExecutable(t, base, "pueued-session", `#!/bin/sh
set -eu
trap 'exit 99' HUP INT TERM
printf '%s\n' "$$" >"$PUEUE_PRIVATE_DAEMON_MARKER"
while [ ! -f "$PUEUE_PRIVATE_DAEMON_RELEASE" ]; do sleep 0.01; done
printf 'survived\n' >"$PUEUE_PRIVATE_DAEMON_SURVIVED"
`)
	config := writePrivateDaemonTestConfig(t, base)
	command := exec.Command(os.Args[0], "-test.run", "^TestPrivateDaemonLaunchHelper$")
	fixture.command = command
	command.Env = append(
		os.Environ(),
		privateDaemonHelperMode+"=session-launcher",
		"PUEUE_PRIVATE_DAEMON_CHILD_MODE=session-child",
		"PUEUE_PRIVATE_DAEMON_BASE="+base,
		"PUEUE_PRIVATE_DAEMON_EXECUTABLE="+script,
		"PUEUE_PRIVATE_DAEMON_CONFIG="+config,
		privateDaemonMarker+"="+pidMarker,
		privateDaemonReady+"="+ready,
		privateDaemonRelease+"="+release,
		privateDaemonInterrupt+"="+interrupted,
		privateDaemonClosed+"="+filepath.Join(base, "daemon-closed"),
		"PUEUE_PRIVATE_DAEMON_SURVIVED="+survived,
	)
	caller := fixture.startCaller(t, release)
	waitForPrivateDaemonTestFile(t, ready, 3*time.Second)
	pid, err := strconv.Atoi(strings.TrimSpace(readPrivateDaemonTestFile(t, pidMarker)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid daemon PID: %d: %v", pid, err)
	}
	before, err := observeNativePrivateProcess(pid)
	if err != nil || !before.Present || before.StartToken == "" {
		t.Fatalf("daemon was not alive before interruption: %+v: %v", before, err)
	}
	signal := exec.Command("/bin/sh", "-c", `kill -TERM -"$1"`, "sh", strconv.Itoa(command.Process.Pid))
	if output, signalErr := signal.CombinedOutput(); signalErr != nil {
		t.Fatalf("interrupt caller process group: %v: %s", signalErr, output)
	}
	waitForPrivateDaemonTestFile(t, interrupted, 3*time.Second)
	after, err := observeNativePrivateProcess(pid)
	if err != nil || !after.Present || after.StartToken != before.StartToken {
		t.Fatalf("same daemon was not alive after caller group interruption: before=%+v after=%+v err=%v", before, after, err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-caller.done:
		fixture.joined = true
		if caller.err != nil {
			t.Fatalf("caller helper failed to retain daemon Wait: %v", caller.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller helper did not join daemon Wait")
	}
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
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
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
