package supervisor

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSavedClientCancellationPrecedesExecutableSelection(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	invalid := task.SupervisorRef{ConfigPath: pueue.PrivateConfigPath(fixture.store.Root)}
	client, err := RecoverPrivateClient(ctx, fixture.store, invalid, pueue.Options{}, ExecutableLookup{})
	if client != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled saved client selected invalid executable data: %v", err)
	}
	client, err = NewControlClient(ctx, fixture.store.Root, invalid, pueue.Options{}, ExecutableLookup{})
	if client != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled control client selected invalid executable data: %v", err)
	}
}

func TestPairReconciliationRejectsRootReplacedBeforeBootstrapLock(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	if _, err := fixture.store.PinRootContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Simulate replacement after the resident pinned its Store but before it
	// acquired the pathname-based bootstrap lock. Both Stores remain owned.
	if err := os.Rename(fixture.store.Root, fixture.store.Root+"-original"); err != nil {
		t.Fatal(err)
	}
	replacement, err := taskdir.InitStore(fixture.store.Root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.otherStores = append(fixture.otherStores, replacement)
	err = pueue.WithBootstrapLockContext(t.Context(), fixture.store.Root, func(ctx context.Context) error {
		return ReconcilePublishedSupervisorWithLock(ctx, fixture.store, pueue.Options{}, ExecutableLookup{}, false)
	})
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("reconciliation accepted a different root after waiting for its lock: %v", err)
	}
}
