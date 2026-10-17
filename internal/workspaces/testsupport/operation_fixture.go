package testsupport

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	guardianfixture "github.com/hishamkaram/delegation-layer/internal/guardian/testsupport"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func NewOperation(t *testing.T) (*taskdir.Store, *workspace.Operation) {
	t.Helper()
	return NewOperationDeadline(t, time.Now().Add(time.Hour))
}

func NewOperationDeadline(t *testing.T, deadline time.Time) (*taskdir.Store, *workspace.Operation) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(supervisorfixture.CanonicalTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	digest := task.ComputeSHA256([]byte("workspace-operation"))
	request := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID,
		WorkspaceID: strings.Repeat("b", 32), OperationID: guardianfixture.TaskID,
		Kind: workspace.OperationCapture, Subject: workspace.Subject{Kind: workspace.SubjectTask, ID: strings.Repeat("c", 32), SHA256: digest},
		Deadline: deadline.UTC().Format(time.RFC3339Nano), WorkerExecutable: "/fixture/delegate-run", WorkerSHA256: digest,
		Supervisor: supervisorfixture.ContinuationSupervisorRef(store.Root, digest, "unix:/fixture/pueue.sock"),
	}
	operation, err := workspace.OpenOperation(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := operation.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, operation
}

func GuardianWithRealRecords(t *testing.T, store *taskdir.Store, operation *workspace.Operation) *guardianfixture.RecordFixture {
	t.Helper()
	return guardianfixture.NewRecordFixture(t, store.RootID, operation.Request().OperationID,
		task.LifetimeOwnerWorkspace, operation.Deadline(), operation,
		func(ctx context.Context) (guardian.Owner, error) { return operation.AcquireLifetimeOwner(ctx) },
		func(options *guardian.Options) {
			options.ReconcileOwned = func(ctx context.Context) error { return guardian.ReconcileRecordsOwned(ctx, operation) }
		})
}

func SeedRealOperationClosure(t *testing.T, store *taskdir.Store, operation *workspace.Operation, readClosure func(context.Context, *workspace.Operation) (guardian.ClosureProof, error)) guardian.ClosureProof {
	t.Helper()
	fixture := GuardianWithRealRecords(t, store, operation)
	fixture.Hello(t)
	fixture.RunCommand(t)
	fixture.CloseTask(t)
	fixture.ReleaseWorker()
	if err := fixture.WaitRun(t); err != nil {
		t.Fatal(err)
	}
	proof, err := readClosure(t.Context(), operation)
	if err != nil {
		t.Fatal(err)
	}
	if proof.TerminationSHA256() == "" || proof.WorkerWaitSHA256() == "" {
		t.Fatal("real guardian fixture did not publish exact termination and worker-wait proofs")
	}
	return proof
}
