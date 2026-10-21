package testsupport

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func OpenSelectionOperation(t *testing.T, fixture *RecoveryFixture, request workspace.OperationRequest) *workspace.Operation {
	t.Helper()
	operation, err := workspace.OpenOperation(t.Context(), fixture.Store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = fixture.Resource.RecordOperation(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	return operation
}

func RecoveryUseOwner(t *testing.T, fixture *RecoveryFixture) {
	t.Helper()
	owner, err := fixture.Resource.TryAcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
}

type RecoveryFixture struct {
	Store    *taskdir.Store
	Resource *workspace.Resource
}

func NewRecoveryFixture(t *testing.T) *RecoveryFixture {
	t.Helper()
	return NewRecoveryFixtureIn(t, supervisorfixture.CanonicalTempDir(t))
}

// RecoveryFixtureRoot is the store root of a recovery fixture under base.
func RecoveryFixtureRoot(base string) string {
	return filepath.Join(base, "workspace-recovery-state")
}

// NewRecoveryFixtureIn establishes a recovery fixture under base, which its
// caller owns and removes; test cleanup only closes the fixture's handles.
func NewRecoveryFixtureIn(t *testing.T, base string) *RecoveryFixture {
	t.Helper()
	store, err := taskdir.InitStore(RecoveryFixtureRoot(base))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &RecoveryFixture{Store: store}
	request := workspace.ResourceRequest{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		WorkspaceID:   strings.Repeat("b", 32),
		TaskID:        strings.Repeat("c", 32),
		RequestSHA256: task.ComputeSHA256([]byte("workspace recovery request")),
		SourceCwd:     filepath.Join(filepath.Dir(store.Root), "workspace-recovery-source"),
		Container:     workspace.ContainerPath(store.Root, store.RootID),
		BudgetNanos:   int64(time.Minute),
	}
	fixture.Resource, err = workspace.EstablishResource(t.Context(), store, request)
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := fixture.CloseHandles(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return fixture
}

// CloseHandles closes the resource and store handles. A handle is cleared
// only after it closes, so a failed close leaves it with its owner.
func (f *RecoveryFixture) CloseHandles() error {
	var resourceErr, storeErr error
	if f.Resource != nil {
		if resourceErr = f.Resource.Close(); resourceErr == nil {
			f.Resource = nil
		}
	}
	if f.Store != nil {
		if storeErr = f.Store.Close(); storeErr == nil {
			f.Store = nil
		}
	}
	return errors.Join(resourceErr, storeErr)
}

func (f *RecoveryFixture) Restart(t *testing.T) {
	t.Helper()
	root := f.Store.Root
	if err := f.CloseHandles(); err != nil {
		t.Fatal(err)
	}
	f.Reopen(t, root)
}

func (f *RecoveryFixture) Reopen(t *testing.T, root string) {
	t.Helper()
	store, err := taskdir.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := workspace.LoadResource(t.Context(), store, strings.Repeat("b", 32))
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	f.Store, f.Resource = store, resource
}

// ReopenRecoveryFixture reopens the recovery fixture another process
// established at root; test cleanup closes these handles.
func ReopenRecoveryFixture(t *testing.T, root string) *RecoveryFixture {
	t.Helper()
	fixture := &RecoveryFixture{}
	fixture.Reopen(t, root)
	t.Cleanup(func() {
		if closeErr := fixture.CloseHandles(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return fixture
}

// PersistSelectionOperation records request as OpenSelectionOperation does
// and closes the operation handle immediately.
func PersistSelectionOperation(t *testing.T, fixture *RecoveryFixture, request workspace.OperationRequest) {
	t.Helper()
	operation, err := workspace.OpenOperation(t.Context(), fixture.Store, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(fixture.Resource.RecordOperation(t.Context(), request), operation.Close()); err != nil {
		t.Fatal(err)
	}
}

func (f *RecoveryFixture) CorruptResourceRecord(t *testing.T, name string) {
	t.Helper()
	root := f.Store.Root
	workspaceID := f.Resource.Request().WorkspaceID
	if err := f.CloseHandles(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workspaces", workspaceID, name)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.Reopen(t, root)
}

func (f *RecoveryFixture) CorruptOperationRecord(t *testing.T, operationID, name string) {
	t.Helper()
	root := f.Store.Root
	if err := f.CloseHandles(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workspace-operations", operationID, name)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.Reopen(t, root)
}

func RecoverySource(f *RecoveryFixture) workspace.Source {
	root := filepath.Join(filepath.Dir(f.Store.Root), "workspace-recovery-git")
	return workspace.Source{
		Root: root, CommonDir: filepath.Join(root, ".git"), GitDir: filepath.Join(root, ".git"), RelativeCwd: ".",
		BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1",
	}
}

func RecoveryOperationRequest(t *testing.T, fixture *RecoveryFixture, operationID, kind string, subject workspace.Subject) workspace.OperationRequest {
	t.Helper()
	workerDigest := task.ComputeSHA256([]byte("workspace-recovery-worker-" + operationID))
	return workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: fixture.Store.RootID, OperationID: operationID,
		WorkspaceID: fixture.Resource.Request().WorkspaceID, Kind: kind, Subject: subject,
		Deadline:         time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		WorkerExecutable: "/fixture/delegate-run", WorkerSHA256: workerDigest,
		Supervisor: supervisorfixture.ContinuationSupervisorRef(fixture.Store.Root, workerDigest, "unix:/fixture/pueue.sock"),
	}
}

func OpenRecoveryOperation(t *testing.T, fixture *RecoveryFixture, operationID, kind string, subject workspace.Subject) *workspace.Operation {
	t.Helper()
	request := RecoveryOperationRequest(t, fixture, operationID, kind, subject)
	operation, err := workspace.OpenOperation(t.Context(), fixture.Store, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.Resource.RecordOperation(t.Context(), request); err != nil {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	return operation
}
