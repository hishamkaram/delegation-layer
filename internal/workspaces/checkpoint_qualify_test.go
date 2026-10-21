package workspaces

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// Checkpoint closure stages over the production operation journal.
const (
	checkpointNative              = "registered-native"
	checkpointLegacy              = "legacy-previous-boot"
	checkpointRegisteredRecovered = "registered-recovered"
	checkpointUnregistered        = "unregistered-recovered"
	checkpointUnlaunched          = "unlaunched-fence"
)

func newCheckpointStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, _ := workspacefixture.NewOperation(t)
	return store
}

func ordinaryCheckpointFixtureSelection() workspace.CheckpointSelection {
	return workspace.CheckpointSelection{Kind: workspace.CheckpointOrdinary, Cwd: "/repo/sub", CwdIdentity: &workspace.DirectoryIdentity{Device: 3, Inode: 4}}
}

func openCheckpointCreation(t *testing.T, store *taskdir.Store, operationID, workspaceID string, selection workspace.CheckpointSelection, deadline time.Time) *workspace.Operation {
	t.Helper()
	subject, err := checkpointIntentSubject(workspaceID, strings.Repeat("9", 32), selection)
	if err != nil {
		t.Fatal(err)
	}
	request := workspacefixture.CheckpointOperationRequest(store, operationID, workspace.OperationCheckpoint, workspaceID, subject, &selection, deadline)
	return workspacefixture.OpenCheckpointOperation(t, store, request)
}

func openOrdinaryCheckpoint(t *testing.T, store *taskdir.Store, operationID string, deadline time.Time) *workspace.Operation {
	t.Helper()
	return openCheckpointCreation(t, store, operationID, "", ordinaryCheckpointFixtureSelection(), deadline)
}

func ordinaryCheckpointTarget() workspace.CheckpointTarget {
	source := workspacefixture.OrdinaryCheckpointFixtureSource("/repo")
	return workspace.CheckpointTarget{Source: source, CheckoutRoot: source.Root, AdminDir: source.GitDir}
}

// recordCheckpointWork publishes what a successful worker leaves: the
// candidate inside the live lifetime, then its successful result.
func recordCheckpointWork(t *testing.T, operation *workspace.Operation, target *workspace.CheckpointTarget) {
	t.Helper()
	if target != nil {
		if err := operation.RecordCheckpointCandidate(t.Context(), workspacefixture.CheckpointFixtureCandidate(t, operation, *target)); err != nil {
			t.Fatal(err)
		}
	}
	if err := operation.RecordResult(t.Context(), workspacefixture.SucceededResult(operation)); err != nil {
		t.Fatal(err)
	}
}

// closeCheckpointLifetime closes operation's lifetime as stage. live runs while
// a worker could still publish evidence.
func closeCheckpointLifetime(t *testing.T, store *taskdir.Store, operation *workspace.Operation, stage string, live func()) {
	t.Helper()
	switch stage {
	case checkpointNative:
		workspacefixture.RunRealGuardianLifetime(t, store, operation, live)
		return
	case checkpointUnlaunched:
		if _, err := OperationClosed(t.Context(), operation); err != nil {
			t.Fatal(err)
		}
		return
	case checkpointRegisteredRecovered:
		closeCheckpointLifetime(t, store, operation, checkpointLegacy, live)
		publishPreviousBootAbsence(t, operation)
		return
	}
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	facts := taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "checkpoint", BootID: "checkpoint-boot", Deadline: operation.Deadline()}
	if _, err = operation.RecordLifetimeIntentContext(t.Context(), facts); err != nil {
		t.Fatal(err)
	}
	if stage == checkpointUnregistered {
		if _, err = operation.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceUnregisteredDisposed, facts.BootID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err = operation.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{ScopeKind: facts.ScopeKind, Assurance: facts.Assurance, ScopeID: "checkpoint-scope", BootID: facts.BootID, Deadline: facts.Deadline}); err != nil {
		t.Fatal(err)
	}
	live()
	if _, err = operation.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationPreviousBoot); err != nil {
		t.Fatal(err)
	}
}

func publishPreviousBootAbsence(t *testing.T, operation *workspace.Operation) {
	t.Helper()
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, absenceErr := operation.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsencePreviousBoot, "next-checkpoint-boot")
	if err = errors.Join(absenceErr, owner.Release()); err != nil {
		t.Fatal(err)
	}
}

