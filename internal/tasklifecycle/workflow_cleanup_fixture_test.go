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

	"github.com/hishamkaram/delegation-layer/internal/pueue"
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

func (f *ordinaryAdmissionFixture) close(t *testing.T) {
	t.Helper()
	var closure error
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
		name = ".maintenance.lock"
	}
	if f.enabled.Load() && filepath.Base(path) == name && operation == unix.LOCK_UN {
		return f.cause
	}
	return nil
}

type refusingWorkspaceRuntime struct{ cause error }

func (r refusingWorkspaceRuntime) ManagedRunner() (string, error) { return "", r.cause }
func (r refusingWorkspaceRuntime) PrepareManaged(context.Context, *taskdir.Store, string) (string, *pueue.Client, error) {
	return "", nil, r.cause
}

func (r refusingWorkspaceRuntime) OpenSaved(context.Context, *taskdir.Store, task.SupervisorRef, infrastructure.SavedBindingSource) (*pueue.Client, error) {
	return nil, r.cause
}

func refusedWorkspaceAdmission(t *testing.T) (*workspacefixture.RecoveryFixture, *taskdir.Store, *workspaces.Admission) {
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
	refusal := errors.New("workspace runtime deliberately unavailable")
	managed, _, prepareErr := workspaces.PrepareDispatchContext(t.Context(), workspaces.DispatchRequest{WorkspaceID: fixture.Resource.Request().WorkspaceID, RunnerOwnership: task.RunnerOwnershipManaged}, workspaces.OperationOptions{Environment: []string{}, Runtime: refusingWorkspaceRuntime{cause: refusal}}, fixture.Store, "dddddddddddddddddddddddddddddddd", fixture.Resource.Request().SourceCwd, []byte("brief"))
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
	return fixture, store, managed
}
