package workspaces

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// countingOperationRuntime is the unavailable runtime port that records each
// saved-supervisor open, the first step of every submission attempt.
type countingOperationRuntime struct {
	unavailableOperationRuntime
	opened *[]task.SupervisorRef
}

func (r countingOperationRuntime) OpenSaved(ctx context.Context, store *taskdir.Store, ref task.SupervisorRef, source infrastructure.SavedBindingSource) (*pueue.Client, error) {
	*r.opened = append(*r.opened, ref)
	return r.unavailableOperationRuntime.OpenSaved(ctx, store, ref, source)
}

func (f managedCheckpointFixture) observe(t *testing.T, opened *[]task.SupervisorRef) (CheckpointDescription, error) {
	t.Helper()
	deps := OperationOptions{Runtime: countingOperationRuntime{opened: opened}}
	return ObserveCheckpoint(t.Context(), f.recovery.Store, deps, f.operation.Request().OperationID)
}

func (f managedCheckpointFixture) holdUseOwner(t *testing.T) *taskdir.LifetimeOwnerLease {
	t.Helper()
	owner, err := f.recovery.Resource.TryAcquireUseOwner(t.Context())
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

// requireUnsettled asserts C's reservation is still the active use, with no
// resource reference, submission or capture.
func (f managedCheckpointFixture) requireUnsettled(t *testing.T, opened []task.SupervisorRef) {
	t.Helper()
	if current, err := f.recovery.Resource.Reservation(t.Context()); err != nil || current != f.reservation {
		t.Fatalf("reservation = %+v, %v; want unchanged %+v", current, err, f.reservation)
	}
	if ids, err := f.recovery.Resource.OperationIDs(t.Context()); err != nil || len(ids) != 0 {
		t.Fatalf("observation recorded a resource reference: %v, %v", ids, err)
	}
	if len(opened) != 0 {
		t.Fatalf("observation submitted work: %+v", opened)
	}
	f.requireNoCapture(t)
}

// receiptBytes is the canonical encoding of C's stored receipt.
func (f managedCheckpointFixture) receiptBytes(t *testing.T) string {
	t.Helper()
	receipt, err := f.operation.CheckpointReceipt(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func requireObservedCheckpointed(t *testing.T, description CheckpointDescription, err error, id string) {
	t.Helper()
	if err != nil || description.ID != id || description.State != workspace.CheckpointCheckpointed || !description.Usable {
		t.Fatalf("observed checkpoint = %+v, %v; want usable checkpointed %s", description, err, id)
	}
}

// requireRedrivenOnly asserts C's repaired reference and exactly one
// submission attempt of C's own request per observation, under the same
// reservation and without capture.
func (f managedCheckpointFixture) requireRedrivenOnly(t *testing.T, opened []task.SupervisorRef, observations int) {
	t.Helper()
	request := f.operation.Request()
	if ids, err := f.recovery.Resource.OperationIDs(t.Context()); err != nil || len(ids) != 1 || ids[0] != request.OperationID {
		t.Fatalf("resource reference after %d observations = %v, %v", observations, ids, err)
	}
	if len(opened) != observations || opened[observations-1] != request.Supervisor {
		t.Fatalf("submissions after %d observations = %+v; want one redrive of %+v each", observations, opened, request.Supervisor)
	}
	if current, err := f.recovery.Resource.Reservation(t.Context()); err != nil || current != f.reservation {
		t.Fatalf("reservation after %d observations = %+v, %v", observations, current, err)
	}
	f.requireNoCapture(t)
}

// TestObserveManagedCheckpointRepairsInterruptedAdmission starts from a
// reservation committed before RecordOperation and submission. Observation
// repairs the same request's reference and redrives only that request, with
// the same digest, deadline and reservation, on every observation.
func TestObserveManagedCheckpointRepairsInterruptedAdmission(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	request := fixture.operation.Request()
	var opened []task.SupervisorRef
	for observation := 1; observation <= 2; observation++ {
		description, err := fixture.observe(t, &opened)
		if !errors.Is(err, workspace.ErrPending) || !errors.Is(err, errOperationFixtureRuntimeUnavailable) {
			t.Fatalf("observation %d = %v; want pending redrive of the same request", observation, err)
		}
		if description.ID != request.OperationID || description.State != CheckpointPending {
			t.Fatalf("observation %d description = %+v", observation, description)
		}
		fixture.requireRedrivenOnly(t, opened, observation)
	}
	reloaded, err := workspace.LoadOperation(t.Context(), fixture.recovery.Store, request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := reloaded.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if reloaded.Digest() != fixture.operation.Digest() || !reloaded.Deadline().Equal(fixture.operation.Deadline()) {
		t.Fatalf("redrive changed the request: %s %v", reloaded.Digest(), reloaded.Deadline())
	}
}

// TestObserveManagedCheckpointSettlesClosedHelper starts from a canonically
// closed helper with no receipt and an open reservation.
func TestObserveManagedCheckpointSettlesClosedHelper(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	closeCheckpointLifetime(t, fixture.recovery.Store, fixture.operation, checkpointNative, func() { recordCheckpointWork(t, fixture.operation, &fixture.target) })
	var opened []task.SupervisorRef
	description, err := fixture.observe(t, &opened)
	requireObservedCheckpointed(t, description, err, fixture.operation.Request().OperationID)
	fixture.requireSettled(t)
	if len(opened) != 0 {
		t.Fatalf("settling a closed helper submitted work: %+v", opened)
	}
}

// TestObserveManagedCheckpointSettlesQualifiedReceipt starts from a stored
// qualified receipt with an open reservation: a receipt is creation
// authority, not proof that the reservation closed.
func TestObserveManagedCheckpointSettlesQualifiedReceipt(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	closeCheckpointLifetime(t, fixture.recovery.Store, fixture.operation, checkpointNative, func() { recordCheckpointWork(t, fixture.operation, &fixture.target) })
	if _, err := QualifyCheckpoint(t.Context(), fixture.recovery.Store, fixture.operation); err != nil {
		t.Fatal(err)
	}
	stored := fixture.receiptBytes(t)
	var opened []task.SupervisorRef
	description, err := fixture.observe(t, &opened)
	requireObservedCheckpointed(t, description, err, fixture.operation.Request().OperationID)
	fixture.requireSettled(t)
	if fixture.receiptBytes(t) != stored {
		t.Fatal("settlement rewrote the stored receipt")
	}
	if len(opened) != 0 {
		t.Fatalf("settling a qualified receipt submitted work: %+v", opened)
	}
}

// TestObserveManagedCheckpointContentionKeepsQualification holds the use
// owner: observation qualifies C but neither waits nor settles; the free
// owner's next observation settles the same reservation.
func TestObserveManagedCheckpointContentionKeepsQualification(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	closeCheckpointLifetime(t, fixture.recovery.Store, fixture.operation, checkpointNative, func() { recordCheckpointWork(t, fixture.operation, &fixture.target) })
	owner := fixture.holdUseOwner(t)
	var opened []task.SupervisorRef
	description, err := fixture.observe(t, &opened)
	requireObservedCheckpointed(t, description, err, fixture.operation.Request().OperationID)
	fixture.requireUnsettled(t, opened)
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
	description, err = fixture.observe(t, &opened)
	requireObservedCheckpointed(t, description, err, fixture.operation.Request().OperationID)
	fixture.requireSettled(t)
}

// settleThenReserveLaterUse settles C under the use owner, releases it, and
// reserves another use B, which has no operation record.
func (f managedCheckpointFixture) settleThenReserveLaterUse(t *testing.T) workspace.Reservation {
	t.Helper()
	store, resource := f.recovery.Store, f.recovery.Resource
	closeCheckpointLifetime(t, store, f.operation, checkpointNative, func() { recordCheckpointWork(t, f.operation, &f.target) })
	owner := f.holdUseOwner(t)
	if err := ReconcileUse(t.Context(), store, resource, OperationOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	later := workspace.Subject{Kind: workspace.SubjectOperation, ID: strings.Repeat("4", 32), SHA256: task.ComputeSHA256([]byte("later use"))}
	reservation, err := resource.Reserve(t.Context(), later)
	if err != nil {
		t.Fatal(err)
	}
	return reservation
}

// requireLaterUseUntouched asserts B stays the active use with no capture,
// added resource reference or submission.
func (f managedCheckpointFixture) requireLaterUseUntouched(t *testing.T, reservation workspace.Reservation, opened []task.SupervisorRef) {
	t.Helper()
	resource := f.recovery.Resource
	if current, err := resource.Reservation(t.Context()); err != nil || current != reservation {
		t.Fatalf("later use = %+v, %v; want unchanged %+v", current, err, reservation)
	}
	if _, err := resource.CaptureObligation(t.Context(), reservation.Sequence); !workspace.OperationAbsent(err) {
		t.Fatalf("observation reconciled the later use: %v", err)
	}
	if _, err := resource.AssociatedOperation(t.Context(), workspace.CapturePurpose(reservation.Sequence)); !workspace.OperationAbsent(err) {
		t.Fatalf("observation scheduled the later use's capture: %v", err)
	}
	if ids, err := resource.OperationIDs(t.Context()); err != nil || len(ids) != 1 {
		t.Fatalf("observation changed resource references: %v, %v", ids, err)
	}
	if len(opened) != 0 {
		t.Fatalf("observation submitted work: %+v", opened)
	}
}

// TestObserveSettledManagedCheckpointLeavesLaterUseAlone settles C, then
// reserves another use B. Observing C reads its history and never reconciles
// B, whether B's owner is free or held.
func TestObserveSettledManagedCheckpointLeavesLaterUseAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		held bool
	}{{"owner-free", false}, {"owner-held", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newReservedManagedCheckpoint(t)
			reservation := fixture.settleThenReserveLaterUse(t)
			if tc.held {
				fixture.holdUseOwner(t)
			}
			var opened []task.SupervisorRef
			description, err := fixture.observe(t, &opened)
			requireObservedCheckpointed(t, description, err, fixture.operation.Request().OperationID)
			fixture.requireLaterUseUntouched(t, reservation, opened)
		})
	}
}

// newForgedManagedCheckpoint reserves C's operation ID with another digest.
func newForgedManagedCheckpoint(t *testing.T) managedCheckpointFixture {
	t.Helper()
	recovery := workspacefixture.NewRecoveryFixture(t)
	source, checkout := workspacefixture.ManagedCheckpointFixture(t, recovery.Resource)
	selection := workspace.CheckpointSelection{Kind: workspace.CheckpointManaged}
	operation := openCheckpointCreation(t, recovery.Store, reservedManagedCheckpointID, recovery.Resource.Request().WorkspaceID, selection, time.Now().Add(time.Hour))
	forged := workspace.Subject{Kind: workspace.SubjectOperation, ID: operation.Request().OperationID, SHA256: task.ComputeSHA256([]byte("forged"))}
	reservation, err := recovery.Resource.Reserve(t.Context(), forged)
	if err != nil {
		t.Fatal(err)
	}
	return managedCheckpointFixture{
		recovery: recovery, operation: operation, reservation: reservation,
		target: workspace.CheckpointTarget{Source: source, CheckoutRoot: checkout.CheckoutPath, AdminDir: checkout.AdminDir},
	}
}

// TestObserveManagedCheckpointRefusesForgedReservationDigest reserves C's ID
// with another digest. The binding is refused before any effect, whether the
// use owner is free or held: observation never skips it as an unrelated use
// or hides it behind owner contention and pending metadata.
func TestObserveManagedCheckpointRefusesForgedReservationDigest(t *testing.T) {
	for _, tc := range []struct {
		name string
		held bool
	}{{"owner-free", false}, {"owner-held", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newForgedManagedCheckpoint(t)
			if tc.held {
				fixture.holdUseOwner(t)
			}
			var opened []task.SupervisorRef
			_, err := fixture.observe(t, &opened)
			requireConcreteFault(t, "forged reservation digest", err, task.ErrEvidenceFault)
			fixture.requireUnsettled(t, opened)
		})
	}
}

// TestObserveManagedCheckpointCorruptReservationIsAFault corrupts C's active
// reservation record: an unreadable reservation is never clean settlement.
func TestObserveManagedCheckpointCorruptReservationIsAFault(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	if fixture.reservation.Sequence != 1 {
		t.Fatalf("fixture reservation sequence = %d, want 1", fixture.reservation.Sequence)
	}
	fixture.recovery.CorruptResourceRecord(t, "use-"+strings.Repeat("0", 19)+"1.json")
	var opened []task.SupervisorRef
	_, err := fixture.observe(t, &opened)
	if err == nil || PendingError(err) {
		t.Fatalf("corrupt reservation = %v; want a concrete fault", err)
	}
	if ids, idErr := fixture.recovery.Resource.OperationIDs(t.Context()); idErr != nil || len(ids) != 0 || len(opened) != 0 {
		t.Fatalf("corrupt reservation was settled: %v, %v, %+v", ids, idErr, opened)
	}
}

// TestUseOwnerContentionRequiresEveryCauseBusy keeps a joined concrete fault
// or a bare deadline out of the observational contention branch.
func TestUseOwnerContentionRequiresEveryCauseBusy(t *testing.T) {
	concrete := errors.New("lease close fault")
	cases := []struct {
		err  error
		want bool
	}{
		{task.ErrLockBusy, true},
		{errors.Join(task.ErrLockBusy, task.ErrLockBusy), true},
		{nil, false},
		{errors.Join(task.ErrLockBusy, concrete), false},
		{errors.Join(task.ErrLockBusy, context.DeadlineExceeded), false},
		{context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		if got := useOwnerContended(tc.err); got != tc.want {
			t.Errorf("useOwnerContended(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}
