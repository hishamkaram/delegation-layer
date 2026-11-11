package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func guardedProjectStart(t *testing.T) (*TaskDir, *StartPermit, task.DirectoryIdentity) {
	t.Helper()
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
	inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	must(t, err)
	_, err = store.OriginReopenContext(t.Context(), "gateway", "recovery", "generation", inventory.CompletenessReceipt, root)
	must(t, err)
	req, meta, brief := preparedInput(t, store)
	project, err := ObserveProjectContext(t.Context(), req.CanonicalCwd)
	must(t, err)
	req.ConnectorContext = &task.ConnectorContext{RootToken: root, Project: project}
	req.Origin = &task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "operation", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64)}
	td, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	permit, err := td.PrepareStart(0)
	must(t, err)
	t.Cleanup(func() { must(t, permit.Release()) })
	must(t, permit.Consume())
	must(t, td.RecordStarted(1))
	return td, permit, project
}

func TestProjectCustodyUnchangedAndCancelledValidationSeals(t *testing.T) {
	td, permit, expected := guardedProjectStart(t)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := permit.RunnerLease().ValidateProject(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled guard check returned %v", err)
	}
	must(t, permit.RunnerLease().ValidateProject(context.WithoutCancel(cancelled)))
	must(t, permit.WithProjectDirectory(t.Context(), func(file *os.File, identity task.DirectoryIdentity) error {
		if identity != expected {
			t.Errorf("borrowed identity = %+v, want %+v", identity, expected)
		}
		var held unix.Stat_t
		if err := unix.Fstat(int(file.Fd()), &held); err != nil {
			return err
		}
		if uint64(held.Dev) != expected.Device || uint64(held.Ino) != expected.Inode {
			t.Error("borrowed file does not name the original directory")
		}
		return nil
	}))
	_, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	if err = permit.RunnerLease().ValidateProject(t.Context()); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("sealed runner granted new validation authority: %v", err)
	}
	called := false
	err = permit.WithProjectDirectory(t.Context(), func(*os.File, task.DirectoryIdentity) error { called = true; return nil })
	if !errors.Is(err, task.ErrInvalidPermit) || called {
		t.Fatalf("sealed runner granted new project borrow: called=%v err=%v", called, err)
	}
}

func TestProjectCustodyObservedMismatchRemainsPoisonedAfterRestore(t *testing.T) {
	td, permit, expected := guardedProjectStart(t)
	held := expected.Path + "-retained"
	must(t, os.Rename(expected.Path, held))
	must(t, os.Mkdir(expected.Path, 0o700))
	if err := permit.RunnerLease().ValidateProject(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("replacement directory was accepted: %v", err)
	}
	must(t, os.Remove(expected.Path))
	must(t, os.Rename(held, expected.Path))
	if err := permit.RunnerLease().ValidateProject(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("restoring the path erased observed mismatch: %v", err)
	}
	if _, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("poisoned directory custody allowed sealing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(td.Dir, "provider.exit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("poisoned runner published provider exit: %v", err)
	}
}

func TestProjectCustodyBorrowExcludesReleaseAndSeal(t *testing.T) {
	td, permit, _ := guardedProjectStart(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	joined := false
	t.Cleanup(func() {
		unblock()
		if !joined {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	})
	go func() {
		done <- permit.WithProjectDirectory(t.Context(), func(file *os.File, _ task.DirectoryIdentity) error {
			close(entered)
			<-release
			_, err := file.Stat()
			return err
		})
	}()
	ctx, cancel := context.WithTimeout(t.Context(), taskInitializationTestTimeout)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("project borrow did not start:", ctx.Err())
	}
	copy := *permit.RunnerLease()
	if err := copy.Release(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("active borrow did not exclude release: %v", err)
	}
	if _, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("in-flight guard check did not exclude seal: %v", err)
	}
	if _, err := permit.Record(); err != nil {
		t.Fatalf("rejected release still invalidated permit: %v", err)
	}
	unblock()
	err := <-done
	joined = true
	must(t, err)
	_, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, copy.Release())
}

func TestProjectCustodyUnguardedValidationDoesNotGrantBorrow(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	permit := consumeStart(t, td)
	t.Cleanup(func() { must(t, permit.Release()) })
	must(t, permit.RunnerLease().ValidateProject(t.Context()))
	called := false
	err := permit.WithProjectDirectory(t.Context(), func(*os.File, task.DirectoryIdentity) error { called = true; return nil })
	if !errors.Is(err, task.ErrInvalidPermit) || called {
		t.Fatalf("unguarded runner granted project borrow: called=%v err=%v", called, err)
	}
}
