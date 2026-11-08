package infrastructurehost

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func TestInfrastructureRoutingRejectsCapturedSourceFaultBeforeStaging(t *testing.T) {
	t.Setenv(infrastructure.OriginEnvironment, "invalid original invocation")
	invocation := CaptureInvocation(t.Context())
	if !errors.Is(invocation.err, infrastructure.ErrIdentity) {
		t.Fatal("fixture did not capture an invalid actual origin")
	}
	store := supervisorfixture.NewInspectionStore(t)
	selected := newInfrastructurePreparationFixture(t).selected
	routing := Routing{invocation: invocation, defaultRunner: "/unavailable-managed-runner"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	before := supervisorfixture.SnapshotStore(t, store.Root)
	for _, initial := range []infrastructure.InitialOperation{
		{},
		{Kind: infrastructure.InitialRecoverSaved},
		{Kind: infrastructure.InitialMigrateOwn},
	} {
		_, err := routing.prepare(ctx, store, selected, initial)
		if !errors.Is(err, infrastructure.ErrIdentity) {
			t.Fatalf("stored source fault did not precede target selection: %v", err)
		}
		if !maps.Equal(before, supervisorfixture.SnapshotStore(t, store.Root)) {
			t.Fatal("invalid source staged target context or artifacts")
		}
	}
}

func TestInfrastructureRoutingRequiresCapturedFiniteInvocation(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	selected := newInfrastructurePreparationFixture(t).selected
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	before := supervisorfixture.SnapshotStore(t, store.Root)
	if _, err := (Routing{}).prepare(ctx, store, selected, infrastructure.InitialOperation{}); !errors.Is(err, infrastructure.ErrIdentity) {
		t.Fatalf("uncaptured invocation accepted: %v", err)
	}
	if _, err := (Routing{}).prepare(t.Context(), store, selected, infrastructure.InitialOperation{}); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("unbounded invocation accepted: %v", err)
	}
	if !maps.Equal(before, supervisorfixture.SnapshotStore(t, store.Root)) {
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
	store := supervisorfixture.NewInspectionStore(t)
	other := supervisorfixture.NewInspectionStore(t)
	cause := &infrastructureRoutingCleanupError{resources: &infrastructureRoutingResources{target: store}, cause: os.ErrPermission}
	joined := errors.Join(errors.New("earlier unrelated cause"), cause)
	if !taskdir.CleanupRetainsStore(joined, store) || taskdir.CleanupRetainsStore(joined, other) {
		t.Fatal("routing cleanup lost actual borrowed Store identity")
	}
	if !errors.Is(joined, os.ErrPermission) {
		t.Fatal("routing cleanup discarded the original failure")
	}
	resources := &infrastructureRoutingResources{target: store}
	if err := resources.finish(os.ErrPermission); !errors.Is(err, os.ErrPermission) || taskdir.CleanupRetainsStore(err, store) {
		t.Fatal("closed parent references invented retained cleanup")
	}
	if _, err := store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("routing closed caller-owned Store: %v", err)
	}
}
