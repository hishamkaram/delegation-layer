package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// These cases recover managed checkpoint state only through the public
// `checkpoint status` command: Run composes the production observer, the
// use-owner settlement and the saved-supervisor submission over the native
// Git, journals and fake Pueue client of checkpointRuntimeFixture. The fake
// add returns task 9 and status lists only rows written here. No real Pueue,
// Pueue-launched worker or provider runs.

var errCheckpointReferenceLink = errors.New("checkpoint operation reference link failed")

// checkpointFakeTaskID is the numeric task ID the fake supervisor's add returns.
const checkpointFakeTaskID int64 = 9

// checkpointReferenceLinkFault fails the first operation-reference write in
// one resource directory at its hard link. Before the link nothing is
// recorded; after it the reference exists but its directory barrier fails.
// Either way managed admission stops after Reserve and before submission.
type checkpointReferenceLinkFault struct {
	taskdir.BaseFaultInjector
	directory string
	afterLink bool
	fired     atomic.Bool
}

func (f *checkpointReferenceLinkFault) claim(destination string) bool {
	return filepath.Dir(destination) == f.directory && strings.HasPrefix(filepath.Base(destination), "operation-") && !f.fired.Swap(true)
}

func (f *checkpointReferenceLinkFault) OnLink(_, destination string) error {
	if !f.afterLink && f.claim(destination) {
		return errCheckpointReferenceLink
	}
	return nil
}

func (f *checkpointReferenceLinkFault) OnPostLinkDirBarrier(destination string) error {
	if f.afterLink && f.claim(destination) {
		return errCheckpointReferenceLink
	}
	return nil
}

var _ taskdir.FaultInjector = (*checkpointReferenceLinkFault)(nil)

// managedCheckpointAuthorization is the durable authorization canonical
// managed admission committed: the request, its digest and its reservation.
type managedCheckpointAuthorization struct {
	resource    *workspace.Resource
	id          string
	request     workspace.OperationRequest
	digest      string
	reservation workspace.Reservation
}

// beginManagedCheckpointThroughFault runs canonical managed admission of a
// provisioned child while the reference fault is installed, then removes it.
// The authorized request keeps its reservation and was never submitted.
func beginManagedCheckpointThroughFault(t *testing.T, fixture checkpointRuntimeFixture, resource *workspace.Resource, afterLink bool) managedCheckpointAuthorization {
	t.Helper()
	store := fixture.lineage.store
	fault := &checkpointReferenceLinkFault{directory: filepath.Join(store.Root, "workspaces", resource.Request().WorkspaceID), afterLink: afterLink}
	adds := fixture.adds.Load()
	store.SetFaultInjector(fault)
	ack, err := workspaces.BeginManagedCheckpoint(fixture.ctx(), store, resource, fixture.options, time.Minute)
	store.SetFaultInjector(nil)
	if !fault.fired.Load() || !ack.Authorized() || ack.OperationID() == "" || !errors.Is(err, errCheckpointReferenceLink) || workspaces.PendingError(err) {
		t.Fatalf("faulted managed admission = %+v, %v (fired %t); want authorized reference fault", ack, err, fault.fired.Load())
	}
	fixture.requireAdds(t, adds)
	authorization := loadManagedCheckpointAuthorization(t, fixture, resource, ack.OperationID())
	if _, err = loadCheckpointReceipt(t, fixture, authorization.id); !workspace.OperationAbsent(err) {
		t.Fatalf("faulted managed admission recorded a receipt: %v", err)
	}
	return authorization
}

// beginManagedCheckpoint runs canonical managed admission to its submitted
// receipt.
func beginManagedCheckpoint(t *testing.T, fixture checkpointRuntimeFixture, resource *workspace.Resource) managedCheckpointAuthorization {
	t.Helper()
	ack, err := workspaces.BeginManagedCheckpoint(fixture.ctx(), fixture.lineage.store, resource, fixture.options, time.Minute)
	return loadManagedCheckpointAuthorization(t, fixture, resource, requireAcceptedPending(t, "managed checkpoint", ack, err))
}