// TestCheckpointSuccessRequiresNativeClosureWhereGenericAcceptanceDoesNot
// pairs the unchanged generic acceptance used by historical capture with the
// strict checkpoint qualifier over the same closure facts and successful
// worker evidence.
func TestCheckpointSuccessRequiresNativeClosureWhereGenericAcceptanceDoesNot(t *testing.T) {
	cases := []struct {
		stage, status, reason string
		kind                  task.LifetimeClosureKind
		genericErr            error
	}{
		{checkpointNative, workspace.CheckpointCheckpointed, "", "", nil},
		{checkpointLegacy, workspace.CheckpointUnavailable, workspace.CheckpointResultUnavailable, task.LifetimeClosureLegacyPreviousBoot, nil},
		{checkpointRegisteredRecovered, workspace.CheckpointUnavailable, workspace.CheckpointResultUnavailable, "", nil},
		{checkpointUnregistered, workspace.CheckpointUnavailable, workspace.CheckpointNotLaunched, task.LifetimeClosureUnregisteredRecovered, task.ErrLifetimeUnlaunched},
		{checkpointUnlaunched, workspace.CheckpointUnavailable, workspace.CheckpointNotLaunched, "", task.ErrLifetimeUnlaunched},
	}
	for _, test := range cases {
		t.Run(test.stage, func(t *testing.T) {
			store := newCheckpointStore(t)
			deadline := time.Now().Add(time.Hour)
			if test.stage == checkpointUnlaunched {
				deadline = time.Now().Add(-time.Second)
			}
			operation := openOrdinaryCheckpoint(t, store, strings.Repeat("1", 32), deadline)
			target := ordinaryCheckpointTarget()
			closeCheckpointLifetime(t, store, operation, test.stage, func() { recordCheckpointWork(t, operation, &target) })
			if _, _, err := AcceptedOperation(t.Context(), store, operation); !errors.Is(err, test.genericErr) || (test.genericErr == nil && err != nil) {
				t.Fatalf("generic acceptance = %v, want %v", err, test.genericErr)
			}
			receipt, err := QualifyCheckpoint(t.Context(), store, operation)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Status != test.status || receipt.ReasonCode != test.reason || receipt.LifetimeClosure != test.kind {
				t.Fatalf("checkpoint receipt = %+v", receipt)
			}
			requireStoredCheckpointReceipt(t, operation, receipt)
		})
	}
}

// requireStoredCheckpointReceipt asserts receipt is the stored outcome and
// that a checkpointed receipt carries its candidate and exact Wait proof.
func requireStoredCheckpointReceipt(t *testing.T, operation *workspace.Operation, receipt workspace.CheckpointReceipt) {
	t.Helper()
	if receipt.Status == workspace.CheckpointCheckpointed && (receipt.Candidate == nil || receipt.WorkerWaitSHA256 == "" || receipt.TerminationSHA256 == "") {
		t.Fatalf("native checkpoint lacks its candidate or exact Wait proof: %+v", receipt)
	}
	if stored, err := operation.CheckpointReceipt(t.Context()); err != nil || stored.Status != receipt.Status || stored.ReasonCode != receipt.ReasonCode {
		t.Fatalf("stored receipt = %+v, %v", stored, err)
	}
}

func TestCheckpointHeldResultBeforeExactWaitStaysPending(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openOrdinaryCheckpoint(t, store, strings.Repeat("1", 32), time.Now().Add(time.Hour))
	target := ordinaryCheckpointTarget()
	fixture := workspacefixture.GuardianWithRealRecords(t, store, operation)
	fixture.Hello(t)
	recordCheckpointWork(t, operation, &target)
	if _, err := QualifyCheckpoint(t.Context(), store, operation); !PendingError(err) {
		t.Fatalf("live checkpoint with a result = %v, want pending", err)
	}
	if _, err := operation.CheckpointReceipt(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("pending checkpoint published a receipt: %v", err)
	}
	fixture.RunCommand(t)
	fixture.CloseTask(t)
	fixture.ReleaseWorker()
	if err := fixture.WaitRun(t); err != nil {
		t.Fatal(err)
	}
	if receipt, err := QualifyCheckpoint(t.Context(), store, operation); err != nil || receipt.Status != workspace.CheckpointCheckpointed {
		t.Fatalf("closed native checkpoint = %+v, %v", receipt, err)
	}
}

func TestCheckpointReceiptStoredFirstSurvivesChangedClosureFacts(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openOrdinaryCheckpoint(t, store, strings.Repeat("1", 32), time.Now().Add(time.Hour))
	target := ordinaryCheckpointTarget()
	closeCheckpointLifetime(t, store, operation, checkpointLegacy, func() { recordCheckpointWork(t, operation, &target) })
	first, err := QualifyCheckpoint(t.Context(), store, operation)
	if err != nil || first.LifetimeClosure != task.LifetimeClosureLegacyPreviousBoot {
		t.Fatalf("termination-only receipt = %+v, %v", first, err)
	}
	publishPreviousBootAbsence(t, operation)
	_, proof, err := AcceptedOperation(t.Context(), store, operation)
	if err != nil || receiptClosureEvidence(proof).Kind == first.LifetimeClosure {
		t.Fatalf("absence did not change the recomputed closure facts: %v, %v", proof.Kind(), err)
	}
	second, err := QualifyCheckpoint(t.Context(), store, operation)
	if err != nil || second != first {
		t.Fatalf("stored receipt was not authoritative: %+v, %v; want %+v", second, err, first)
	}
}

