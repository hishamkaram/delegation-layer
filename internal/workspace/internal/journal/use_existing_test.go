package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type existingUseFixture struct {
	parent   string
	store    *taskdir.Store
	resource *Resource
	owners   []*taskdir.LifetimeOwnerLease
}

func newExistingUseFixture(t *testing.T) *existingUseFixture {
	t.Helper()
	parent, err := os.MkdirTemp("", "existing-use-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &existingUseFixture{parent: parent}
	t.Cleanup(func() {
		var closeErr error
		for index := len(fixture.owners) - 1; index >= 0; index-- {
			closeErr = errors.Join(closeErr, fixture.owners[index].Release())
		}
		if fixture.resource != nil {
			closeErr = errors.Join(closeErr, fixture.resource.Close())
		}
		if fixture.store != nil {
			closeErr = errors.Join(closeErr, fixture.store.Close())
		}
		if closeErr != nil {
			t.Errorf("retained existing-use fixture %s: %v", fixture.parent, closeErr)
			return
		}
		if removeErr := os.RemoveAll(fixture.parent); removeErr != nil {
			t.Error(removeErr)
		}
	})
	fixture.store, err = taskdir.InitStore(filepath.Join(parent, "state"))
	if err != nil {
		t.Fatal(err)
	}
	request := ResourceRequest{SchemaVersion: task.SchemaVersion, RootID: fixture.store.RootID, WorkspaceID: testID('a'), TaskID: testID('b'), RequestSHA256: testSHA('c'), SourceCwd: filepath.Join(parent, "source"), Container: filepath.Join(parent, "container"), BudgetNanos: int64(time.Minute)}
	fixture.resource, err = EstablishResource(t.Context(), fixture.store, request)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *existingUseFixture) own(lease *taskdir.LifetimeOwnerLease) {
	if lease != nil {
		f.owners = append(f.owners, lease)
	}
}

func TestResourceExistingUseOwnerDoesNotCreateAndPreservesOwnership(t *testing.T) {
	fixture := newExistingUseFixture(t)
	resource := fixture.resource
	lease, present, err := resource.TryAcquireExistingUseOwner(t.Context())
	fixture.own(lease)
	if err != nil || present || lease != nil {
		t.Fatalf("absent use owner: %t %v", present, err)
	}
	path := filepath.Join(fixture.store.Root, "workspaces", resource.Request().WorkspaceID, ".lifetime-owner.lock")
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("existing-only wrapper created use inode: %v", err)
	}
	held, err := resource.AcquireUseOwner(t.Context())
	fixture.own(held)
	if err != nil {
		t.Fatal(err)
	}
	lease, present, err = resource.TryAcquireExistingUseOwner(t.Context())
	fixture.own(lease)
	if !present || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("live use owner missed: %t %v", present, err)
	}
	if err = held.Release(); err != nil {
		t.Fatal(err)
	}
	lease, present, err = resource.TryAcquireExistingUseOwner(t.Context())
	fixture.own(lease)
	if err != nil || !present || lease == nil {
		t.Fatalf("free existing use owner: %t %v", present, err)
	}
	if err = fixture.store.Close(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("use owner lost Store maintenance hold: %v", err)
	}
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestResourceExistingUseOwnerHonorsCanceledContext(t *testing.T) {
	fixture := newExistingUseFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	lease, present, err := fixture.resource.TryAcquireExistingUseOwner(ctx)
	fixture.own(lease)
	if !errors.Is(err, context.Canceled) || present {
		t.Fatalf("canceled existing-use wrapper: %t %v", present, err)
	}
}

func TestWorkspacePhysicalCandidatesRemainDistinctFromCommittedInventory(t *testing.T) {
	fixture := newExistingUseFixture(t)
	request := fixture.resource.Request()
	orphan := testID('d')
	if err := os.Mkdir(filepath.Join(fixture.store.Root, "workspaces", orphan), 0o700); err != nil {
		t.Fatal(err)
	}
	inventoryOnly := request
	inventoryOnly.WorkspaceID, inventoryOnly.TaskID, inventoryOnly.RequestSHA256 = testID('e'), testID('f'), testSHA('a')
	if _, err := establishResourceInventory(t.Context(), fixture.store, inventoryOnly); err != nil {
		t.Fatal(err)
	}
	physical, err := fixture.store.WorkspaceResourceIDs(t.Context())
	if err != nil || !slices.Equal(physical, []string{request.WorkspaceID, orphan}) {
		t.Fatalf("physical candidates hid committed/orphan dirs: %v %v", physical, err)
	}
	committed, err := ResourceIDs(t.Context(), fixture.store)
	if err != nil || !slices.Equal(committed, []string{request.WorkspaceID, inventoryOnly.WorkspaceID}) {
		t.Fatalf("committed inventory changed interpretation: %v %v", committed, err)
	}
	// Even malformed workspace-local data is not decoded by physical enumeration.
	path := filepath.Join(fixture.store.Root, "workspaces", request.WorkspaceID, "request.json")
	if err = os.WriteFile(path, []byte("invalid request"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := fixture.store.WorkspaceResourceIDs(t.Context())
	if err != nil || !slices.Equal(again, physical) {
		t.Fatalf("candidate enumeration read resource records: %v %v", again, err)
	}
}