func loadManagedCheckpointAuthorization(t *testing.T, fixture checkpointRuntimeFixture, resource *workspace.Resource, id string) managedCheckpointAuthorization {
	t.Helper()
	operation, err := workspace.LoadOperation(fixture.ctx(), fixture.lineage.store, id)
	if err != nil {
		t.Fatal(err)
	}
	authorization := managedCheckpointAuthorization{resource: resource, id: id, request: operation.Request(), digest: operation.Digest()}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	if authorization.reservation, err = resource.Reservation(fixture.ctx()); err != nil {
		t.Fatal(err)
	}
	if want := (workspace.Subject{Kind: workspace.SubjectOperation, ID: id, SHA256: authorization.digest}); authorization.reservation.Subject != want {
		t.Fatalf("managed admission reserved %+v, want %+v", authorization.reservation.Subject, want)
	}
	return authorization
}

func loadCheckpointReceipt(t *testing.T, fixture checkpointRuntimeFixture, id string) (workspace.Receipt, error) {
	t.Helper()
	operation, err := workspace.LoadOperation(fixture.ctx(), fixture.lineage.store, id)
	if err != nil {
		t.Fatal(err)
	}
	receipt, receiptErr := operation.Receipt(fixture.ctx())
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	return receipt, receiptErr
}

// requireReferences asserts the resource's exact operation references.
func (a managedCheckpointAuthorization) requireReferences(t *testing.T, fixture checkpointRuntimeFixture, want ...string) {
	t.Helper()
	ids, err := a.resource.OperationIDs(fixture.ctx())
	if err != nil || len(ids) != len(want) || (len(want) != 0 && !reflect.DeepEqual(ids, want)) {
		t.Fatalf("resource references = %v, %v; want %v", ids, err, want)
	}
}

// requireSubmittedUnchanged asserts the same request, digest, deadline,
// purpose and reservation, submitted once as the fake task.
func (a managedCheckpointAuthorization) requireSubmittedUnchanged(t *testing.T, fixture checkpointRuntimeFixture) {
	t.Helper()
	reloaded := loadManagedCheckpointAuthorization(t, fixture, a.resource, a.id)
	if !reflect.DeepEqual(reloaded.request, a.request) || reloaded.digest != a.digest || reloaded.reservation != a.reservation {
		t.Fatalf("recovered authorization = %+v; want unchanged %+v", reloaded, a)
	}
	if latest, err := a.resource.LatestReservation(fixture.ctx()); err != nil || latest != a.reservation {
		t.Fatalf("latest reservation = %+v, %v; want %+v", latest, err, a.reservation)
	}
	reference, err := a.resource.AssociatedOperation(fixture.ctx(), workspaces.CheckpointPurpose(a.request.Subject.ID))
	if err != nil || reference.OperationID != a.id || reference.Kind != workspace.OperationCheckpoint {
		t.Fatalf("checkpoint purpose = %+v, %v; want %s", reference, err, a.id)
	}
	if receipt, err := loadCheckpointReceipt(t, fixture, a.id); err != nil || receipt.NumericTaskID != checkpointFakeTaskID || receipt.RequestSHA256 != a.digest {
		t.Fatalf("submission receipt = %+v, %v; want task %d of %s", receipt, err, checkpointFakeTaskID, a.digest)
	}
}

// requireSettledOnce asserts the reservation closed with no successor use
// and no capture of it.
func (a managedCheckpointAuthorization) requireSettledOnce(t *testing.T, fixture checkpointRuntimeFixture) {
	t.Helper()
	resource := a.resource
	if _, err := resource.Reservation(fixture.ctx()); !workspace.OperationAbsent(err) {
		t.Fatalf("settled checkpoint kept its reservation: %v", err)
	}
	if latest, err := resource.LatestReservation(fixture.ctx()); err != nil || latest != a.reservation {
		t.Fatalf("latest reservation = %+v, %v; want settled %+v", latest, err, a.reservation)
	}
	if _, err := resource.CaptureObligation(fixture.ctx(), a.reservation.Sequence); !workspace.OperationAbsent(err) {
		t.Fatalf("checkpoint settlement created a capture obligation: %v", err)
	}
	if _, err := resource.AssociatedOperation(fixture.ctx(), workspace.CapturePurpose(a.reservation.Sequence)); !workspace.OperationAbsent(err) {
		t.Fatalf("checkpoint settlement scheduled a capture: %v", err)
	}
}

