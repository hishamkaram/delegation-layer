package testsupport

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func CanonicalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func CanonicalTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "dla-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cleanupErr := os.RemoveAll(directory); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	return CanonicalPath(t, directory)
}

func NewInspectionRequest(t *testing.T, store *taskdir.Store) task.TaskRecord {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return task.TaskRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		TaskID:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Provider:      "fixture:test",
		Mode:          "read-only",
		CanonicalCwd:  workspace,
		RequestedConfig: task.TaskConfig{
			Permission: "read-only",
			Budget:     "1m0s",
		},
		BudgetNanos: int64(time.Minute),
		BriefSHA256: task.ComputeSHA256([]byte("inspection brief")),
		BriefLength: int64(len("inspection brief")),
	}
}

func NewInspectionStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(CanonicalTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func NewBlockingSupervisor(t *testing.T, rootID string) (*pueue.Client, string) {
	t.Helper()
	rawBase, err := os.MkdirTemp("/tmp", "dlp-app-inspection-")
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
	configPath := filepath.Join(base, "pueue.json")
	statusPath := filepath.Join(base, "status.json")
	logPath := filepath.Join(base, "invocations.log")
	script := `#!/bin/sh
set -eu
{
  printf '%s\n' '---'
  for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
} >> "$FAKE_LOG"
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status) sleep 1; cat "$FAKE_STATUS" ;;
  *) exit 64 ;;
esac
`
	if writeErr := os.WriteFile(executable, []byte(script), 0o700); writeErr != nil {
		t.Fatal(writeErr)
	}
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
	status, err := json.Marshal(map[string]any{
		"tasks":  map[string]any{},
		"groups": map[string]any{"delegation-inspection-" + rootID: map[string]any{"status": "Running", "parallel_tasks": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(statusPath, status, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	environment := append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath)
	// The initial bind probes the fixture through /bin/sh. Keep that setup
	// budget at the production default so scheduler contention under -race
	// cannot turn shell startup into a false binding failure. The returned
	// client deliberately uses a shorter observation window below so the
	// operation still exercises retained in-flight status ownership.
	bound, err := pueue.Bind(context.Background(), executable, configPath, pueue.Options{Environment: environment, ObservationTimeout: pueue.DefaultObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	client, err := pueue.NewClient(bound.Binding(), pueue.Options{Environment: environment, ObservationTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return client, logPath
}

func RequireExecutable(t *testing.T, runner, contents string) {
	t.Helper()
	data, err := os.ReadFile(runner)
	if err != nil || string(data) != contents {
		t.Fatalf("runner content = %q, want %q (err=%v)", data, contents, err)
	}
}

func WriteExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
