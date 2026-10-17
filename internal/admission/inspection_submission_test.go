package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSubmitInspectionOnceUsesConcurrentAddReceiptWithoutQueueSnapshot(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	supervisor, statusCalls := newTerminalEventInspectionSupervisor(t, store)
	runner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner, err = filepath.EvalSymlinks(runner)
	if err != nil {
		t.Fatal(err)
	}
	runnerSHA, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		t.Fatal(err)
	}
	request := supervisorfixture.NewInspectionRequest(t, store)
	operation, err := inspection.OpenConcurrentOperation(store, request, inspection.Binding{
		DefinitionRevision: commonprovider.RuntimeInspectionRevision,
		DefinitionSHA256:   task.ComputeSHA256([]byte("runtime-definition")),
		CapabilitySHA256:   task.ComputeSHA256([]byte("runtime-capability")),
		HelperExecutable:   runner,
		HelperSHA256:       runnerSHA,
		WorkerExecutable:   runner,
		WorkerSHA256:       runnerSHA,
		Supervisor:         supervisor.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	assertTerminalEventInspectionGenerationReady(t, store, supervisor, ctx)
	if _, err = submitInspectionOnceWithState(ctx, operation, supervisor, runner, store.Root); err != nil {
		t.Fatal(err)
	}
	receipt, err := operation.ReceiptContext(ctx)
	if err != nil || receipt.NumericTaskID == nil || *receipt.NumericTaskID != 9 {
		t.Fatalf("concurrent inspection receipt = %+v err=%v", receipt, err)
	}
	if calls := statusCalls.Load(); calls != 0 {
		t.Fatalf("concurrent submission made %d queue-wide status calls", calls)
	}
}

func assertTerminalEventInspectionGenerationReady(t *testing.T, store *taskdir.Store, supervisor *pueue.Client, ctx context.Context) {
	t.Helper()
	complete, completeErr := pueue.PrivateSupervisorMigrationComplete(store.Root, supervisor.Binding())
	if completeErr != nil || !complete {
		migration, migrationErr := pueue.PrivateSupervisorMigrationPending(store.Root)
		recovery, recoveryErr := pueue.PrivateRecoveryBindingPresent(store.Root)
		bootstrap, bootstrapErr := pueue.PrivateBootstrapPending(store.Root)
		daemon, daemonErr := pueue.PrivateDaemonIdentityPresent(store.Root)
		entries, readErr := os.ReadDir(filepath.Join(store.Root, ".supervisor"))
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("private generation incomplete: complete=%t err=%v migration=%t/%v recovery=%t/%v bootstrap=%t/%v daemon=%t/%v files=%v read=%v binding=%+v runtime=%+v", complete, completeErr, migration, migrationErr, recovery, recoveryErr, bootstrap, bootstrapErr, daemon, daemonErr, names, readErr, supervisor.Binding(), supervisor.RuntimeBinding())
	}
	recorded, recordErr := supervisorcore.OrdinarySchedulingRecorded(store.Root, supervisor.Binding())
	if recordErr != nil || !recorded {
		t.Fatalf("private scheduling unrecorded: recorded=%t err=%v binding=%+v runtime=%+v", recorded, recordErr, supervisor.Binding(), supervisor.RuntimeBinding())
	}
	if _, gateErr := supervisorcore.SubmissionGate(store.Root)(ctx, supervisor.Binding()); gateErr != nil {
		t.Fatalf("submission gate: %v", gateErr)
	}
}

func TestAdmissionInspectionReplayAcceptsAuthorizedManagedSuccessor(t *testing.T) {
	fixture := admissionfixture.NewQueuedInspectionUpgradeFixture(t)
	operation, err := OpenInspectionOperationContext(t.Context(), fixture.Store, fixture.Request, fixture.Binding, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	original := operation.Request()
	claim, err := operation.ClaimSubmissionContext(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = claim.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}

	workerDigest, err := commonprovider.FingerprintExecutable(fixture.Replacement.NewRunner)
	if err != nil {
		t.Fatal(err)
	}
	binding := original.Binding
	binding.WorkerExecutable = fixture.Replacement.NewRunner
	binding.WorkerSHA256 = workerDigest
	if _, err = OpenInspectionOperationContext(t.Context(), fixture.Store, fixture.Request, binding, time.Now()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unverified managed successor was accepted: %v", err)
	}

	if err = supervisorcore.AuthorizeQueuedInspectionRunnerUpgrade(t.Context(), fixture.Store, fixture.Replacement); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(fixture.OldRunner); err != nil {
		t.Fatal(err)
	}
	operation, err = OpenInspectionOperationContext(t.Context(), fixture.Store, fixture.Request, binding, time.Now().UTC())
	if err != nil {
		t.Fatalf("authorized inspection admission replay failed: %v", err)
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	replayed := operation.Request()
	if replayed.Binding.WorkerExecutable != original.Binding.WorkerExecutable || replayed.Binding.WorkerSHA256 != original.Binding.WorkerSHA256 {
		t.Fatalf("replay changed immutable worker binding: before=%+v after=%+v", original.Binding, replayed.Binding)
	}
	assertIncompleteInspectionReplayHasNoStartOrCompletion(t, fixture.Store, operation)
	before, err := os.ReadFile(fixture.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = submitInspectionOnceWithState(t.Context(), operation, fixture.Supervisor, binding.WorkerExecutable, fixture.Store.Root); err != nil {
		t.Fatalf("already-submitted inspection was not reconciled through its original journal: %v", err)
	}
	after, err := os.ReadFile(fixture.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("already-submitted replay launched another supervisor command: before=%q after=%q", before, after)
	}
	assertIncompleteInspectionReplayHasNoStartOrCompletion(t, fixture.Store, operation)
}

func assertIncompleteInspectionReplayHasNoStartOrCompletion(t *testing.T, store *taskdir.Store, operation *inspection.Operation) {
	t.Helper()
	if _, err := operation.ReceiptContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete replay unexpectedly has a receipt: %v", err)
	}
	if _, _, err := operation.ReadCompletedContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete replay unexpectedly has completion evidence: %v", err)
	}
	control, err := store.OpenInspectionContext(t.Context(), operation.Request().TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	var start inspection.StartRecord
	err = control.Read("start.json", &start)
	closeErr := control.Close()
	if !errors.Is(err, os.ErrNotExist) || closeErr != nil {
		t.Fatalf("incomplete replay start evidence err=%v close=%v", err, closeErr)
	}
}
