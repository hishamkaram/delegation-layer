package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	supervisor, _ := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
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
	currentBinding := oldBinding
	currentBinding.DefinitionSHA256 = strings.Repeat("e", 64)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = rejectCompletedRuntimeIdentityDrift(canceled, operation, oldBinding, currentBinding); !errors.Is(err, context.Canceled) {
		t.Fatalf("runtime drift check discarded caller cancellation: %v", err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	replayed, err := OpenInspectionOperationContext(t.Context(), store, request, currentBinding, time.Now().UTC())
	if replayed != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("completed runtime proof crossed identity drift: operation=%v err=%v", replayed, err)
	}
}

func TestAdmissionReplayReprobesIncompleteMarkerlessRuntimeAfterRefresh(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	supervisor, _ := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
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
	currentBinding := oldBinding
	currentBinding.DefinitionSHA256 = strings.Repeat("d", 64)
	currentBinding.CapabilitySHA256 = strings.Repeat("e", 64)
	currentBinding.HelperExecutable = "/usr/bin/provider-new"
	currentBinding.HelperSHA256 = strings.Repeat("f", 64)
	currentBinding.RunnerOwnership = task.RunnerOwnershipManaged
	currentBinding.EnvironmentRecorded = true
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	allowed, replayErr := allowIncompleteMarkerlessRuntimeReplay(canceled, operation, oldBinding, currentBinding)
	if allowed || !errors.Is(replayErr, context.Canceled) {
		t.Fatalf("markerless replay discarded caller cancellation: allowed=%v err=%v", allowed, replayErr)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	replayed, err := OpenInspectionOperationContext(t.Context(), store, request, currentBinding, time.Unix(101, 0).UTC())
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

func TestAwaitInspectionRetainsInFlightSupervisorObservation(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	request.TaskID = strings.Repeat("b", 32)
	bound, logPath := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := admissionfixture.NewObservedInspectionTestClient(t, bound, logPath, 10*time.Second, func(event pueue.CommandEvent) {
		if event.Stage == "started" && strings.Contains("\x00"+strings.Join(event.Argv, "\x00")+"\x00", "\x00status\x00") {
			cancel()
		}
	})
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
	facts, err := awaitInspectionWithSubmissionState(ctx, operation, client, inspectionSubmissionState{})
	pending := admissionfixture.AssertRetainedInspectionPending(t, err)
	if facts != nil || !errors.Is(err, pueue.ErrInFlight) {
		t.Fatalf("in-flight observation result facts=%s err=%v", facts, err)
	}
	admissionfixture.DrainInspectionPending(t, pending)
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 1 {
		t.Fatalf("status command was launched %d times while first remained in flight: %s", count, invocations)
	}
}

func TestSubmitInspectionOnceRetainsInFlightSupervisorCommand(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	request.TaskID = strings.Repeat("c", 32)
	bound, logPath := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := admissionfixture.NewObservedInspectionTestClient(t, bound, logPath, 10*time.Second, func(event pueue.CommandEvent) {
		if event.Stage == "started" && strings.Contains("\x00"+strings.Join(event.Argv, "\x00")+"\x00", "\x00status\x00") {
			cancel()
		}
	})
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
	_, err = submitInspectionOnceWithState(ctx, operation, client, runner, store.Root)
	pending := admissionfixture.AssertRetainedInspectionPending(t, err)
	if !errors.Is(err, pueue.ErrInFlight) {
		t.Fatalf("in-flight submission returned %v", err)
	}
	admissionfixture.DrainInspectionPending(t, pending)
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 1 {
		t.Fatalf("status command was launched %d times while submission remained in flight: %s", count, invocations)
	}
}

func TestInspectionLaunchAlwaysUsesGuardian(t *testing.T) {
	launch := guardedInspectionLaunch("/tmp/custom-delegate-run", "/tmp/delegate-state")
	if launch.RunnerExecutable != "/tmp/custom-delegate-run" || launch.RootPath != "/tmp/delegate-state" || !launch.Supervised {
		t.Fatalf("inspection launch is not guardian supervised: %+v", launch)
	}
}

func TestSubmitInspectionOnceRejectsWorkerChangedAfterClaim(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	request.TaskID = strings.Repeat("d", 32)
	client, logPath := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
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
	_, err = submitInspectionOnceWithState(ctx, operation, client, changedRunner, store.Root)
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