// writeCheckpointQueueRow lists the fake task as id's exact supervisor row.
func writeCheckpointQueueRow(t *testing.T, fixture checkpointRuntimeFixture, id string, state map[string]any) {
	t.Helper()
	store := fixture.lineage.store
	label := pueue.InspectionIdentity{RootID: store.RootID, TaskID: id, QueueGroup: "default"}.Label()
	writeTerminalEventStatus(t, store, pueue.Job{ID: checkpointFakeTaskID, Group: "default", Label: &label}, state)
}

var (
	checkpointQueueStashed = map[string]any{"Stashed": map[string]any{"enqueue_at": nil}}
	checkpointQueueRunning = map[string]any{"Running": map[string]any{"enqueued_at": "2026-10-10T00:00:00Z", "start": "2026-10-10T00:00:00Z"}}
)

func requirePendingCheckpointStatus(t *testing.T, label string, code int, response Response, id string) {
	t.Helper()
	checkpoint := requireCheckpoint(t, label, code, response, 3, id, "", checkpointWatch)
	requireCurrent(t, label, checkpoint, workspaces.CheckpointPending, false)
	if response.Error != "" {
		t.Fatalf("%s error = %q", label, response.Error)
	}
}

// TestCheckpointStatusRedrivesInterruptedManagedAdmissionOnce interrupts
// canonical managed admission at the operation-reference link, before or
// after the link. Public status repairs the reference only when it is
// missing, submits the same request once, and a later matching Running row
// is observed without another add or enqueue.
func TestCheckpointStatusRedrivesInterruptedManagedAdmissionOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		afterLink bool
	}{{"reservation-before-reference", false}, {"reference-before-submission", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCheckpointRuntimeFixture(t)
			resource, _ := fixture.lineage.provisionedChild(t, fixture.lineage.main)
			seeded := beginManagedCheckpointThroughFault(t, fixture, resource, tc.afterLink)
			if tc.afterLink {
				seeded.requireReferences(t, fixture, seeded.id)
			} else {
				seeded.requireReferences(t, fixture)
			}

			enqueued := fixture.enqueues.Load()
			code, response := fixture.cli(t, "status", seeded.id)
			requirePendingCheckpointStatus(t, "redriven admission", code, response, seeded.id)
			fixture.requireAdds(t, 1)
			if got := fixture.enqueues.Load(); got != enqueued+1 {
				t.Fatalf("redriven admission enqueued %d times, want exactly once", got-enqueued)
			}
			seeded.requireReferences(t, fixture, seeded.id)
			seeded.requireSubmittedUnchanged(t, fixture)

			writeCheckpointQueueRow(t, fixture, seeded.id, checkpointQueueRunning)
			enqueued = fixture.enqueues.Load()
			code, response = fixture.cli(t, "status", seeded.id)
			requirePendingCheckpointStatus(t, "running redriven admission", code, response, seeded.id)
			fixture.requireAdds(t, 1)
			if got := fixture.enqueues.Load(); got != enqueued {
				t.Fatalf("running observation enqueued again: %d, want %d", got, enqueued)
			}
			seeded.requireReferences(t, fixture, seeded.id)
			seeded.requireSubmittedUnchanged(t, fixture)
		})
	}
}

