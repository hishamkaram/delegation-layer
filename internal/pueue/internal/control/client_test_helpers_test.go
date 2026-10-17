package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (failure *submitStartFailure) observe(event CommandEvent) {
	if !slices.Contains(event.Argv, "add") {
		return
	}
	switch event.Stage {
	case "entry":
		failure.err = os.Rename(failure.executable, failure.hidden)
		failure.moved = failure.err == nil
	case "completed":
		if event.PID != 0 {
			return
		}
		failure.failed = true
		if failure.moved {
			restoreErr := os.Rename(failure.hidden, failure.executable)
			failure.err = errors.Join(failure.err, restoreErr)
			failure.moved = restoreErr != nil
		}
	}
}

func (failure *submitStartFailure) restore() error {
	if !failure.moved {
		return nil
	}
	err := os.Rename(failure.hidden, failure.executable)
	if err == nil {
		failure.moved = false
	}
	return err
}

func validBindingForTest() task.SupervisorRef {
	digest := task.ComputeSHA256([]byte("binding"))
	return task.SupervisorRef{
		ClientExecutable:     "/bin/sh",
		ClientSHA256:         digest,
		ResolvedConfigSHA256: digest,
		ConfigPath:           "/tmp/pueue.yml",
		ConfigDigest:         digest,
		Endpoint:             "unix:/tmp/pueue.sock",
		ObservedVersion:      FixtureVersion,
	}
}

func newFakeSupervisor(t *testing.T, mode string) *fakeSupervisor {
	t.Helper()
	fake := newFakeSupervisorPaths(t, mode, "pueue "+FixtureVersion)
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{ObservationTimeout: DefaultObservationTimeout, Environment: fake.environment})
	if err != nil {
		var inFlight *InFlightError
		if errors.As(err, &inFlight) && inFlight.Pending != nil {
			fake.cleanupPending = inFlight.Pending
			if !awaitPendingNaturally(inFlight.Pending, 15*time.Second) {
				t.Fatalf("fake supervisor binding remained in flight: %v", err)
			}
			fake.cleanupPending = nil
		}
		t.Fatal(err)
	}
	fake.client = client
	return fake
}

