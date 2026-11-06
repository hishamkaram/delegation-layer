package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

type joinedCleanupFault struct {
	taskdir.BaseFaultInjector
	name     string
	cause    error
	attempts int
}

func (*joinedCleanupFault) OnLockFileBarrier(string) error      { return nil }
func (*joinedCleanupFault) OnLockParentDirBarrier(string) error { return nil }
func (f *joinedCleanupFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == f.name && operation == unix.LOCK_UN {
		f.attempts++
		return f.cause
	}
	return nil
}

func joinedObservationFailure(t *testing.T, kind string) (*taskdir.Store, func() error, error) {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	lease := appCleanupLifetimeLease(t, store)
	cleanup := lease.Release
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	store.SetFaultInjector(&joinedCleanupFault{name: ".lifetime-owner.lock", cause: errors.New("joined cleanup fault")})
	if kind == "workspace" {
		owner := &workspaceCommandResources{store: store, childOwner: lease}
		cleanup = owner.close
		return store, cleanup, owner.closeAfter(nil)
	}
	return store, cleanup, lease.ReleaseWithError(errors.New("work failed"))
}

func TestJoinedObservationCleanupRetainsExactLaterStore(t *testing.T) {
	for _, kind := range []string{"lifetime", "workspace"} {
		for _, order := range []string{"settled-first", "pending-first"} {
			t.Run(kind+"/"+order, func(t *testing.T) { assertJoinedObservationStore(t, kind, order) })
		}
	}
}

func assertJoinedObservationStore(t *testing.T, kind, order string) {
	t.Helper()
	historicalStore, settleFirst, historical := joinedObservationFailure(t, kind)
	historicalStore.SetFaultInjector(nil)
	if err := settleFirst(); err != nil {
		t.Fatal(err)
	}
	store, settle, pending := joinedObservationFailure(t, kind)
	other, err := taskdir.OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := other.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	joined := errors.Join(historical, fmt.Errorf("nested: %w", pending))
	if order == "pending-first" {
		joined = errors.Join(pending, fmt.Errorf("nested: %w", historical))
	}
	result := commandResult{err: joined}
	closeObservationStore(&result, store)
	if !errors.Is(result.err, joined) || errors.Is(result.err, task.ErrLockBusy) {
		t.Fatal("attempted Store.Close behind a later pending owner")
	}
	if _, err = store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("historical first cleanup hid exact later Store owner: %v", err)
	}
	closeObservationStore(&result, other)
	if _, err = other.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatal("retained another Store on same root")
	}
	store.SetFaultInjector(nil)
	if err = settle(); err != nil {
		t.Fatal(err)
	}
	closeObservationStore(&result, store)
	if _, err = store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatal("settled history retained Store")
	}
	if workspaces.PendingError(errors.Join(workspace.ErrPending, result.err)) || !errors.Is(result.err, pending) {
		t.Fatal("historical fault became pending telemetry or was lost")
	}
}

// This fixture owns the native children, controls and coordinator resources.
// Cleanup releases children before parents on success and assertion failure.
type joinedWorkspaceChildren struct {
	resources *workspaceCommandResources
	controls  []*taskdir.ControlDir
	leases    []*taskdir.LifetimeOwnerLease
	causes    []error
	fault     *joinedCleanupFault
}

func newJoinedWorkspaceChildren(t *testing.T) *joinedWorkspaceChildren {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	fixture := &joinedWorkspaceChildren{
		resources: &workspaceCommandResources{store: store},
		fault:     &joinedCleanupFault{name: ".lifetime-owner.lock", cause: errors.New("joined lifetime unlock failure")},
	}
	t.Cleanup(func() { fixture.close(t) })
	for _, id := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		fixture.acquireChild(t, id)
	}
	return fixture
}

func (f *joinedWorkspaceChildren) acquireChild(t *testing.T, id string) {
	t.Helper()
	store := f.resources.store
	store.SetFaultInjector(nil)
	control, openErr := store.OpenInspectionContext(t.Context(), id, true)
	if control != nil {
		f.controls = append(f.controls, control)
	}
	if openErr != nil {
		t.Fatal(openErr)
	}
	lease, acquireErr := control.AcquireLifetimeOwner(t.Context())
	if lease != nil {
		f.leases = append(f.leases, lease)
	}
	if acquireErr != nil {
		t.Fatal(acquireErr)
	}
	store.SetFaultInjector(f.fault)
	f.causes = append(f.causes, lease.ReleaseWithError(errors.New(id)))
}

