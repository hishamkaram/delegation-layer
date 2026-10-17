package guardian

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestWorkspaceGuardianRejectsUnlaunchedFenceBeforePlanning(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})
	operationID := "abcdef0123456789abcdef0123456789"
	control, err := store.OpenWorkspaceOperation(t.Context(), operationID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Errorf("close control: %v", closeErr)
		}
	})
	deadline := time.Now().Add(-time.Minute)
	operation, err := taskdir.NewControlLifetime(
		control,
		store.RootID,
		operationID,
		task.LifetimeOwnerWorkspace,
		task.ComputeSHA256([]byte("workspace-guardian-owner")),
		deadline,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = operation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
		t.Fatalf("seal workspace fence: %v", err)
	}

	guardian, planCalls, scopeCalls := newWorkspaceGuardianForUnlaunchedFence(t, store, operationID, operation, deadline)
	if err = guardian.Run(t.Context()); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("workspace guardian error = %v, want ErrLifetimeUnlaunched", err)
	}
	if *planCalls != 0 || *scopeCalls != 0 {
		t.Fatalf("workspace fence crossed planning boundary: plan=%d scope=%d", *planCalls, *scopeCalls)
	}
}

func newWorkspaceGuardianForUnlaunchedFence(
	t *testing.T,
	store *taskdir.Store,
	operationID string,
	operation *taskdir.ControlLifetime,
	deadline time.Time,
) (*Guardian, *int, *int) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		closeWorkspaceGuardianTestConnection(t, serverConn)
		closeWorkspaceGuardianTestConnection(t, clientConn)
	})
	ref, err := lifetime.NewRef(lifetime.KindProcessGroup, lifetime.AssuranceCooperative, "workspace-test-scope")
	if err != nil {
		t.Fatal(err)
	}
	scope := &lifetimeGuardianTestScope{ref: ref}
	planCalls := 0
	scopeCalls := 0
	guardian, err := New(Options{
		RootID:    store.RootID,
		TaskID:    operationID,
		OwnerKind: task.LifetimeOwnerWorkspace,
		Deadline:  deadline,
		Records:   operation,
		AcquireOwner: func(ctx context.Context) (Owner, error) {
			return operation.AcquireLifetimeOwner(ctx)
		},
		ReconcileOwned: func(context.Context) error {
			t.Fatal("workspace fence attempted native recovery")
			return nil
		},
		Worker:     exec.Command("unused-workspace-worker"),
		Connection: serverConn,
		PlanFactory: func(string, string, string) (lifetime.Plan, error) {
			planCalls++
			return lifetime.NewPlan(lifetime.KindProcessGroup, lifetime.AssuranceCooperative, "workspace-plan")
		},
		ScopeFactory: func(lifetime.Plan) (Scope, error) {
			scopeCalls++
			return scope, nil
		},
		Now: func() time.Time { return deadline.Add(-time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return guardian, &planCalls, &scopeCalls
}

func closeWorkspaceGuardianTestConnection(t *testing.T, connection net.Conn) {
	t.Helper()
	if err := connection.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close workspace guardian test connection: %v", err)
	}
}

type lifetimeGuardianRecordsWithoutUnlaunched struct{ Records }

func TestWorkspaceGuardianRequiresTypedUnlaunchedReader(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close server connection: %v", err)
		}
		if err := clientConn.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close client connection: %v", err)
		}
	})
	records := &lifetimeGuardianTestRecords{ownerKind: task.LifetimeOwnerWorkspace}
	guardian := &Guardian{options: Options{
		OwnerKind: task.LifetimeOwnerWorkspace,
		Records:   lifetimeGuardianRecordsWithoutUnlaunched{Records: records},
	}}
	if err := guardian.refuseTerminalReplay(t.Context()); !errors.Is(err, errLifetimeGuardianInvalid) {
		t.Fatalf("workspace records without fence reader = %v, want invalid guardian configuration", err)
	}
}
