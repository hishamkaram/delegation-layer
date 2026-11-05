package tasklifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

type ordinaryAdmissionFixture struct {
	parent string
	store  *taskdir.Store
	stores []*taskdir.Store
	leases []*taskdir.RootAdmissionLease
}

func newOrdinaryAdmissionFixture(t *testing.T) *ordinaryAdmissionFixture {
	t.Helper()
	parent, err := os.MkdirTemp("", "ordinary-admission-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &ordinaryAdmissionFixture{parent: parent}
	t.Cleanup(func() { fixture.close(t) })
	fixture.store, err = taskdir.InitStore(filepath.Join(parent, "state"))
	if fixture.store != nil {
		fixture.stores = append(fixture.stores, fixture.store)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(parent, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *ordinaryAdmissionFixture) own(lease *taskdir.RootAdmissionLease) {
	if lease != nil {
		f.leases = append(f.leases, lease)
	}
}

func (f *ordinaryAdmissionFixture) close(t *testing.T) {
	t.Helper()
	var closure error
	for _, lease := range f.leases {
		closure = errors.Join(closure, lease.Release())
	}
	for _, store := range f.stores {
		closure = errors.Join(closure, store.Close())
	}
	if closure != nil {
		t.Errorf("retained unresolved ordinary-admission fixture %s: %v", f.parent, closure)
		return
	}
	if err := os.RemoveAll(f.parent); err != nil {
		t.Error(err)
	}
}

func TestCreateTaskRootAdmissionPrecedesPreparation(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	store := fixture.store
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	req := task.TaskRecord{TaskID: "0123456789abcdef0123456789abcdef"}
	result := CreateTaskContext(ctx, supervisorcore.AdmissionSelection{}, CreationOptions{}, store, req, []byte("brief"), "")
	if !errors.Is(result.Err(), context.DeadlineExceeded) {
		t.Fatalf("preparation bypassed exclusive root admission: %v", result.Err())
	}
	present, err := store.TaskEntryExists(req.TaskID)
	if err != nil || present {
		t.Fatalf("blocked creator persisted task: %v", err)
	}
}

func TestCreateTaskFailedPreparationReleasesOnlyOwnedRootAdmission(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	store := fixture.store
	result := CreateTaskContext(t.Context(), supervisorcore.AdmissionSelection{}, CreationOptions{}, store, task.TaskRecord{}, nil, "")
	if !errors.Is(result.Err(), task.ErrEvidenceFault) {
		t.Fatalf("missing captured profiles did not refuse preparation: %v", result.Err())
	}
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if err != nil {
		t.Fatalf("failed preparation leaked admission: %v", err)
	}
	if err = exclusive.Release(); err != nil {
		t.Fatal(err)
	}
	shared, err := store.AcquireRootAdmissionContext(t.Context())
	fixture.own(shared)
	if err != nil {
		t.Fatal(err)
	}

	result = CreateTaskContext(t.Context(), supervisorcore.AdmissionSelection{}, CreationOptions{RootAdmission: shared}, store, task.TaskRecord{}, nil, "")
	if !errors.Is(result.Err(), task.ErrEvidenceFault) {
		t.Fatal(result.Err())
	}
	if err = shared.CheckRootContext(t.Context(), store); err != nil {
		t.Fatalf("borrowed owner was released: %v", err)
	}
}

func TestOrdinaryRootAdmissionPreservesLegacyAndManagedContext(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	store := fixture.store
	type invocationKey struct{}
	parent := context.WithValue(t.Context(), invocationKey{}, "invocation")
	ctx, lease, err := ordinaryRootAdmission(parent, store, nil, pueue.Options{})
	fixture.own(lease)

	if err != nil || ctx != parent {
		t.Fatalf("legacy embedding context changed: %v", err)
	}
	managed, borrowed, err := ordinaryRootAdmission(context.WithoutCancel(ctx), store, lease, pueue.Options{ControlOwnerExecutable: filepath.Join(store.Root, "delegate-run")})
	if err != nil || borrowed != lease || managed.Value(invocationKey{}) != "invocation" {
		t.Fatalf("managed propagation lost original resource or values: %v", err)
	}
	if managed == ctx {
		t.Fatal("managed lease was not attached")
	}
	// A later legacy hop must retain an incoming managed context, never remove
	// the capability to make an in-process fallback appear permissible.
	legacy, same, err := ordinaryRootAdmission(managed, store, lease, pueue.Options{})
	if err != nil || legacy != managed || same != lease {
		t.Fatalf("legacy hop stripped managed context: %v", err)
	}
}

func TestDispatchRootAdmissionPrecedesResourceAndTaskPreparation(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	store := fixture.store
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var opened *taskdir.Store
	options := DispatchOptions{OpenStore: func(root string, _ bool) (*taskdir.Store, error) {
		opened, err = taskdir.OpenStore(root)
		if opened != nil {
			fixture.stores = append(fixture.stores, opened)
		}
		return opened, err
	}}
	request := DispatchRequest{Root: store.Root, Cwd: filepath.Join(fixture.parent, "work"), BriefBytes: []byte("brief"), TaskID: "0123456789abcdef0123456789abcdef"}
	result := DispatchContext(ctx, request, options)
	if !errors.Is(result.Err(), context.DeadlineExceeded) {
		t.Fatalf("dispatch reached preparation before root hold: %v", result.Err())
	}
	if result.Resources != nil || result.Creation != nil {
		t.Fatal("blocked dispatch entered resource/preparation phase")
	}
	if opened == nil {
		t.Fatal("store was not opened")
	}
	if _, err = opened.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("owned store not closed: %v", err)
	}
}

func TestCreationResultIncludesClosureFaultAfterSubmission(t *testing.T) {
	closeErr := errors.New("root admission close fault")
	result := CreationResult{RootAdmissionReleaseError: closeErr, Submission: &SubmissionResult{}}
	if !errors.Is(result.Err(), closeErr) {
		t.Fatal("submission hid later root admission closure fault")
	}
}

func TestPassiveStatusDoesNotInitializeRootAdmission(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	path := filepath.Join(fixture.store.Root, taskdir.RootAdmissionLockName)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture unexpectedly has root admission: %v", err)
	}
	_, err := StatusContext(t.Context(), fixture.store, fixture.td, SupervisorOptions{}, "", commonprovider.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive unsubmitted status initialized root admission: %v", err)
	}
}

func TestOrdinaryRecoveryReadsCurrentLaunchRefusal(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	if err := fixture.td.RequireTaskLaunchPermissionContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	permit, err := fixture.td.PrepareStop("fedcba9876543210fedcba9876543210", "user", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	if err = fixture.td.RequireTaskLaunchPermissionContext(t.Context()); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("current stop did not refuse launch: %v", err)
	}
	// Nil command resources are deliberately unreachable after the canonical
	// stop check. Neither path may prepare an actuator or invent a new receipt.
	submitted := SubmitWithClientContext(t.Context(), fixture.td, fixture.request, fixture.meta, nil, "", nil)
	if !errors.Is(submitted.Err(), task.ErrTerminalTask) {
		t.Fatalf("submission passed current refusal: %v", submitted.Err())
	}
	_, err = RecoverStagedSubmission(t.Context(), fixture.store.Root, fixture.td, nil, pueue.Observation{})
	if !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("recovery passed current refusal: %v", err)
	}
}

