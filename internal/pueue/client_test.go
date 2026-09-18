package pueue

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

type fakeSupervisor struct {
	client         *Client
	executable     string
	configPath     string
	statusPath     string
	logPath        string
	environment    []string
	cleanupPending *Pending
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
	fake.environment = append(os.Environ(),
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

const fakeSupervisorScript = `#!/bin/sh
set -eu
{
  printf 'argc=%s\n' "$#"
  for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
  printf '%s\n' '---'
} >> "$FAKE_LOG"
mode=${FAKE_MODE:-ok}
case "${3-}" in
  --version)
    printf '%s\n' "${FAKE_VERSION:-pueue 4.0.4}"
    ;;
  status)
    if [ "$mode" = "delay-status" ]; then sleep 0.08; fi
    cat "$FAKE_STATUS"
    ;;
  add)
    printf '%s\n' "${FAKE_ADD_ID:-7}"
    ;;
  kill|remove)
    case "$mode" in
      stop-refuse) printf '%s\n' 'refused' >&2; exit 7 ;;
      stop-overflow) dd if=/dev/zero bs=1048577 count=1 2>/dev/null ;;
      *) exit 0 ;;
    esac
    ;;
  *)
    printf '%s\n' 'unexpected command' >&2
    exit 64
    ;;
esac
`

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

type taskFixture struct {
	store   *taskdir.Store
	taskDir *taskdir.TaskDir
}

func newTaskFixture(t *testing.T, binding task.SupervisorRef) *taskFixture {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := task.NewTaskID()
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
	requestID, err := task.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	permit, err := fixture.taskDir.PrepareStop(requestID, "user", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return permit
}

func TestBindReconcileAndReceiptUseCompleteBinding(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	rootID, taskID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	writeStatusFor(t, fake, rootID, taskID, 7, StateQueued)
	identity := Identity{RootID: rootID, TaskID: taskID, SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64)}
	observation, err := fake.client.Reconcile(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Matched || observation.State != StateQueued || observation.NumericTaskID != 7 {
		t.Fatalf("unexpected observation: %+v", observation)
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if receipt.NumericTaskID != 7 || receipt.SupervisorRef() != fake.client.Binding() {
		t.Fatalf("receipt lost binding: %+v", receipt)
	}
	if err := task.ValidateSupervisorReceipt(receipt); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotIdentityCopiesMutableNumericTarget(t *testing.T) {
	target := int64(7)
	snapshot := snapshotIdentity(Identity{NumericTaskID: &target})
	target = 8
	if snapshot.NumericTaskID == nil || *snapshot.NumericTaskID != 7 {
		t.Fatalf("numeric target alias escaped snapshot: %+v", snapshot)
	}
}

func TestSubmitConsumesOnePermitAndUsesLiteralArguments(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	prepared, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	record, err := prepared.Record()
	if err != nil {
		t.Fatal(err)
	}
	writeStatusFor(t, fake, record.RootID, record.TaskID, 7, StateQueued)
	observation, err := fake.client.Submit(context.Background(), prepared, Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Matched || observation.NumericTaskID != 7 || observation.State != StateQueued {
		t.Fatalf("submission was not reconciled: %+v", observation)
	}
	if err := prepared.Release(); err != nil {
		t.Fatal(err)
	}
	invocations := readInvocations(t, fake.logPath)
	var add []string
	for _, args := range invocations {
		if len(args) > 2 && args[2] == "add" {
			add = args
		}
	}
	want := []string{"-c", fake.configPath, "add", "--escape", "--label", record.Label, "--print-task-id", "--", fake.executable, "--root", fixture.store.Root, record.TaskID}
	if !slices.Equal(add, want) {
		t.Fatalf("literal add argv changed: got=%q want=%q", add, want)
	}
	if countCommand(invocations, "add") != 1 {
		t.Fatalf("expected exactly one add: %+v", invocations)
	}
}

func TestSubmitNeverRetriesAfterUncertainAdmission(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	prepared, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	record, err := prepared.Record()
	if err != nil {
		t.Fatal(err)
	}
	writeStatusFor(t, fake, record.RootID, record.TaskID, 8, StateQueued)
	observation, err := fake.client.Submit(context.Background(), prepared, Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root})
	if !errors.Is(err, ErrUnknown) || observation.Matched {
		t.Fatalf("lost admission was treated as success: %+v %v", observation, err)
	}
	if _, err := fake.client.Submit(context.Background(), prepared, Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root}); !errors.Is(err, task.ErrPermitAlreadyUsed) {
		t.Fatalf("second add did not stop at consumed permit: %v", err)
	}
	if err := prepared.Release(); err != nil {
		t.Fatal(err)
	}
	if countCommand(readInvocations(t, fake.logPath), "add") != 1 {
		t.Fatal("uncertain admission retried add")
	}
}

func TestStopWithoutSavedTargetNeverReconcilesOrMutates(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	record := createSubmission(t, fixture, fake.client.Binding())
	writeStatusFor(t, fake, record.RootID, record.TaskID, 7, StateRunning)
	permit := createStopPermit(t, fixture)
	before := len(readInvocations(t, fake.logPath))
	result, err := fake.client.Stop(context.Background(), permit)
	if !errors.Is(err, ErrUnknown) || !result.Requested || result.Attempted || result.Matched || result.NumericTaskID != nil {
		t.Fatalf("missing target was routed: %+v %v", result, err)
	}
	if after := len(readInvocations(t, fake.logPath)); after != before {
		t.Fatalf("missing target triggered a fresh status lookup: before=%d after=%d", before, after)
	}
	if err := permit.Release(); err != nil {
		t.Fatal(err)
	}
}

func prepareTargetedStop(t *testing.T, mode string, state State) (*fakeSupervisor, *taskFixture, *taskdir.StopPermit, *task.SubmitRecord) {
	t.Helper()
	fake := newFakeSupervisor(t, mode)
	fixture := newTaskFixture(t, fake.client.Binding())
	record := createSubmission(t, fixture, fake.client.Binding())
	writeStatusFor(t, fake, record.RootID, record.TaskID, 7, state)
	identity := identityFromRecord(record)
	id := int64(7)
	identity.NumericTaskID = &id
	observation, err := fake.client.Reconcile(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.taskDir.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
	permit := createStopPermit(t, fixture)
	return fake, fixture, permit, record
}

type targetedStopCase struct {
	name      string
	mode      string
	state     State
	action    string
	ack       *bool
	wantError error
}

func TestTargetedStopRoutesStateAndSeparatesAcknowledgment(t *testing.T) {
	cases := []targetedStopCase{
		{name: "queued acknowledged remove", mode: "ok", state: StateQueued, action: "remove", ack: boolPointer(true)},
		{name: "running acknowledged kill", mode: "ok", state: StateRunning, action: "kill", ack: boolPointer(true)},
		{name: "queued refusal", mode: "stop-refuse", state: StateQueued, action: "remove", ack: boolPointer(false)},
		{name: "queued output uncertainty", mode: "stop-overflow", state: StateQueued, action: "remove", wantError: ErrControlLimit},
		{name: "ended no mutation", mode: "ok", state: StateEnded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runTargetedStopCase(t, tc) })
	}
}

func runTargetedStopCase(t *testing.T, tc targetedStopCase) {
	t.Helper()
	fake, _, permit, _ := prepareTargetedStop(t, tc.mode, tc.state)
	t.Cleanup(func() {
		if err := permit.Release(); err != nil {
			t.Error(err)
		}
	})
	result, err := fake.client.Stop(context.Background(), permit)
	assertTargetedStopError(t, tc, err)
	if result.ObservedState != tc.state || !result.Matched {
		t.Fatalf("wrong stop observation: %+v", result)
	}
	if result.Action != tc.action || result.Attempted != (tc.action != "") {
		t.Fatalf("wrong stop routing: %+v", result)
	}
	assertTargetedStopAcknowledgment(t, tc, result)
	if countCommand(readInvocations(t, fake.logPath), tc.action) != boolToInt(tc.action != "") {
		t.Fatalf("mutation count changed: %+v", readInvocations(t, fake.logPath))
	}
}

func assertTargetedStopError(t *testing.T, tc targetedStopCase, err error) {
	t.Helper()
	if tc.wantError != nil {
		if !errors.Is(err, tc.wantError) {
			t.Fatalf("unexpected stop error: %v", err)
		}
		return
	}
	if tc.mode == "stop-refuse" {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("refusal did not preserve CLI exit error: %v", err)
		}
		return
	}
	if tc.state != StateEnded && err != nil {
		t.Fatal(err)
	}
}

func assertTargetedStopAcknowledgment(t *testing.T, tc targetedStopCase, result StopResult) {
	t.Helper()
	if tc.ack == nil {
		if result.Acknowledged != nil {
			t.Fatalf("uncertain stop acquired acknowledgment: %+v", result)
		}
		return
	}
	if result.Acknowledged == nil || *result.Acknowledged != *tc.ack {
		t.Fatalf("wrong acknowledgment: %+v", result)
	}
}

func TestExplicitResolutionMustMatchChildEnvironment(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "ok", "pueue "+FixtureVersion)
	resolution, err := CurrentResolutionContext()
	if err != nil {
		t.Fatal(err)
	}
	changedHome := filepath.Join(t.TempDir(), "different-home")
	environment := append(append([]string{}, fake.environment...), "HOME="+changedHome)
	if _, bindErr := Bind(context.Background(), fake.executable, fake.configPath, Options{Environment: environment, Resolution: &resolution}); !errors.Is(bindErr, ErrConfiguration) {
		t.Fatalf("accepted resolution for different child environment: %v", bindErr)
	}
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{Environment: fake.environment, Resolution: &resolution})
	if err != nil {
		t.Fatal(err)
	}
	if client.Binding().ResolvedConfigSHA256 == "" {
		t.Fatal("matching explicit environment lost resolved binding")
	}
}

func TestFreshBindingDetectsConfigDrift(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	data, err := os.ReadFile(fake.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if unmarshalErr := json.Unmarshal(data, &document); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	document["client"] = map[string]any{"dark_mode": true}
	changed, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake.configPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	if _, err := fake.client.Reconcile(context.Background(), identity); !errors.Is(err, ErrBinding) {
		t.Fatalf("config drift was accepted: %v", err)
	}
}

func TestFreshBindingDetectsExecutableDrift(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	file, err := os.OpenFile(fake.executable, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.client.Reconcile(context.Background(), testIdentity()); !errors.Is(err, ErrBinding) {
		t.Fatalf("executable drift was accepted: %v", err)
	}
}

func TestFreshBindingDetectsResolutionDrift(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "ok", "pueue "+FixtureVersion)
	if err := os.WriteFile(fake.configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	bound, err := Bind(context.Background(), fake.executable, fake.configPath, Options{Environment: fake.environment})
	if err != nil {
		t.Fatal(err)
	}
	changed := append(append([]string{}, fake.environment...), "HOME="+filepath.Join(t.TempDir(), "different-home"))
	client, err := NewClient(bound.Binding(), Options{Environment: changed, ObservationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Reconcile(context.Background(), testIdentity()); !errors.Is(err, ErrBinding) {
		t.Fatalf("environment drift was accepted: %v", err)
	}
}

func testIdentity() Identity {
	return Identity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64)}
}

func TestNewClientRequiresEverySavedBindingComponent(t *testing.T) {
	base := validBindingForTest()
	cases := map[string]func(*task.SupervisorRef){
		"client executable": func(ref *task.SupervisorRef) { ref.ClientExecutable = "" },
		"client digest":     func(ref *task.SupervisorRef) { ref.ClientSHA256 = "" },
		"resolved digest":   func(ref *task.SupervisorRef) { ref.ResolvedConfigSHA256 = "" },
		"config path":       func(ref *task.SupervisorRef) { ref.ConfigPath = "" },
		"config digest":     func(ref *task.SupervisorRef) { ref.ConfigDigest = "" },
		"endpoint":          func(ref *task.SupervisorRef) { ref.Endpoint = "" },
		"version":           func(ref *task.SupervisorRef) { ref.ObservedVersion = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ref := base
			mutate(&ref)
			if _, err := NewClient(ref, Options{}); err == nil {
				t.Fatal("incomplete binding accepted")
			}
		})
	}
}

func TestOpenRegularRefusesRacedPipeAndFinalSymlink(t *testing.T) {
	base := t.TempDir()
	fifo := filepath.Join(base, "config.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := readRegular(fifo, MaxControlBytes)
		finished <- err
	}()
	select {
	case err := <-finished:
		if !errors.Is(err, ErrConfiguration) {
			t.Fatalf("FIFO refusal lost configuration error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("FIFO read blocked instead of refusing")
	}
	target := filepath.Join(base, "target")
	if err := os.WriteFile(target, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(link, MaxControlBytes); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("final symlink was followed: %v", err)
	}
}

func boolPointer(value bool) *bool { return &value }

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
