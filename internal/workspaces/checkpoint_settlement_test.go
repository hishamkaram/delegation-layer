package workspaces

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

type managedCheckpointFixture struct {
	recovery    *workspacefixture.RecoveryFixture
	operation   *workspace.Operation
	target      workspace.CheckpointTarget
	reservation workspace.Reservation
}

// newReservedManagedCheckpoint models a managed creation whose reservation
// committed but whose resource reference was lost before RecordOperation.
func newReservedManagedCheckpoint(t *testing.T) managedCheckpointFixture {
	t.Helper()
	return newReservedManagedCheckpointIn(t, workspacefixture.NewRecoveryFixture(t))
}

// reservedManagedCheckpointID names the creation of a reserved managed checkpoint.
var reservedManagedCheckpointID = strings.Repeat("1", 32)

// newReservedManagedCheckpointIn is newReservedManagedCheckpoint over recovery.
func newReservedManagedCheckpointIn(t *testing.T, recovery *workspacefixture.RecoveryFixture) managedCheckpointFixture {
	t.Helper()
	source, checkout := workspacefixture.ManagedCheckpointFixture(t, recovery.Resource)
	selection := workspace.CheckpointSelection{Kind: workspace.CheckpointManaged}
	operation := openCheckpointCreation(t, recovery.Store, reservedManagedCheckpointID, recovery.Resource.Request().WorkspaceID, selection, time.Now().Add(time.Hour))
	use := workspace.Subject{Kind: workspace.SubjectOperation, ID: operation.Request().OperationID, SHA256: operation.Digest()}
	reservation, err := recovery.Resource.Reserve(t.Context(), use)
	if err != nil {
		t.Fatal(err)
	}
	target := workspace.CheckpointTarget{Source: source, CheckoutRoot: checkout.CheckoutPath, AdminDir: checkout.AdminDir}
	return managedCheckpointFixture{recovery: recovery, operation: operation, target: target, reservation: reservation}
}

func (f managedCheckpointFixture) requireNoCapture(t *testing.T) {
	t.Helper()
	resource := f.recovery.Resource
	if _, err := resource.CaptureObligation(t.Context(), f.reservation.Sequence); !workspace.OperationAbsent(err) {
		t.Fatalf("checkpoint settlement read or created a capture obligation: %v", err)
	}
	if _, err := resource.AssociatedOperation(t.Context(), workspace.CapturePurpose(f.reservation.Sequence)); !workspace.OperationAbsent(err) {
		t.Fatalf("checkpoint settlement scheduled capture work: %v", err)
	}
	if latest, err := resource.LatestReservation(t.Context()); err != nil || latest != f.reservation {
		t.Fatalf("checkpoint settlement created another reservation: %+v, %v", latest, err)
	}
}

// requireSettled asserts the checkpoint's own reservation closed as a
// completed operation, with its resource reference present and no capture.
func (f managedCheckpointFixture) requireSettled(t *testing.T) {
	t.Helper()
	resource := f.recovery.Resource
	closed, err := resource.ReservationClosed(t.Context(), f.reservation.Sequence)
	if err != nil || closed.Disposition != "operation-completed" || closed.CaptureID != "" {
		t.Fatalf("checkpoint reservation closure = %+v, %v", closed, err)
	}
	if _, err = resource.Reservation(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("settled checkpoint still holds the workspace: %v", err)
	}
	if ids, idErr := resource.OperationIDs(t.Context()); idErr != nil || len(ids) != 1 || ids[0] != f.operation.Request().OperationID {
		t.Fatalf("missing resource reference was not repaired: %v, %v", ids, idErr)
	}
	f.requireNoCapture(t)
}