type admissionReleaseFault struct {
	taskdir.BaseFaultInjector
	enabled atomic.Bool
	cause   error
	name    string
}

func (*admissionReleaseFault) OnLockFileBarrier(string) error      { return nil }
func (*admissionReleaseFault) OnLockParentDirBarrier(string) error { return nil }

func (f *admissionReleaseFault) OnFlock(path string, operation int) error {
	name := f.name
	if name == "" {
		name = taskdir.RootAdmissionLockName
	}
	if f.enabled.Load() && filepath.Base(path) == name && operation == unix.LOCK_UN {
		return f.cause
	}
	return nil
}

func TestCreatorRetainsFailedAdmissionRelease(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	store := fixture.store
	fault := &admissionReleaseFault{cause: errors.New("root admission unlock fault")}
	fault.enabled.Store(true)
	store.SetFaultInjector(fault)
	defer fault.enabled.Store(false)
	result := CreateTaskContext(t.Context(), supervisorcore.AdmissionSelection{}, CreationOptions{}, store, task.TaskRecord{}, nil, "")
	fixture.own(result.RootAdmissionCleanup)
	if !errors.Is(result.Err(), fault.cause) || result.RootAdmissionCleanup == nil {
		t.Fatalf("failed release lost owned handle: %v", result.Err())
	}
	if _, err := store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("creator closed borrowed store: %v", err)
	}
	fault.enabled.Store(false)
	if err := result.RootAdmissionCleanup.Release(); err != nil {
		t.Fatal(err)
	}
	result.RootAdmissionCleanup = nil
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if exclusive != nil {
		t.Cleanup(func() {
			if closeErr := exclusive.Release(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil {
		t.Fatalf("retry did not close original hold: %v", err)
	}
}

func TestDispatchRetainsStoreUntilAdmissionReleaseRetry(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	fault := &admissionReleaseFault{cause: errors.New("dispatch root unlock fault")}
	fault.enabled.Store(true)
	defer fault.enabled.Store(false)
	var opened *taskdir.Store
	options := DispatchOptions{OpenStore: func(root string, _ bool) (*taskdir.Store, error) {
		var err error
		opened, err = taskdir.OpenStore(root)
		if opened != nil {
			fixture.stores = append(fixture.stores, opened)
		}
		if opened != nil {
			opened.SetFaultInjector(fault)
		}
		return opened, err
	}}
	assessment := clearAssessment(time.Now())
	options.resourceAssessment = &assessment
	request := DispatchRequest{Root: fixture.store.Root, Cwd: filepath.Join(fixture.parent, "work"), BriefBytes: []byte("brief"), TaskID: "cccccccccccccccccccccccccccccccc", Provider: "future-agent:exec", Config: task.TaskConfig{Budget: "1m", Permission: "read-only"}}

	result := DispatchContext(t.Context(), request, options)
	fixture.own(result.RootAdmissionCleanup)
	if !errors.Is(result.Err(), fault.cause) || result.RootAdmissionCleanup == nil || result.StoreCleanup != opened {
		t.Fatalf("failed owned release lost lease/store: %v", result.Err())
	}
	if _, err := opened.PinRootContext(t.Context()); err != nil {
		t.Fatalf("store closed before native hold: %v", err)
	}
	fault.enabled.Store(false)
	if err := result.RootAdmissionCleanup.Release(); err != nil {
		t.Fatal(err)
	}
	result.RootAdmissionCleanup = nil
	if err := result.StoreCleanup.Close(); err != nil {
		t.Fatal(err)
	}
	result.StoreCleanup = nil
	exclusive, err := fixture.store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if exclusive != nil {
		t.Cleanup(func() {
			if closeErr := exclusive.Release(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil {
		t.Fatalf("retry left native root hold: %v", err)
	}
}

// This negative runtime never authorizes a helper or invokes a provider.
type refusingWorkspaceRuntime struct{ cause error }

func (r refusingWorkspaceRuntime) ManagedRunner() (string, error) { return "", r.cause }
func (r refusingWorkspaceRuntime) PrepareManaged(context.Context, *taskdir.Store, string) (string, *pueue.Client, error) {
	return "", nil, r.cause
}

func (r refusingWorkspaceRuntime) OpenSaved(context.Context, *taskdir.Store, task.SupervisorRef, infrastructure.SavedBindingSource) (*pueue.Client, error) {
	return nil, r.cause
}

func TestOuterAdmissionWaitsForWorkspaceCleanup(t *testing.T) {
	fixture, store, lease, managed := refusedWorkspaceAdmission(t)
	var err error
	fault := &admissionReleaseFault{name: ".lifetime-owner.lock", cause: errors.New("workspace owner unlock fault")}
	fault.enabled.Store(true)
	fixture.Store.SetFaultInjector(fault)
	childErr := managed.Close()
	var child *workspaces.AdmissionCleanupError
	if !errors.As(childErr, &child) || child.Admission() != managed {
		t.Fatalf("missing actual child cleanup owner: %v", childErr)
	}
	retained, releaseErr := releaseRootAdmission(lease, childErr)
	if retained != lease || releaseErr != nil {
		t.Fatalf("outer native hold lost behind child cleanup: %v", releaseErr)
	}
	retainedStore, closeErr := closeAdmissionStore(store, retained, childErr)
	if retainedStore != store || closeErr != nil {
		t.Fatalf("parent store lost behind child cleanup: %v", closeErr)
	}
	if _, err = store.PinRootContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	exclusive, busyErr := fixture.Store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if exclusive != nil {
		t.Cleanup(func() {
			if cleanupErr := exclusive.Release(); cleanupErr != nil {
				t.Error(cleanupErr)
			}
		})
	}
	if !errors.Is(busyErr, task.ErrLockBusy) {
		t.Fatalf("unresolved child released root SH: %v", busyErr)
	}
	fault.enabled.Store(false)
	if err = child.Admission().Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = releaseRootAdmission(lease, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = closeAdmissionStore(store, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRootAdmissionCleanupNeverBecomesPendingTelemetry(t *testing.T) {
	for _, cause := range []error{task.ErrLockBusy, pueue.ErrUnknown, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			fixture := newOrdinaryAdmissionFixture(t)
			lease, err := fixture.store.AcquireRootAdmissionContext(t.Context())
			fixture.own(lease)
			if err != nil {
				t.Fatal(err)
			}
			fault := &admissionReleaseFault{cause: cause}
			fault.enabled.Store(true)
			fixture.store.SetFaultInjector(fault)
			defer fault.enabled.Store(false)
			cleanupErr := supervisorcore.ReleaseOwnedRootAdmission(lease, cause)
			var cleanup *supervisorcore.RootAdmissionCleanupError
			if !errors.As(cleanupErr, &cleanup) || cleanup.Lease() != lease {
				t.Fatalf("missing native cleanup ownership: %v", cleanupErr)
			}
			if IsPureCollectionPendingError(errors.Join(task.ErrNoSeal, cleanupErr)) || IsPureSupervisorUncertaintyError(errors.Join(pueue.ErrInFlight, cleanupErr)) {
				t.Fatal("retained native owner became pending-only telemetry")
			}
			reported := collectionSupervisorError(t.Context(), cleanupErr)
			var reportedOwner *supervisorcore.RootAdmissionCleanupError
			if !errors.As(reported, &reportedOwner) || reportedOwner.Lease() != lease {
				t.Fatal("collection timeout path discarded its actual cleanup owner")
			}
		})
	}
}

func refusedWorkspaceAdmission(t *testing.T) (*workspacefixture.RecoveryFixture, *taskdir.Store, *taskdir.RootAdmissionLease, *workspaces.Admission) {
	t.Helper()
	fixture := workspacefixture.NewRecoveryFixture(t)
	store, err := taskdir.OpenStore(fixture.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, err := store.AcquireRootAdmissionContext(t.Context())
	if lease != nil {
		t.Cleanup(func() {
			if closeErr := lease.Release(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("workspace runtime deliberately unavailable")
	managed, _, prepareErr := workspaces.PrepareDispatchContext(t.Context(), workspaces.DispatchRequest{WorkspaceID: fixture.Resource.Request().WorkspaceID, RunnerOwnership: task.RunnerOwnershipManaged}, workspaces.OperationOptions{Environment: []string{}, RootAdmission: lease, Runtime: refusingWorkspaceRuntime{cause: refusal}}, fixture.Store, "dddddddddddddddddddddddddddddddd", fixture.Resource.Request().SourceCwd, []byte("brief"))
	if managed != nil {
		t.Cleanup(func() {
			fixture.Store.SetFaultInjector(nil)
			if closeErr := managed.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if managed == nil || !errors.Is(prepareErr, refusal) {
		t.Fatalf("fixture did not reach owned workspace preparation: %v", prepareErr)
	}
	return fixture, store, lease, managed
}
