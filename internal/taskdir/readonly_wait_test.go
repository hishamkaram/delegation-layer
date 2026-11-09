package taskdir

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
)

func TestReadOnlyNeverStartedRequiresSettledCanonicalProof(t *testing.T) {
	store := testStore(t)
	td, expected := readOnlyRefusedTask(t, store)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	before := snapshotTree(t, store.Root)
	proven, reference, err := reader.ReadNeverStartedTaskContext(t.Context(), td.TaskID)
	if err != nil || proven || reference != "" {
		t.Fatalf("unsettled refusal became complete: %t %q %v", proven, reference, err)
	}
	if after := snapshotTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("passive proof created owner or other records: before %v after %v", before, after)
	}
	settled, _, err := td.ProveUnlaunchedTaskReferenceContext(t.Context())
	must(t, err)
	if !settled {
		t.Fatal("fixture settlement failed")
	}
	before = snapshotTree(t, store.Root)
	proven, reference, err = reader.ReadNeverStartedTaskContext(t.Context(), td.TaskID)
	if err != nil || !proven || reference != expected {
		t.Fatalf("settled proof lost: %t %q %v", proven, reference, err)
	}
	if after := snapshotTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("passive proof changed settled storage: before %v after %v", before, after)
	}
	requireMaintenanceReleased(t, store)
}

func TestReadOnlyNeverStartedRetainsBusyOwnerAsUnproven(t *testing.T) {
	store := testStore(t)
	td, _ := readOnlyRefusedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	proven, reference, err := reader.ReadNeverStartedTaskContext(t.Context(), td.TaskID)
	if err != nil || proven || reference != "" {
		t.Fatalf("busy owner became never-started proof: %t %q %v", proven, reference, err)
	}
}

func TestReadOnlyProofCleanupRetainsRootUntilExactLeaseRelease(t *testing.T) {
	store := testStore(t)
	td, _ := readOnlyRefusedTask(t, store)
	settled, _, err := td.ProveUnlaunchedTaskReferenceContext(t.Context())
	must(t, err)
	if !settled {
		t.Fatal("fixture settlement failed")
	}
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	fault := &partialOwnerFault{owner: true, cause: errors.New("injected proof owner unlock")}
	reader.store.SetFaultInjector(fault)
	_, _, err = reader.ReadNeverStartedTaskContext(t.Context(), td.TaskID)
	var retained *LifetimeOwnerCleanupError
	if !errors.Is(err, fault.cause) || !errors.As(err, &retained) || retained.Lease() == nil {
		t.Fatalf("proof discarded exact cleanup ownership: %v", err)
	}
	t.Cleanup(func() {
		fault.owner = false
		must(t, retained.Lease().Release())
		must(t, reader.Close())
	})
	if closeErr := reader.Close(); !errors.Is(closeErr, fault.cause) {
		t.Fatalf("close hid unresolved cleanup: %v", closeErr)
	}
	if _, statErr := reader.store.rootHandle.Stat("."); statErr != nil {
		t.Fatalf("root closed while cleanup retained it: %v", statErr)
	}
	fault.owner = false
	must(t, retained.Lease().Release())
	must(t, reader.Close())
	if _, statErr := reader.store.rootHandle.Stat("."); !errors.Is(statErr, os.ErrClosed) {
		t.Fatalf("resolved reader did not close root: %v", statErr)
	}
}
