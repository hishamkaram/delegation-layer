package app

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// endedCustodyInspection is one ended inspection row whose journal records a
// closed, placed lifetime with a bound view.
type endedCustodyInspection struct {
	store        *taskdir.Store
	supervisor   task.SupervisorRef
	job          pueue.Job
	legacy       inspection.GroupRequestRecord
	registration *task.LifetimeRegistrationRecord
}

func newEndedCustodyInspection(t *testing.T) endedCustodyInspection {
	t.Helper()
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	request.TaskID = strings.Repeat("b", 32)
	supervisor, _ := newBlockingInspectionSupervisor(t, store.RootID)
	digest := strings.Repeat("a", 64)
	openedAt := time.Now().UTC()
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: "/usr/bin/true", WorkerSHA256: digest,
		Supervisor: supervisor.Binding(),
	}, openedAt)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := operation.ClaimSubmission(openedAt.Add(time.Millisecond))
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
	if err = operation.Complete(inspection.ResultUnavailable, nil, openedAt.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(7); err != nil {
		t.Fatal(err)
	}
	registration := recordCustodyLifetimeUntil(t, operation, newCustodyTestPlacement(t), task.LifetimeTerminationCompleted, operation.Deadline())
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	bindCustodyTestView(t, registration)

	job := pueue.Job{ID: 7, State: pueue.StateEnded}
	label := (pueue.InspectionIdentity{RootID: store.RootID, TaskID: request.TaskID}).Label()
	job.Label = &label
	job.Group = (pueue.InspectionIdentity{RootID: store.RootID}).Group()
	return endedCustodyInspection{
		store: store, supervisor: supervisor.Binding(), job: job, registration: registration,
		legacy: inspection.GroupRequestRecord{RootID: store.RootID, Supervisor: supervisor.Binding(), Name: job.Group, ParallelTasks: 1},
	}
}

func (f endedCustodyInspection) removable(t *testing.T) bool {
	t.Helper()
	plan, err := authorizePrivateSuccessfulCleanup(context.Background(), f.store.Root, f.supervisor, []pueue.Job{f.job})
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(plan.RemoveIDs, f.job.ID)
}

func TestInspectionCustodyEvidenceKeepsNativePolicy(t *testing.T) {
	var evidence supervisorcore.InspectionEvidence = inspectionCustodyEvidence{}
	if _, ok := evidence.(supervisorcore.InspectionCustody); !ok {
		t.Fatal("the cleanup evidence does not provide inspection custody")
	}
}

// TestPrivateCleanupOwnerReleasesVerifiedInspectionCustody drives the
// production cleanup factory: identity-protected preparation never touches
// custody, the unprotected custody phase reports a missing pool while keeping
// its views and its row, migration's result is unchanged, and the next custody
// phase releases the exact recorded pool and then authorizes the row.
func TestPrivateCleanupOwnerReleasesVerifiedInspectionCustody(t *testing.T) {
	f := newEndedCustodyInspection(t)
	taskID := strings.Repeat("b", 32)
	restore := moveCustodyTestPool(t, f.registration.CustodyPlacement)
	if err := privateCleanupOwner().PrepareEndedInspectionEvidence(t.Context(), f.store, f.legacy, taskID, f.job); err != nil {
		t.Fatalf("a custody fault changed preparation's result: %v", err)
	}
	if err := releasePrivateTerminalCustody(t.Context(), f.store.Root, f.supervisor, pueue.PrivateTerminalCustodyScope{Candidates: []pueue.Job{f.job}}); err == nil {
		t.Fatal("the custody phase did not report a missing pool")
	}
	if err := inspectLegacyEndedJob(context.Background(), f.store, f.legacy, f.job); err != nil {
		t.Fatalf("migration validator consulted custody: %v", err)
	}
	if f.removable(t) {
		t.Fatal("a row whose pool is missing was authorized for removal")
	}
	restore()
	if f.removable(t) {
		t.Fatal("a row with unreleased custody was authorized for removal")
	}
	if !custodyTestPending(t, f.registration) {
		t.Fatal("a missing pool was treated as released")
	}
	if err := privateCleanupOwner().PrepareEndedInspectionEvidence(t.Context(), f.store, f.legacy, taskID, f.job); err != nil {
		t.Fatal(err)
	}
	if !custodyTestPending(t, f.registration) {
		t.Fatal("identity-protected preparation released custody")
	}
	if err := releasePrivateTerminalCustody(t.Context(), f.store.Root, f.supervisor, pueue.PrivateTerminalCustodyScope{Candidates: []pueue.Job{f.job}}); err != nil {
		t.Fatal(err)
	}
	if custodyTestPending(t, f.registration) {
		t.Fatal("the custody phase did not release the recorded pool")
	}
	if !f.removable(t) {
		t.Fatal("a released row was not authorized for removal")
	}
}