func TestCheckpointReceiptConcurrentFinalizersConvergeOnStoredWinner(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openOrdinaryCheckpoint(t, store, strings.Repeat("1", 32), time.Now().Add(time.Hour))
	target := ordinaryCheckpointTarget()
	closeCheckpointLifetime(t, store, operation, checkpointNative, func() { recordCheckpointWork(t, operation, &target) })
	const callers = 6
	receipts, errs := make([]workspace.CheckpointReceipt, callers), make([]error, callers)
	done := make(chan struct{}, callers)
	for index := range callers {
		go func() {
			receipts[index], errs[index] = QualifyCheckpoint(t.Context(), store, operation)
			done <- struct{}{}
		}()
	}
	for range callers {
		<-done
	}
	stored, err := operation.CheckpointReceipt(t.Context())
	if err != nil || stored.Status != workspace.CheckpointCheckpointed {
		t.Fatalf("stored winner = %+v, %v", stored, err)
	}
	for index := range callers {
		if errs[index] != nil || receipts[index].Status != stored.Status || *receipts[index].Candidate != *stored.Candidate {
			t.Fatalf("caller %d = %+v, %v; want stored winner", index, receipts[index], errs[index])
		}
	}
}

// linkedRecordFault fails after the named record is linked and its directory
// barrier is durable: an independent cleanup fault that the durable record
// must not hide. run, when set, executes once at that point first.
type linkedRecordFault struct {
	taskdir.BaseFaultInjector
	name string
	run  func()
	err  error
}

func (f *linkedRecordFault) OnAfterLinkDirBarrier(destination string) error {
	if !strings.HasSuffix(destination, f.name) {
		return nil
	}
	if run := f.run; run != nil {
		f.run = nil
		run()
	}
	return f.err
}

func TestCheckpointReceiptLinkedWinnerWithCleanupFaultFailsThenConverges(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openOrdinaryCheckpoint(t, store, strings.Repeat("1", 32), time.Now().Add(time.Hour))
	target := ordinaryCheckpointTarget()
	closeCheckpointLifetime(t, store, operation, checkpointNative, func() { recordCheckpointWork(t, operation, &target) })
	injected := errors.New("injected receipt stage cleanup fault")
	store.SetFaultInjector(&linkedRecordFault{name: "checkpoint-receipt.json", err: injected})
	if _, err := QualifyCheckpoint(t.Context(), store, operation); !errors.Is(err, injected) {
		t.Fatalf("linked receipt with cleanup fault = %v, want the complete fault", err)
	}
	store.SetFaultInjector(nil)
	stored, err := operation.CheckpointReceipt(t.Context())
	if err != nil || stored.Status != workspace.CheckpointCheckpointed {
		t.Fatalf("linked winner = %+v, %v", stored, err)
	}
	if again, againErr := QualifyCheckpoint(t.Context(), store, operation); againErr != nil || again.Status != stored.Status || *again.Candidate != *stored.Candidate {
		t.Fatalf("clean call did not converge on the stored winner: %+v, %v", again, againErr)
	}
}

func TestCheckpointQualifiedSuccessWithoutCandidateIsAnEvidenceFault(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openOrdinaryCheckpoint(t, store, strings.Repeat("1", 32), time.Now().Add(time.Hour))
	closeCheckpointLifetime(t, store, operation, checkpointNative, func() { recordCheckpointWork(t, operation, nil) })
	if _, err := QualifyCheckpoint(t.Context(), store, operation); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("success without a durable candidate = %v, want evidence fault", err)
	}
	if _, err := operation.CheckpointReceipt(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("inconsistent evidence produced a receipt: %v", err)
	}
}

func TestCheckpointCreationIntentBindsRequestFields(t *testing.T) {
	store := newCheckpointStore(t)
	selection := ordinaryCheckpointFixtureSelection()
	subject, err := checkpointIntentSubject("", strings.Repeat("9", 32), selection)
	if err != nil {
		t.Fatal(err)
	}
	other := selection
	other.Cwd = "/repo/other"
	request := workspacefixture.CheckpointOperationRequest(store, strings.Repeat("2", 32), workspace.OperationCheckpoint, "", subject, &other, time.Now().Add(time.Hour))
	forged := workspacefixture.OpenCheckpointOperation(t, store, request)
	if err = ValidateCheckpointAction(t.Context(), store, forged); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("subject from another selection = %v, want identity mismatch", err)
	}
	if err = SubmitOperation(t.Context(), store, forged, OperationOptions{Runtime: unavailableOperationRuntime{}}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("forged creation submission = %v, want refusal before any runtime use", err)
	}
	if _, _, err = AcceptedOperation(t.Context(), store, forged); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("forged creation acceptance = %v, want identity mismatch", err)
	}
}