func (f *joinedWorkspaceChildren) close(t *testing.T) {
	t.Helper()
	f.resources.store.SetFaultInjector(nil)
	for _, lease := range f.leases {
		if closeErr := lease.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if closeErr := f.resources.close(); closeErr != nil {
		t.Error(closeErr)
	}
	for _, control := range f.controls {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
}

func TestJoinedWorkspaceCleanupSettlesAllRetainedChildren(t *testing.T) {
	fixture := newJoinedWorkspaceChildren(t)
	resources, fault := fixture.resources, fixture.fault
	store := resources.store
	leases, causes := fixture.leases, fixture.causes
	joined := errors.Join(causes[0], fmt.Errorf("later: %w", causes[1]), causes[0])
	retainedErr := resources.closeAfter(joined)
	var retained *workspaceCommandCleanupError
	if !errors.As(retainedErr, &retained) || !errors.Is(retainedErr, joined) {
		t.Fatal("lost whole joined child ownership")
	}
	before := fault.attempts
	failed := retained.Cleanup()
	if failed == nil || fault.attempts != before+1 || resources.store == nil {
		t.Fatal("explicit cleanup retried duplicate failed child or released parent")
	}
	store.SetFaultInjector(nil)
	if closeErr := leases[0].Release(); closeErr != nil {
		t.Fatal(closeErr)
	}
	borrowed := &workspaceCommandCleanupError{owner: &workspaceCommandResources{store: store, childOwner: leases[0]}, cause: joined}
	if borrowed.RetainedStore() != store {
		t.Fatal("settled first child hid another pending Store borrower")
	}
	if cleanupErr := retained.Cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if leases[0].CleanupPending() || leases[1].CleanupPending() || retained.RetainedStore() != nil {
		t.Fatal("explicit workspace cleanup left a joined sibling behind")
	}
}

func joinedWorkspaceAdmissionFailure(t *testing.T) (*workspaceCommandResources, error) {
	t.Helper()
	fixture := workspacefixture.NewRecoveryFixture(t)
	resources := &workspaceCommandResources{store: fixture.Store}
	var admission *workspaces.Admission
	t.Cleanup(func() {
		fixture.Store.SetFaultInjector(nil)
		if closeErr := errors.Join(admission.Close(), resources.close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	var err error
	admission, _, err = workspaces.PrepareDispatchContext(t.Context(), workspaces.DispatchRequest{
		WorkspaceID: fixture.Resource.Request().WorkspaceID, RunnerOwnership: task.RunnerOwnershipManaged,
	}, workspaces.OperationOptions{Environment: []string{}, Runtime: &checkpointAdmissionProbe{store: fixture.Store}}, fixture.Store,
		"dddddddddddddddddddddddddddddddd", fixture.Resource.Request().SourceCwd, []byte("work"))
	if admission == nil || !errors.Is(err, errCheckpointAdmissionProbe) {
		t.Fatalf("missing real admission: %v", err)
	}
	fixture.Store.SetFaultInjector(&joinedCleanupFault{name: ".lifetime-owner.lock", cause: errors.New("admission close failed")})
	return resources, admission.Close()
}

func TestJoinedWorkspaceAdmissionRetainsWholeCause(t *testing.T) {
	historicalResources, historical := joinedWorkspaceAdmissionFailure(t)
	historicalResources.store.SetFaultInjector(nil)
	var historicalOwner *workspaces.AdmissionCleanupError
	if !errors.As(historical, &historicalOwner) {
		t.Fatal("missing historical admission owner")
	}
	if err := historicalOwner.Admission().Close(); err != nil {
		t.Fatal(err)
	}
	resources, pending := joinedWorkspaceAdmissionFailure(t)
	sibling := errors.New("other joined failure")
	joined := errors.Join(historical, fmt.Errorf("pending: %w", pending), sibling)
	retainedErr := resources.closeAfter(joined)
	var retained *workspaceCommandCleanupError
	if !errors.As(retainedErr, &retained) || !errors.Is(retainedErr, joined) || !errors.Is(retainedErr, sibling) || resources.store == nil {
		t.Fatal("settled admission hid pending sibling or original joined cause was lost")
	}
	resources.store.SetFaultInjector(nil)
	if err := retained.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if retained.RetainedStore() != nil {
		t.Fatal("settled admission still retained Store")
	}
}
