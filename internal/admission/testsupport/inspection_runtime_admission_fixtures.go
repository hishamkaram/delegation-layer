package testsupport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func WaitForTestPath(t *testing.T, path string) {
	WaitForTestPathWithin(t, path, pueue.DefaultObservationTimeout)
}

func WaitForTestPathWithin(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", path)
		case <-ticker.C:
		}
	}
}

func AssertRetainedInspectionPending(t *testing.T, err error) *pueue.Pending {
	t.Helper()
	var inFlight *pueue.InFlightError
	if !errors.As(err, &inFlight) || inFlight == nil || inFlight.Pending == nil {
		t.Fatalf("missing retained in-flight supervisor command: %v", err)
	}
	select {
	case <-inFlight.Pending.Done():
		t.Fatal("returned only after the in-flight supervisor command completed")
	default:
	}
	return inFlight.Pending
}

func DrainInspectionPending(t *testing.T, pending *pueue.Pending) {
	t.Helper()
	if pending == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-pending.Done():
	case <-ctx.Done():
		t.Fatalf("timed out waiting for retained supervisor command: %v", ctx.Err())
	}
}

func NewObservedInspectionTestClient(t *testing.T, bound *pueue.Client, logPath string, timeout time.Duration, observer func(pueue.CommandEvent), extraEnvironment ...string) *pueue.Client {
	t.Helper()
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	environment := append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath)
	environment = append(environment, extraEnvironment...)
	client, err := pueue.NewClient(bound.Binding(), pueue.Options{
		Environment:        environment,
		ObservationTimeout: timeout,
		Observer:           observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client
}

func NewUncertainGroupSupervisor(t *testing.T, rootID string) (*pueue.Client, string, string) {
	return NewUncertainGroupSupervisorWithConfig(t, rootID, "")
}

func NewUncertainGroupSupervisorWithConfig(t *testing.T, rootID, configuredPath string) (*pueue.Client, string, string) {
	return NewUncertainGroupSupervisorWithConfigAndTimeout(t, rootID, configuredPath, 500*time.Millisecond)
}

func NewUncertainGroupSupervisorWithConfigAndTimeout(t *testing.T, rootID, configuredPath string, observationTimeout time.Duration) (*pueue.Client, string, string) {
	t.Helper()
	rawBase, err := os.MkdirTemp("/tmp", "dlp-app-inspection-group-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(rawBase); removeErr != nil {
			t.Error(removeErr)
		}
	})
	base, err := filepath.EvalSymlinks(rawBase)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(base, "pueue")
	configPath := configuredPath
	if configPath == "" {
		configPath = filepath.Join(base, "pueue.json")
	} else if mkdirErr := os.MkdirAll(filepath.Dir(configPath), 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	statusPath, afterPath, donePath, logPath := WriteUncertainGroupSupervisorFixture(
		t, base, executable, configPath, rootID,
	)
	environment := append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath, "FAKE_AFTER="+afterPath, "FAKE_DONE="+donePath)
	bound, err := pueue.Bind(context.Background(), executable, configPath, pueue.Options{Environment: environment, ObservationTimeout: pueue.DefaultObservationTimeout})
	if err != nil {
		t.Fatalf("bind uncertain-group supervisor fixture: %v", err)
	}
	client, err := pueue.NewClient(bound.Binding(), pueue.Options{Environment: environment, ObservationTimeout: observationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	return client, logPath, donePath
}

func WriteUncertainGroupSupervisorFixture(
	t *testing.T,
	base, executable, configPath, rootID string,
) (string, string, string, string) {
	t.Helper()
	statusPath := filepath.Join(base, "status.json")
	afterPath := filepath.Join(base, "status-after.json")
	donePath := filepath.Join(base, "group-done")
	logPath := filepath.Join(base, "invocations.log")
	WriteUncertainGroupSupervisorExecutable(t, executable)
	WriteUncertainGroupSupervisorConfig(t, base, configPath, rootID, statusPath, afterPath)
	return statusPath, afterPath, donePath, logPath
}

func WriteUncertainGroupSupervisorExecutable(t *testing.T, executable string) {
	t.Helper()
	script := `#!/bin/sh
set -eu
{
  printf '%s\n' '---'
  for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
} >> "$FAKE_LOG"
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status)
    if [ -n "${FAKE_STATUS_FAIL_ONCE:-}" ] && [ ! -e "$FAKE_STATUS_FAIL_ONCE" ]; then
      : > "$FAKE_STATUS_FAIL_ONCE"
      exit 23
    fi
    cat "$FAKE_STATUS"
    exit "${FAKE_STATUS_EXIT:-0}"
    ;;
  shutdown)
    sleep 1
    touch "$(dirname "$FAKE_LOG")/shutdown-done"
    exit 1
    ;;
  group)
    [ "${4-}" = add ] && [ "${5-}" = --parallel ] && [ "${6-}" = 1 ]
    sleep 1
    cp "$FAKE_AFTER" "$FAKE_STATUS"
    : > "$FAKE_DONE"
    ;;
  add)
    [ "${4-}" = --help ]
    printf '%s\n' '--stashed --print-task-id'
    ;;
  enqueue)
    [ "${4-}" = --help ]
    printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'
    ;;
  *) exit 64 ;;
esac
`
	if writeErr := os.WriteFile(executable, []byte(script), 0o700); writeErr != nil {
		t.Fatal(writeErr)
	}
}

func WriteUncertainGroupSupervisorConfig(
	t *testing.T,
	base, configPath, rootID, statusPath, afterPath string,
) {
	t.Helper()
	shared := map[string]any{
		"pueue_directory":    filepath.Join(base, "data"),
		"runtime_directory":  filepath.Join(base, "run"),
		"unix_socket_path":   filepath.Join(base, "socket"),
		"alias_file":         filepath.Join(base, "aliases"),
		"pid_path":           filepath.Join(base, "pid"),
		"shared_secret_path": filepath.Join(base, "secret"),
		"daemon_cert":        filepath.Join(base, "cert"),
		"daemon_key":         filepath.Join(base, "key"),
	}
	config, err := json.Marshal(map[string]any{"shared": shared})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(configPath, config, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if writeErr := os.WriteFile(statusPath, []byte(`{"tasks":{},"groups":{}}`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	group := "delegation-inspection-" + rootID
	after := []byte(`{"tasks":{},"groups":{"` + group + `":{"status":"Running","parallel_tasks":1}}}`)
	if writeErr := os.WriteFile(afterPath, after, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
}