// TestCheckpointStatusReplaysMatchingQueueRowWithoutResubmission observes a
// submitted managed checkpoint whose exact row is Stashed or Running. Status
// replays the existing receipt and never adds another task.
func TestCheckpointStatusReplaysMatchingQueueRowWithoutResubmission(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	resource, _ := fixture.lineage.provisionedChild(t, fixture.lineage.main)
	submitted := beginManagedCheckpoint(t, fixture, resource)
	fixture.requireAdds(t, 1)
	submitted.requireSubmittedUnchanged(t, fixture)
	enqueued := fixture.enqueues.Load()

	writeCheckpointQueueRow(t, fixture, submitted.id, checkpointQueueStashed)
	code, response := fixture.cli(t, "status", submitted.id)
	requirePendingCheckpointStatus(t, "stashed observation", code, response, submitted.id)
	fixture.requireAdds(t, 1)
	if got := fixture.enqueues.Load(); got != enqueued+1 {
		t.Fatalf("stashed observation enqueued %d times, want exactly once", got-enqueued)
	}
	submitted.requireReferences(t, fixture, submitted.id)
	submitted.requireSubmittedUnchanged(t, fixture)

	writeCheckpointQueueRow(t, fixture, submitted.id, checkpointQueueRunning)
	code, response = fixture.cli(t, "status", submitted.id)
	requirePendingCheckpointStatus(t, "running observation", code, response, submitted.id)
	fixture.requireAdds(t, 1)
	if got := fixture.enqueues.Load(); got != enqueued+1 {
		t.Fatalf("running observation changed enqueue count to %d, want %d", got, enqueued+1)
	}
	submitted.requireReferences(t, fixture, submitted.id)
	submitted.requireSubmittedUnchanged(t, fixture)
}

// TestCheckpointStatusSettlesClosedManagedHelperOnce closes the helper's
// lifetime without guardian completion, so the reservation stays active,
// optionally after qualifying its receipt. Public status settles exactly
// that reservation without recapture or rewriting the receipt, and a second
// observation changes nothing.
func TestCheckpointStatusSettlesClosedManagedHelperOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		qualified bool
	}{{"closed-helper", false}, {"qualified-receipt", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCheckpointRuntimeFixture(t)
			resource, _ := fixture.lineage.provisionedChild(t, fixture.lineage.main)
			submitted := beginManagedCheckpoint(t, fixture, resource)
			operation := closeManagedHelperWithoutCompletion(t, fixture, submitted, tc.qualified)
			stored := ""
			if tc.qualified {
				stored = checkpointReceiptBytes(t, fixture, operation)
			}
			for observation := 1; observation <= 2; observation++ {
				label := fmt.Sprintf("%s observation %d", tc.name, observation)
				submitted.requireSettledStatus(t, fixture, label)
				got := checkpointReceiptBytes(t, fixture, operation)
				if stored != "" && got != stored {
					t.Fatalf("%s rewrote the checkpoint receipt:\n%s\nwant\n%s", label, got, stored)
				}
				stored = got
			}
		})
	}
}