func TestManagedCheckpointSettlementRepairsReferenceAndClosesSameReservation(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	store, resource := fixture.recovery.Store, fixture.recovery.Resource
	closeCheckpointLifetime(t, store, fixture.operation, checkpointNative, func() { recordCheckpointWork(t, fixture.operation, &fixture.target) })
	if ids, err := resource.OperationIDs(t.Context()); err != nil || len(ids) != 0 {
		t.Fatalf("fixture already indexed the checkpoint: %v, %v", ids, err)
	}
	workspacefixture.RecoveryUseOwner(t, fixture.recovery)
	if err := ReconcileUse(t.Context(), store, resource, OperationOptions{}); err != nil {
		t.Fatal(err)
	}
	fixture.requireSettled(t)
	if receipt, receiptErr := fixture.operation.CheckpointReceipt(t.Context()); receiptErr != nil || receipt.Status != workspace.CheckpointCheckpointed {
		t.Fatalf("managed checkpoint receipt = %+v, %v", receipt, receiptErr)
	}
	description, err := Describe(t.Context(), resource)
	if err != nil || description.Capture != nil || description.State == "busy" {
		t.Fatalf("workspace after checkpoint = %+v, %v", description, err)
	}
	if err = DescribeOperation(t.Context(), store, resource, fixture.operation.Request().OperationID, &description); err != nil || description.Operation != nil || description.ReasonCode != "" {
		t.Fatalf("checkpoint operation projected a generic result: %+v, %v", description, err)
	}
	if err = ReconcileUse(t.Context(), store, resource, OperationOptions{}); err != nil {
		t.Fatalf("settled checkpoint replay = %v", err)
	}
}

func TestManagedCheckpointSettlementRecordsUnavailableRecoveredSuccess(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	store, resource := fixture.recovery.Store, fixture.recovery.Resource
	closeCheckpointLifetime(t, store, fixture.operation, checkpointRegisteredRecovered, func() { recordCheckpointWork(t, fixture.operation, &fixture.target) })
	workspacefixture.RecoveryUseOwner(t, fixture.recovery)
	if err := ReconcileUse(t.Context(), store, resource, OperationOptions{}); err != nil {
		t.Fatal(err)
	}
	receipt, err := fixture.operation.CheckpointReceipt(t.Context())
	if err != nil || receipt.Status != workspace.CheckpointUnavailable || receipt.ReasonCode != workspace.CheckpointResultUnavailable {
		t.Fatalf("recovered managed checkpoint = %+v, %v", receipt, err)
	}
	if _, err = fixture.operation.CheckpointCandidate(t.Context()); err != nil {
		t.Fatalf("unavailable checkpoint lost its candidate for cleanup: %v", err)
	}
	if err = OperationOutcome(t.Context(), store, fixture.operation); !strings.Contains(errorText(err), workspace.CheckpointResultUnavailable) {
		t.Fatalf("observed unavailable checkpoint = %v, want its reason as a failure", err)
	}
	fixture.requireNoCapture(t)
}

func TestManagedCheckpointSettlementRefusesTargetOutsideResourceProvenance(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	store, resource := fixture.recovery.Store, fixture.recovery.Resource
	wrong := fixture.target
	wrong.CheckoutRoot += "-replaced"
	closeCheckpointLifetime(t, store, fixture.operation, checkpointNative, func() { recordCheckpointWork(t, fixture.operation, &wrong) })
	workspacefixture.RecoveryUseOwner(t, fixture.recovery)
	if err := ReconcileUse(t.Context(), store, resource, OperationOptions{}); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("checkpoint of another checkout = %v, want evidence fault", err)
	}
	if _, err := fixture.operation.CheckpointReceipt(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("wrong target was qualified: %v", err)
	}
	if current, err := resource.Reservation(t.Context()); err != nil || current != fixture.reservation {
		t.Fatalf("refused checkpoint lost its reservation: %+v, %v", current, err)
	}
}

func TestManagedCheckpointUnexpectedCaptureObligationRefusesBeforeCapture(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	store, resource := fixture.recovery.Store, fixture.recovery.Resource
	if err := resource.RecordCaptureObligation(t.Context(), fixture.reservation, fixture.reservation.Subject); err != nil {
		t.Fatal(err)
	}
	workspacefixture.RecoveryUseOwner(t, fixture.recovery)
	if err := ReconcileUse(t.Context(), store, resource, OperationOptions{}); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("capture obligation for a checkpoint = %v, want refusal", err)
	}
	if _, err := resource.AssociatedOperation(t.Context(), workspace.CapturePurpose(fixture.reservation.Sequence)); !workspace.OperationAbsent(err) {
		t.Fatalf("refused obligation still scheduled capture: %v", err)
	}
	if _, err := fixture.operation.CheckpointReceipt(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("refused obligation qualified the checkpoint: %v", err)
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
