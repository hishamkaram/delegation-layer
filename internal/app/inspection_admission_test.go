package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInspectionDeadlineIsOperationalDispatchFailure(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	briefPath := filepath.Join(request.CanonicalCwd, "brief.md")
	writeAppTestFile(t, briefPath, []byte("inspection brief"))
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"dispatch", "--json", "--root", store.Root,
		"--provider", request.Provider, "--brief", briefPath,
		"--cwd", request.CanonicalCwd, "--id", request.TaskID,
	}, &stdout, &stderr, Dependencies{
		PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			return commonprovider.ProfileCandidate{}, errors.Join(errors.New("inspection preparation"), inspection.ErrAdmissionExpired)
		},
	})
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("deadline exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.TaskID != request.TaskID || response.Admission != "unknown" || response.Outcome != nil {
		t.Fatalf("deadline invented terminal authority: %+v", response)
	}
	assertAppInspectionNoOrdinaryTasks(t, store)
}

func TestInspectionPollingStopsWhenSupervisorCommandIsInFlight(t *testing.T) {
	pending := &pueue.Pending{}
	if !shouldStopInspectionPolling(&pueue.InFlightError{Pending: pending}) {
		t.Fatal("in-flight supervisor command remained pollable")
	}
	if shouldStopInspectionPolling(&pueue.InFlightError{}) {
		t.Fatal("in-flight error without a retained command stopped polling")
	}
	if shouldStopInspectionPolling(pueue.ErrUnknown) {
		t.Fatal("ordinary observation uncertainty stopped polling")
	}
}

