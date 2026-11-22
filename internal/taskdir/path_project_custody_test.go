package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func newPathCustodyFixture(t *testing.T) (*Store, *PathProjectCustody, task.ConnectorContext) {
	t.Helper()
	store := testStore(t)
	constraint, project := pathWorkspaceFixture(t, store)
	root, err := store.RootTokenContext(t.Context())
	must(t, err)
	binding := task.ConnectorContext{RootToken: root, Project: project, WorkspaceConstraint: constraint}
	custody, err := store.NewPathProjectCustody(t.Context(), binding)
	must(t, err)
	return store, custody, binding
}

func TestPathCustodyBorrowsCurrentReplacementWithoutMutatingStore(t *testing.T) {
	store, custody, binding := newPathCustodyFixture(t)
	before := snapshotTree(t, store.Root)
	replacePathDirectory(t, binding.Project.Path)
	current, err := ObserveProjectContext(t.Context(), binding.Project.Path)
	must(t, err)
	binding.WorkspaceConstraint.RelativeProject = "caller mutation"
	var borrowed *os.File
	must(t, custody.WithProjectDirectory(t.Context(), func(file *os.File, launch task.ProjectLaunchContext) error {
		borrowed = file
		if launch.Observed != current || launch.Binding.Project != custody.binding.Project {
			t.Errorf("borrow did not select current canonical path: %+v", launch)
		}
		// A nested validation proves the callback holds no custody mutex.
		return custody.ValidateProject(t.Context())
	}))
	if _, err = borrowed.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("selection file survived its callback: %v", err)
	}
	must(t, os.Rename(current.Path, current.Path+"-after-start"))
	must(t, custody.ValidateProject(t.Context()))
	if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
		t.Fatal("path custody mutated Native state")
	}
}

func TestPathCustodyRefusesEscapedSelectionAndKeepsFaultAfterRestore(t *testing.T) {
	_, custody, binding := newPathCustodyFixture(t)
	project := binding.Project.Path
	must(t, os.Rename(project, project+"-saved"))
	outside := t.TempDir()
	must(t, os.Symlink(outside, project))
	starts := 0
	use := func(*os.File, task.ProjectLaunchContext) error { starts++; return nil }
	if err := custody.WithProjectDirectory(t.Context(), use); err == nil || starts != 0 {
		t.Fatalf("escaped project granted a start borrow: starts=%d error=%v", starts, err)
	}
	must(t, os.Remove(project))
	must(t, os.Rename(project+"-saved", project))
	if err := custody.WithProjectDirectory(t.Context(), use); err == nil || starts != 0 {
		t.Fatalf("restoring the pathname erased custody failure: starts=%d error=%v", starts, err)
	}
}

func TestPathCustodyPreservesCancellationAndRootIdentity(t *testing.T) {
	store, custody, _ := newPathCustodyFixture(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := custody.ValidateProject(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost caller cancellation: %v", err)
	}
	must(t, custody.ValidateProject(t.Context()))
	oldRoot := store.Root + "-held"
	must(t, os.Rename(store.Root, oldRoot))
	must(t, os.MkdirAll(store.Root, 0o700))
	if err := custody.ValidateProject(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("replaced Native root was accepted: %v", err)
	}
	must(t, os.Remove(store.Root))
	must(t, os.Rename(oldRoot, store.Root))
	if err := custody.ValidateProject(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("restoring Native root erased observed replacement: %v", err)
	}
	physical, err := ObserveProjectContext(t.Context(), filepath.Dir(store.Root))
	must(t, err)
	root, err := store.RootTokenContext(t.Context())
	must(t, err)
	if _, err = store.NewPathProjectCustody(t.Context(), task.ConnectorContext{RootToken: root, Project: physical}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("physical approval entered path-only custody: %v", err)
	}
}

func TestPathCustodyClosesFailedAndCanceledBorrows(t *testing.T) {
	_, custody, _ := newPathCustodyFixture(t)
	callbackFault := errors.New("native start refusal")
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		var borrowed *os.File
		want := callbackFault
		if canceled {
			want = context.Canceled
		}
		err := custody.WithProjectDirectory(ctx, func(file *os.File, _ task.ProjectLaunchContext) error {
			borrowed = file
			cancel()
			return want
		})
		cancel()
		if !errors.Is(err, want) {
			t.Fatalf("borrow lost its failure: %v", err)
		}
		if _, err = borrowed.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("failed borrow retained its selection: %v", err)
		}
		must(t, custody.ValidateProject(t.Context()))
	}
}