func newFakeSupervisorPaths(t *testing.T, mode, version string) *fakeSupervisor {
	t.Helper()
	rawBase, err := os.MkdirTemp("/tmp", "dlp-")
	if err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(rawBase)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSupervisor{
		executable: filepath.Join(base, "pueue-fake"),
		configPath: filepath.Join(base, "pueue.json"),
		statusPath: filepath.Join(base, "status.json"),
		logPath:    filepath.Join(base, "invocations.log"),
	}
	t.Cleanup(func() {
		if fake.cleanupPending != nil && !awaitPendingNaturally(fake.cleanupPending, 15*time.Second) {
			t.Logf("preserving fake supervisor files while binding remains in flight: %s", base)
			return
		}
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(fake.executable, []byte(fakeSupervisorScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake.statusPath, statusTestPayload(t, map[string]any{}, map[string]any{}), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFakeConfig(t, fake.configPath, base)
	fake.environment = append(
		os.Environ(),
		"FAKE_LOG="+fake.logPath,
		"FAKE_STATUS="+fake.statusPath,
		"FAKE_MODE="+mode,
		"FAKE_VERSION="+version,
	)
	return fake
}

func awaitPendingNaturally(pending *Pending, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-pending.Done():
		return true
	case <-timer.C:
		return false
	}
}

func TestWithObservationTimeoutDoesNotChangeBoundClient(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	inspection, err := fake.client.WithObservationTimeout(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.options.ObservationTimeout != time.Minute {
		t.Fatalf("inspection timeout=%s, want %s", inspection.options.ObservationTimeout, time.Minute)
	}
	if fake.client.options.ObservationTimeout != DefaultObservationTimeout {
		t.Fatalf("ordinary timeout changed to %s", fake.client.options.ObservationTimeout)
	}
	if inspection.binding != fake.client.binding || inspection.runtimeBinding != fake.client.runtimeBinding {
		t.Fatal("timeout-scoped client did not retain the bound supervisor identity")
	}
	if _, err = fake.client.WithObservationTimeout(-time.Second); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("negative timeout was accepted: %v", err)
	}
}

func TestWithCommandEnvironmentKeepsBindingAndPendingOwnerImmutable(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	original := slices.Clone(fake.client.options.Environment)
	environment := append(slices.Clone(original), "WORKSPACE_VERIFY_SENTINEL=original")
	command, err := fake.client.WithCommandEnvironment(environment)
	if err != nil {
		t.Fatal(err)
	}
	environment[len(environment)-1] = "WORKSPACE_VERIFY_SENTINEL=changed"
	if !slices.Equal(fake.client.options.Environment, original) || !slices.Contains(command.options.Environment, "WORKSPACE_VERIFY_SENTINEL=original") {
		t.Fatal("derived environment aliases the caller or changes daemon/control defaults")
	}
	if command.Binding() != fake.client.Binding() || command.RuntimeBinding() != fake.client.RuntimeBinding() || command.workOwner != fake.client.workOwner || command.ownsWorkOwner {
		t.Fatal("derived command client changed supervisor identity or work ownership")
	}
	if err = command.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = fake.client.checkVersion(t.Context()); err != nil {
		t.Fatalf("closing a derived client closed the original owner: %v", err)
	}
}

func writeFakeConfig(t *testing.T, path, base string) {
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
	data, err := json.Marshal(map[string]any{"shared": shared})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeStatusFor(t *testing.T, fake *fakeSupervisor, rootID, taskID string, id int64, state State) {
	t.Helper()
	var status map[string]any
	switch state {
	case StateStashed:
		status = map[string]any{"Stashed": map[string]any{"enqueue_at": nil}}
	case StateQueued:
		status = map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}}
	case StateRunning:
		status = map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}
	case StateEnded:
		status = map[string]any{"Done": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": "Success"}}
	case StateUnknown:
		t.Fatalf("unknown status cannot be rendered as a positive row")
	default:
		t.Fatalf("unsupported test status %q", state)
	}
	label := "delegate:" + rootID + ":" + taskID
	data := statusTestPayload(t, map[string]any{strconv.FormatInt(id, 10): statusTestJob(id, label, status)}, map[string]any{})
	if err := os.WriteFile(fake.statusPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readInvocations(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all [][]string
	var current []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch {
		case strings.HasPrefix(line, "argc="):
			current = nil
		case strings.HasPrefix(line, "arg="):
			current = append(current, strings.TrimPrefix(line, "arg="))
		case line == "---":
			all = append(all, current)
		}
	}
	return all
}

func countCommand(invocations [][]string, command string) int {
	count := 0
	for _, args := range invocations {
		if len(args) > 2 && args[2] == command {
			count++
		}
	}
	return count
}

func newTaskFixture(t *testing.T, binding task.SupervisorRef) *taskFixture {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("hello brief")
	requested := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	req := &task.TaskRecord{
		SchemaVersion:   task.SchemaVersion,
		RootID:          store.RootID,
		TaskID:          taskID,
		Provider:        "fixture:test",
		Mode:            "read-only",
		CanonicalCwd:    workspace,
		RequestedConfig: requested,
		BudgetNanos:     int64(time.Minute),
		BriefSHA256:     task.ComputeSHA256(brief),
		BriefLength:     int64(len(brief)),
	}
	meta := &task.MetaRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             store.RootID,
		TaskID:             taskID,
		RequestedConfig:    requested,
		EffectiveConfig:    task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("effective"))},
		Containment:        "fixture-only",
		Approval:           "never",
		ProviderExecutable: binding.ClientExecutable,
		ProviderVersion:    "fixture-v1",
		PublisherBuild:     "test-build",
		PublisherVersion:   "test",
		Predicate:          task.FixturePredicateRef(),
		SupervisorConfig:   binding,
		CreatedAt:          time.Now().UTC().Format(time.RFC3339Nano),
	}
	taskDir, err := store.CreateTask(taskID, req, brief, meta)
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	fixture := &taskFixture{store: store, taskDir: taskDir}
	t.Cleanup(func() {
		if err := taskDir.Close(); err != nil {
			t.Error(err)
		}
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return fixture
}

func identityFromRecord(record *task.SubmitRecord) Identity {
	return Identity{RootID: record.RootID, TaskID: record.TaskID, SpecSHA256: record.SpecSHA256, MetaSHA256: record.MetaSHA256}
}

func createSubmission(t *testing.T, fixture *taskFixture, binding task.SupervisorRef) *task.SubmitRecord {
	t.Helper()
	permit, err := fixture.taskDir.PrepareSubmission(binding)
	if err != nil {
		t.Fatal(err)
	}
	record, err := permit.Record()
	if err != nil {
		t.Fatal(err)
	}
	if err := permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err := permit.Release(); err != nil {
		t.Fatal(err)
	}
	return record
}

func createStopPermit(t *testing.T, fixture *taskFixture) *taskdir.StopPermit {
	t.Helper()
	requestID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	permit, err := fixture.taskDir.PrepareStop(requestID, "user", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return permit
}
