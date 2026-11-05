package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInfrastructureRoutingRejectsCapturedSourceFaultBeforeStaging(t *testing.T) {
	t.Setenv(infrastructure.OriginEnvironment, "invalid original invocation")
	invocation := captureInfrastructureInvocation(t.Context())
	if !errors.Is(invocation.err, infrastructure.ErrIdentity) {
		t.Fatal("fixture did not capture an invalid actual origin")
	}
	store := newBareInspectionStore(t)
	selected := newInfrastructurePreparationFixture(t).selected
	routing := infrastructureOwnerRouting{invocation: invocation, defaultRunner: "/unavailable-managed-runner"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	before := snapshotContinuationSuccessorTestState(t, store.Root)
	for _, initial := range []infrastructure.InitialOperation{
		{},
		{Kind: infrastructure.InitialRecoverSaved},
		{Kind: infrastructure.InitialMigrateReferenced},
		{Kind: infrastructure.InitialMigrateOwn},
	} {
		_, err := routing.prepare(ctx, store, selected, initial)
		if !errors.Is(err, infrastructure.ErrIdentity) {
			t.Fatalf("stored source fault did not precede target selection: %v", err)
		}
		if !maps.Equal(before, snapshotContinuationSuccessorTestState(t, store.Root)) {
			t.Fatal("invalid source staged target context or artifacts")
		}
	}
}

func TestInfrastructureRoutingRequiresCapturedFiniteInvocation(t *testing.T) {
	store := newBareInspectionStore(t)
	selected := newInfrastructurePreparationFixture(t).selected
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	before := snapshotContinuationSuccessorTestState(t, store.Root)
	if _, err := (infrastructureOwnerRouting{}).prepare(ctx, store, selected, infrastructure.InitialOperation{}); !errors.Is(err, infrastructure.ErrIdentity) {
		t.Fatalf("uncaptured invocation accepted: %v", err)
	}
	if _, err := (infrastructureOwnerRouting{}).prepare(t.Context(), store, selected, infrastructure.InitialOperation{}); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("unbounded invocation accepted: %v", err)
	}
	if !maps.Equal(before, snapshotContinuationSuccessorTestState(t, store.Root)) {
		t.Fatal("invalid invocation prepared target")
	}
}

func TestInfrastructureRoutingTypedContactCannotHideAnotherCause(t *testing.T) {
	ref := infrastructure.StartupReference{Sequence: 1, Epoch: strings.Repeat("a", 32), IntentSHA256: strings.Repeat("b", 64)}
	contact := &infrastructure.OwnerContactRequiredError{Startup: ref}
	other := ref
	other.Sequence++
	for _, fixture := range []struct {
		name  string
		err   error
		clean bool
	}{
		{"typed", contact, true},
		{"wrapped", fmt.Errorf("owner selection: %w", contact), true},
		{"same reference", errors.Join(contact, contact), true},
		{"different reference", errors.Join(contact, &infrastructure.OwnerContactRequiredError{Startup: other}), false},
		{"independent cause", errors.Join(contact, os.ErrPermission), false},
		{"retained owner", &retainedPreparationFixtureError{cause: contact}, false},
		{"invalid reference", &infrastructure.OwnerContactRequiredError{}, false},
		{"nil", nil, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			selected, clean := infrastructureRoutingContact(fixture.err)
			if clean != fixture.clean || clean && selected != ref {
				t.Fatalf("unsafe contact fallthrough: clean=%t reference=%+v", clean, selected)
			}
		})
	}
}

func TestInfrastructureRoutingCleanupRetainsActualBorrowedStore(t *testing.T) {
	store := newBareInspectionStore(t)
	other := newBareInspectionStore(t)
	cause := &infrastructureRoutingCleanupError{resources: &infrastructureRoutingResources{target: store}, cause: os.ErrPermission}
	joined := errors.Join(errors.New("earlier unrelated cause"), cause)
	if !cleanupRetainsStore(joined, store) || cleanupRetainsStore(joined, other) {
		t.Fatal("routing cleanup lost actual borrowed Store identity")
	}
	if !errors.Is(joined, os.ErrPermission) {
		t.Fatal("routing cleanup discarded the original failure")
	}
	resources := &infrastructureRoutingResources{target: store}
	if err := resources.finish(os.ErrPermission); !errors.Is(err, os.ErrPermission) || cleanupRetainsStore(err, store) {
		t.Fatal("closed parent references invented retained cleanup")
	}
	if _, err := store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("routing closed caller-owned Store: %v", err)
	}
}

func TestInfrastructureRoutingCleanupKeepsAncestorAdmissionAcrossStores(t *testing.T) {
	storeA := newBareInspectionStore(t)
	storeB, err := taskdir.OpenStore(storeA.Root)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := storeA.AcquireRootAdmissionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := errors.Join(lease.Release(), storeB.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	borrowed, err := supervisor.AcquireOrBorrowRootAdmissionContext(t.Context(), storeB, lease, time.Second)
	if err != nil || borrowed != lease {
		t.Fatalf("second Store did not borrow the same ancestor: %v", err)
	}
	child := &infrastructureRoutingCleanupError{resources: &infrastructureRoutingResources{target: storeB}, cause: os.ErrPermission}
	result := commandResult{err: supervisor.ReleaseOwnedRootAdmission(lease, errors.Join(errors.New("earlier independent cause"), child))}
	var retained *supervisor.RootAdmissionCleanupError
	if !errors.As(result.err, &retained) || retained == nil || retained.Lease() != lease || !lease.CleanupPending() {
		t.Fatalf("routing cleanup lost the actual ancestor: %v", result.err)
	}
	closeObservationStore(&result, storeA)
	closeObservationStore(&result, storeB)
	for _, store := range []*taskdir.Store{storeA, storeB} {
		if _, pinErr := store.PinRootContext(t.Context()); pinErr != nil {
			t.Fatalf("parent or child Store closed behind unresolved cleanup: %v", pinErr)
		}
	}
	assertRoutingCleanupBlocksExclusive(t, storeA)
	// The real routing error is sticky. Explicit test teardown owns this lease;
	// it does not claim that a production cleanup retry or child settlement ran.
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
	exclusive, err := storeA.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if err != nil {
		t.Fatalf("explicit teardown did not release the native ancestor: %v", err)
	}
	if err = exclusive.Release(); err != nil {
		t.Fatal(err)
	}
}

func assertRoutingCleanupBlocksExclusive(t *testing.T, store *taskdir.Store) {
	t.Helper()
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if exclusive != nil {
		if releaseErr := exclusive.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("ancestor SH released behind real routing cleanup: %v", err)
	}
}
