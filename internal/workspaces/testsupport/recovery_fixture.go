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
	store, err := taskdir.InitStore(filepath.Join(supervisorfixture.CanonicalTempDir(t), "workspace-recovery-state"))
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

func (f *RecoveryFixture) CloseHandles() error {
	var resultErr error
	if f.Resource != nil {
		resultErr = errors.Join(resultErr, f.Resource.Close())
		f.Resource = nil
	}
	if f.Store != nil {
		resultErr = errors.Join(resultErr, f.Store.Close())
		f.Store = nil
	}
	return resultErr
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