// closeManagedHelperWithoutCompletion runs the submitted checkpoint's action
// inside a real guardian lifetime and skips guardian completion, so its
// reservation stays active. qualified also commits the qualified receipt.
func closeManagedHelperWithoutCompletion(t *testing.T, fixture checkpointRuntimeFixture, submitted managedCheckpointAuthorization, qualified bool) *workspace.Operation {
	t.Helper()
	store := fixture.lineage.store
	operation, err := workspace.LoadOperation(fixture.ctx(), store, submitted.id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	workspacefixture.RunRealGuardianLifetime(t, store, operation, func() { fixture.performAction(t, operation) })
	if qualified {
		if _, err = workspaces.QualifyCheckpoint(fixture.ctx(), store, operation); err != nil {
			t.Fatal(err)
		}
	}
	if current, err := submitted.resource.Reservation(fixture.ctx()); err != nil || current != submitted.reservation {
		t.Fatalf("closed helper reservation = %+v, %v; want active %+v", current, err, submitted.reservation)
	}
	return operation
}

// requireSettledStatus observes the checkpoint through public status and
// asserts its usable checkpoint, its settled reservation and no submission.
func (a managedCheckpointAuthorization) requireSettledStatus(t *testing.T, fixture checkpointRuntimeFixture, label string) {
	t.Helper()
	code, response := fixture.cli(t, "status", a.id)
	current := requireCurrent(t, label, requireCheckpoint(t, label, code, response, 0, a.id, "", ""), workspace.CheckpointCheckpointed, true)
	if current.WorkspaceID != a.resource.Request().WorkspaceID {
		t.Fatalf("%s described workspace %q", label, current.WorkspaceID)
	}
	fixture.requireCheckpointRef(t, current)
	a.requireSettledOnce(t, fixture)
	a.requireReferences(t, fixture, a.id)
	fixture.requireAdds(t, 1)
}

// checkpointReceiptBytes is the canonical encoding of operation's stored
// checkpoint receipt.
func checkpointReceiptBytes(t *testing.T, fixture checkpointRuntimeFixture, operation *workspace.Operation) string {
	t.Helper()
	receipt, err := operation.CheckpointReceipt(fixture.ctx())
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestCheckpointStatusLeavesAnotherCheckpointReservationAlone settles C, then
// interrupts D's admission before its reference. Observing C never repairs,
// submits or settles D's reservation.
func TestCheckpointStatusLeavesAnotherCheckpointReservationAlone(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	resource, _ := fixture.lineage.provisionedChild(t, fixture.lineage.main)
	settled := beginManagedCheckpoint(t, fixture, resource)
	fixture.runHelper(t, settled.id, true)
	pending := beginManagedCheckpointThroughFault(t, fixture, resource, false)
	fixture.requireAdds(t, 1)
	for observation := 1; observation <= 2; observation++ {
		code, response := fixture.cli(t, "status", settled.id)
		requireCurrent(t, "settled checkpoint", requireCheckpoint(t, "settled checkpoint", code, response, 0, settled.id, "", ""), workspace.CheckpointCheckpointed, true)
		if current, err := resource.Reservation(fixture.ctx()); err != nil || current != pending.reservation {
			t.Fatalf("observation %d changed another checkpoint's reservation: %+v, %v", observation, current, err)
		}
		pending.requireReferences(t, fixture, settled.id)
		if _, err := loadCheckpointReceipt(t, fixture, pending.id); !workspace.OperationAbsent(err) {
			t.Fatalf("observation %d submitted another checkpoint: %v", observation, err)
		}
		fixture.requireAdds(t, 1)
	}
}

// TestCheckpointStatusRefusesCorruptManagedReservation corrupts the active
// reservation of an interrupted admission: status fails with the creation's
// own pending description and never repairs, submits or settles it.
func TestCheckpointStatusRefusesCorruptManagedReservation(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	store := fixture.lineage.store
	resource, _ := fixture.lineage.provisionedChild(t, fixture.lineage.main)
	seeded := beginManagedCheckpointThroughFault(t, fixture, resource, false)
	record := filepath.Join(store.Root, "workspaces", resource.Request().WorkspaceID, fmt.Sprintf("use-%020d.json", seeded.reservation.Sequence))
	if _, err := os.Lstat(record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, response := fixture.cli(t, "status", seeded.id)
	checkpoint := requireCheckpoint(t, "corrupt reservation", code, response, 1, seeded.id, "", checkpointStopAndReport)
	requireCurrent(t, "corrupt reservation", checkpoint, workspaces.CheckpointPending, false)
	if !strings.Contains(response.Error, task.ErrEvidenceFault.Error()) {
		t.Fatalf("corrupt reservation error = %q; want an evidence fault", response.Error)
	}
	if contents, err := os.ReadFile(record); err != nil || string(contents) != "{" {
		t.Fatalf("corrupt reservation changed during refusal: %q, %v", contents, err)
	}
	seeded.requireReferences(t, fixture)
	if _, err := loadCheckpointReceipt(t, fixture, seeded.id); !workspace.OperationAbsent(err) {
		t.Fatalf("corrupt reservation was submitted: %v", err)
	}
	fixture.requireAdds(t, 0)
}