func TestSupervisorOptionsForCandidateKeepOrdinaryTimeout(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := commonprovider.FingerprintExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	candidate := commonprovider.ProfileCandidate{Inspection: &commonprovider.InspectionDefinition{
		Revision: commonprovider.RuntimeInspectionRevision, Executable: executable,
		ExecutableSHA256: digest, Arguments: []string{"--version"}, Directory: workspace,
		Environment: []string{}, OutputLimit: commonprovider.MaxInspectionOutput,
		Project: func([]byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	}}
	options, err := supervisorOptionsForCandidate(pueue.Options{ObservationTimeout: time.Millisecond}, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if options.ObservationTimeout != time.Millisecond {
		t.Fatalf("ordinary observation timeout=%s, want %s", options.ObservationTimeout, time.Millisecond)
	}
	longer, err := supervisorOptionsForCandidate(pueue.Options{ObservationTimeout: time.Minute}, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if longer.ObservationTimeout != time.Minute {
		t.Fatalf("explicit longer observation timeout=%s was reduced", longer.ObservationTimeout)
	}
	if _, err = supervisorOptionsForCandidate(pueue.Options{ObservationTimeout: -time.Second}, candidate); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("negative observation timeout was accepted: %v", err)
	}
}

func TestRuntimeInspectionBindingKeepsCapabilityContractAcrossIdentityRefresh(t *testing.T) {
	base := inspection.Binding{DefinitionRevision: commonprovider.RuntimeInspectionRevision, CapabilitySHA256: strings.Repeat("a", 64), Environment: []string{"LANG=C"}, EnvironmentRecorded: true}
	changed := base
	changed.CapabilitySHA256 = strings.Repeat("b", 64)
	if sameInspectionBindingBase(base, changed) {
		t.Fatal("runtime capability contract drift was accepted")
	}
	identityRefresh := base
	identityRefresh.DefinitionSHA256 = strings.Repeat("c", 64)
	if !sameInspectionBindingBase(base, identityRefresh) {
		t.Fatal("runtime executable identity refresh was rejected")
	}
	legacy := base
	legacy.CapabilitySHA256 = ""
	if !sameInspectionBindingBase(base, legacy) {
		t.Fatal("legacy runtime binding was not left for a fresh capability probe")
	}
	legacyChanged := legacy
	legacyChanged.DefinitionSHA256 = strings.Repeat("d", 64)
	if sameInspectionBindingBase(base, legacyChanged) {
		t.Fatal("markerless runtime binding relaxed executable identity")
	}
	legacyNonEmpty := base
	legacyNonEmpty.EnvironmentRecorded = false
	if !sameInspectionBindingBase(base, legacyNonEmpty) {
		t.Fatal("legacy non-empty environment binding was not normalized as recorded")
	}
	explicitEmpty := base
	explicitEmpty.Environment = []string{}
	explicitEmpty.EnvironmentRecorded = true
	legacyEmpty := explicitEmpty
	legacyEmpty.EnvironmentRecorded = false
	if sameInspectionBindingBase(explicitEmpty, legacyEmpty) {
		t.Fatal("inherited and explicit-empty environment bindings were treated as equivalent")
	}
	mixed := inspection.Binding{
		DefinitionRevision: commonprovider.RuntimeInspectionRevision,
		DefinitionSHA256:   strings.Repeat("a", 64),
		HelperExecutable:   "/usr/bin/security",
		HelperSHA256:       strings.Repeat("c", 64),
		Environment:        []string{"LANG=C"},
	}
	mixedChanged := mixed
	mixedChanged.DefinitionSHA256 = strings.Repeat("b", 64)
	mixedChanged.HelperExecutable = "/usr/bin/other-helper"
	if sameInspectionBindingBase(mixed, mixedChanged) {
		t.Fatal("mixed native/runtime binding relaxed its native contract")
	}
}

func TestAdmissionRejectsCompletedRuntimeProofAfterIdentityDrift(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	supervisor, _ := newBlockingInspectionSupervisor(t, store.RootID)
	oldBinding := inspection.Binding{
		DefinitionRevision: commonprovider.RuntimeInspectionRevision,
		DefinitionSHA256:   strings.Repeat("a", 64),
		CapabilitySHA256:   strings.Repeat("b", 64),
		HelperExecutable:   "/usr/bin/true",
		HelperSHA256:       strings.Repeat("c", 64),
		WorkerExecutable:   "/usr/bin/true",
		WorkerSHA256:       strings.Repeat("d", 64),
		Environment:        []string{"LANG=C"},
		Supervisor:         supervisor.Binding(),
	}
	openedAt := time.Now().UTC()
	operation, err := inspection.OpenOperation(store, request, oldBinding, openedAt)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := operation.ClaimSubmissionContext(context.Background(), openedAt.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	start, err := operation.ClaimStart(openedAt.Add(2 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.Complete(inspection.ResultEligible, []byte(`{"eligible":true}`), openedAt.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(17); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordWorkerSuccess(17); err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}

	currentBinding := oldBinding
	currentBinding.DefinitionSHA256 = strings.Repeat("e", 64)
	replayed, err := openAdmissionInspectionOperation(store, request, currentBinding, time.Now().UTC())
	if replayed != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("completed runtime proof crossed identity drift: operation=%v err=%v", replayed, err)
	}
}

func TestAdmissionReplayReprobesIncompleteMarkerlessRuntimeAfterRefresh(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	supervisor, _ := newBlockingInspectionSupervisor(t, store.RootID)
	oldBinding := inspection.Binding{
		DefinitionRevision: commonprovider.RuntimeInspectionRevision,
		DefinitionSHA256:   strings.Repeat("a", 64),
		HelperExecutable:   "/usr/bin/provider-old",
		HelperSHA256:       strings.Repeat("b", 64),
		WorkerExecutable:   "/usr/bin/true",
		WorkerSHA256:       strings.Repeat("c", 64),
		Environment:        []string{"LANG=C"},
		Supervisor:         supervisor.Binding(),
	}
	operation, err := inspection.OpenOperation(store, request, oldBinding, time.Unix(100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}

	currentBinding := oldBinding
	currentBinding.DefinitionSHA256 = strings.Repeat("d", 64)
	currentBinding.CapabilitySHA256 = strings.Repeat("e", 64)
	currentBinding.HelperExecutable = "/usr/bin/provider-new"
	currentBinding.HelperSHA256 = strings.Repeat("f", 64)
	currentBinding.RunnerOwnership = task.RunnerOwnershipManaged
	currentBinding.EnvironmentRecorded = true
	replayed, err := openAdmissionInspectionOperation(store, request, currentBinding, time.Unix(101, 0).UTC())
	if err != nil {
		t.Fatalf("incomplete markerless runtime journal was not reopened for re-probe: %v", err)
	}
	defer func() {
		if closeErr := replayed.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if got := replayed.Request().Binding; !reflect.DeepEqual(got, oldBinding) {
		t.Fatalf("replay mutated immutable markerless binding: got=%+v want=%+v", got, oldBinding)
	}
}

func TestEnsureInspectionGroupReconcilesUncertainCreate(t *testing.T) {
	store := newBareInspectionStore(t)
	client, logPath, donePath := newUncertainGroupSupervisor(t, store.RootID)

	if err := ensureInspectionGroup(store, client, inspection.AdmissionTimeout); err != nil {
		t.Fatalf("uncertain group creation was not reconciled: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(logData), "arg=group\n"); count != 1 {
		t.Fatalf("group creation was retried %d times: %s", count, logData)
	}
	if count := strings.Count(string(logData), "arg=status\n"); count != 2 {
		t.Fatalf("expected initial and reconciliation snapshots, got %d: %s", count, logData)
	}
	if _, err := os.Stat(donePath); err != nil {
		t.Fatalf("uncertain create was not joined before reconciliation: %v", err)
	}
}

func TestAwaitInspectionJoinsInFlightSupervisorObservation(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	request.TaskID = strings.Repeat("b", 32)
	client, logPath := newBlockingInspectionSupervisor(t, store.RootID)
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: "/usr/bin/true", WorkerSHA256: digest,
		Supervisor: client.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	facts, err := awaitInspection(ctx, operation, client)
	assertJoinedInspectionPending(t, err)
	if facts != nil || !errors.Is(err, pueue.ErrInFlight) {
		t.Fatalf("in-flight observation result facts=%s err=%v", facts, err)
	}
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 1 {
		t.Fatalf("status command was launched %d times while first remained in flight: %s", count, invocations)
	}
}

func TestSubmitInspectionOnceJoinsInFlightSupervisorCommand(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	request.TaskID = strings.Repeat("c", 32)
	client, logPath := newBlockingInspectionSupervisor(t, store.RootID)
	runner, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	workerSHA, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: runner, WorkerSHA256: workerSHA,
		Supervisor: client.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = submitInspectionOnce(ctx, operation, client, runner, store.Root)
	assertJoinedInspectionPending(t, err)
	if !errors.Is(err, pueue.ErrInFlight) {
		t.Fatalf("in-flight submission returned %v", err)
	}
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 1 {
		t.Fatalf("status command was launched %d times while submission remained in flight: %s", count, invocations)
	}
}

func TestSubmitInspectionOnceRejectsWorkerChangedAfterClaim(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	request.TaskID = strings.Repeat("d", 32)
	client, logPath := newBlockingInspectionSupervisor(t, store.RootID)
	runner, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		t.Fatal(err)
	}
	definitionDigest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: definitionDigest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: definitionDigest,
		WorkerExecutable: runner, WorkerSHA256: digest,
		Supervisor: client.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	changedRunner := filepath.Join(t.TempDir(), "changed-worker")
	if err = os.WriteFile(changedRunner, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	changedRunner, err = filepath.EvalSymlinks(changedRunner)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = submitInspectionOnce(ctx, operation, client, changedRunner, store.Root)
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("changed worker reached supervisor boundary: %v", err)
	}
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 0 {
		t.Fatalf("changed worker launched supervisor observation %d times: %s", count, invocations)
	}
}

func TestValidateInspectionRunnerRejectsDigestDrift(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	request.TaskID = strings.Repeat("e", 32)
	client, logPath := newBlockingInspectionSupervisor(t, store.RootID)
	runner, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: runner, WorkerSHA256: digest,
		Supervisor: client.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = validateInspectionRunner(operation, runner); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("worker digest drift was accepted: %v", err)
	}
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 0 {
		t.Fatalf("worker digest check launched supervisor observation %d times: %s", count, invocations)
	}
}

func assertJoinedInspectionPending(t *testing.T, err error) {
	t.Helper()
	var inFlight *pueue.InFlightError
	if !errors.As(err, &inFlight) || inFlight == nil || inFlight.Pending == nil {
		t.Fatalf("missing retained in-flight supervisor command: %v", err)
	}
	select {
	case <-inFlight.Pending.Done():
		return
	default:
		// The command is finite in these tests; drain it before failing so the
		// test never leaves an owned supervisor process behind.
		<-inFlight.Pending.Done()
		t.Fatal("returned before joining in-flight supervisor command")
	}
}

func newBlockingInspectionSupervisor(t *testing.T, rootID string) (*pueue.Client, string) {
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

func newUncertainGroupSupervisor(t *testing.T, rootID string) (*pueue.Client, string, string) {
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
	configPath := filepath.Join(base, "pueue.json")
	statusPath := filepath.Join(base, "status.json")
	afterPath := filepath.Join(base, "status-after.json")
	donePath := filepath.Join(base, "group-done")
	logPath := filepath.Join(base, "invocations.log")
	script := `#!/bin/sh
set -eu
{
  printf '%s\n' '---'
  for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
} >> "$FAKE_LOG"
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status) cat "$FAKE_STATUS" ;;
  group)
    [ "${4-}" = add ] && [ "${5-}" = --parallel ] && [ "${6-}" = 1 ]
    sleep 1
    cp "$FAKE_AFTER" "$FAKE_STATUS"
    : > "$FAKE_DONE"
    ;;
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
	if writeErr := os.WriteFile(statusPath, []byte(`{"tasks":{},"groups":{}}`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	group := "delegation-inspection-" + rootID
	after := []byte(`{"tasks":{},"groups":{"` + group + `":{"status":"Running","parallel_tasks":1}}}`)
	if writeErr := os.WriteFile(afterPath, after, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	environment := append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath, "FAKE_AFTER="+afterPath, "FAKE_DONE="+donePath)
	bound, err := pueue.Bind(context.Background(), executable, configPath, pueue.Options{Environment: environment, ObservationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client, err := pueue.NewClient(bound.Binding(), pueue.Options{Environment: environment, ObservationTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return client, logPath, donePath
}

func TestInspectionAdmissionRejectsStaticCandidateBeforeSupervisorBinding(t *testing.T) {
	tests := []struct {
		name      string
		candidate func(task.TaskRecord, *taskdir.Store) commonprovider.ProfileCandidate
	}{
		{
			name: "candidate directory mismatch",
			candidate: func(request task.TaskRecord, _ *taskdir.Store) commonprovider.ProfileCandidate {
				return appInvalidInspectionCandidate(request, filepath.Join(request.CanonicalCwd, "unexpected"), nil, nil)
			},
		},
		{
			name: "writable state overlap",
			candidate: func(request task.TaskRecord, store *taskdir.Store) commonprovider.ProfileCandidate {
				return appInvalidInspectionCandidate(request, request.CanonicalCwd, []string{store.Root}, nil)
			},
		},
		{
			name: "malformed inspection definition",
			candidate: func(request task.TaskRecord, _ *taskdir.Store) commonprovider.ProfileCandidate {
				definition := &commonprovider.InspectionDefinition{
					Revision:    "",
					Executable:  "relative-helper",
					Directory:   request.CanonicalCwd,
					Environment: []string{},
					OutputLimit: 1024,
					Project: func([]byte) (json.RawMessage, error) {
						return json.RawMessage(`{"eligible":true}`), nil
					},
				}
				return appInvalidInspectionCandidate(request, request.CanonicalCwd, nil, definition)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newBareInspectionStore(t)
			request := newBareInspectionRequest(t, store)
			candidate := test.candidate(request, store)
			deps := Dependencies{
				PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
					return candidate, nil
				},
				InitialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue"),
			}
			args := Arguments{PueueConfig: filepath.Join(t.TempDir(), "missing-pueue.yml")}
			if _, err := prepareAdmission(args, deps, store, request); !errors.Is(err, ErrProfileUnavailable) {
				t.Fatalf("static candidate crossed supervisor binding: %v", err)
			}
			assertAppInspectionNoOrdinaryTasks(t, store)
		})
	}
}

func appInvalidInspectionCandidate(request task.TaskRecord, directory string, writableRoots []string, definition *commonprovider.InspectionDefinition) commonprovider.ProfileCandidate {
	return commonprovider.ProfileCandidate{
		Directory:     directory,
		WritableRoots: writableRoots,
		Inspection:    definition,
		Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
			return appInspectionPreparedProfile(request, directory, writableRoots), nil
		},
	}
}

func newBareInspectionStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
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

func newBareInspectionRequest(t *testing.T, store *taskdir.Store) task.TaskRecord {
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
