package testsupport

import (
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

// CheckpointOperationRequest is a fixture request for one checkpoint creation
// or removal; the caller supplies its canonical subject and selection.
func CheckpointOperationRequest(store *taskdir.Store, operationID, kind, workspaceID string, subject workspace.Subject, selection *workspace.CheckpointSelection, deadline time.Time) workspace.OperationRequest {
	workerDigest := task.ComputeSHA256([]byte("checkpoint-worker-" + operationID))
	return workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: operationID,
		WorkspaceID: workspaceID, Kind: kind, Subject: subject, Checkpoint: selection,
		Deadline:         deadline.UTC().Format(time.RFC3339Nano),
		WorkerExecutable: "/fixture/delegate-run", WorkerSHA256: workerDigest,
		Supervisor: supervisorfixture.ContinuationSupervisorRef(store.Root, workerDigest, "unix:/fixture/pueue.sock"),
	}
}

// OpenCheckpointOperation opens a fixture request through the production
// journal and closes it with the test.
func OpenCheckpointOperation(t *testing.T, store *taskdir.Store, request workspace.OperationRequest) *workspace.Operation {
	t.Helper()
	operation, err := workspace.OpenOperation(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return operation
}

// OrdinaryCheckpointFixtureSource is a pinned ordinary source whose selected
// directory is Root/sub.
func OrdinaryCheckpointFixtureSource(root string) workspace.Source {
	source := workspace.Source{
		Root: root, CommonDir: filepath.Join(root, ".git"), GitDir: filepath.Join(root, ".git"), RelativeCwd: "sub",
		BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1",
	}
	return source.WithCommonDirIdentity(workspace.DirectoryIdentity{Device: 1, Inode: 2})
}

// ManagedCheckpointFixture records a pinned source and its checkout
// descriptor on resource, as provisioning would, and returns both.
func ManagedCheckpointFixture(t *testing.T, resource *workspace.Resource) (workspace.Source, workspace.Checkout) {
	t.Helper()
	root := filepath.Join(filepath.Dir(resource.StateRoot()), "checkpoint-source")
	source := OrdinaryCheckpointFixtureSource(root)
	source.RelativeCwd = "."
	checkoutPath := filepath.Join(filepath.Dir(resource.StateRoot()), "checkpoint-checkout")
	admin := filepath.Join(root, ".git", "worktrees", "checkout")
	checkout := workspace.Checkout{Source: source, CheckoutPath: checkoutPath, Workdir: checkoutPath, AdminDir: admin, AdminBacklink: admin}
	if err := resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err := resource.RecordCheckout(t.Context(), checkout); err != nil {
		t.Fatal(err)
	}
	return source, checkout
}

// CheckpointFixtureCandidate is a structurally valid candidate of operation
// for target. Its object IDs are fixture values; native Git is not involved.
func CheckpointFixtureCandidate(t *testing.T, operation *workspace.Operation, target workspace.CheckpointTarget) workspace.CheckpointCandidate {
	t.Helper()
	request := operation.Request()
	ref := "refs/delegate/" + request.RootID + "/checkpoints/" + request.OperationID
	head, tree := strings.Repeat("b", 40), strings.Repeat("c", 40)
	payload := "tree " + tree + "\nparent " + head +
		"\nauthor Delegate <delegate@localhost> 1761048000 +0000\ncommitter Delegate <delegate@localhost> 1761048000 +0000\n\n" +
		"Delegate checkpoint\n\n" + workspace.CheckpointTrailer + request.OperationID + "\n" + workspace.CheckpointRequestTrailer + operation.Digest() + "\n"
	return workspace.CheckpointCandidate{
		SchemaVersion: task.SchemaVersion, OperationID: request.OperationID, RequestSHA256: operation.Digest(), Target: target,
		ObservedHEAD: head, IndexSHA256: task.ComputeSHA256([]byte("index")), TreeOID: tree, CommitOID: strings.Repeat("d", 40),
		SnapshotRef: ref, CommitPayload: payload,
	}
}

// SucceededResult is the worker's successful helper result for operation.
func SucceededResult(operation *workspace.Operation) workspace.OperationResult {
	return workspace.OperationResult{SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID, RequestSHA256: operation.Digest(), Status: "succeeded"}
}

// RunRealGuardianLifetime drives the production guardian through one complete
// native lifetime over operation's real records. live runs while the lifetime
// is registered and its worker is running, before the exact worker Wait.
func RunRealGuardianLifetime(t *testing.T, store *taskdir.Store, operation *workspace.Operation, live func()) {
	t.Helper()
	fixture := GuardianWithRealRecords(t, store, operation)
	fixture.Hello(t)
	live()
	fixture.RunCommand(t)
	fixture.CloseTask(t)
	fixture.ReleaseWorker()
	if err := fixture.WaitRun(t); err != nil {
		t.Fatal(err)
	}
}

// OperationDirectory is the fixture path of one workspace operation journal.
func OperationDirectory(store *taskdir.Store, operationID string) string {
	return filepath.Join(store.Root, "workspace-operations", operationID)
}

// WriteOperationFile replaces one named file of an operation journal outside
// the journal's own writers, to model corruption.
func WriteOperationFile(t *testing.T, store *taskdir.Store, operationID, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(OperationDirectory(store, operationID), name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// RemoveOperationFile removes one named file of an operation journal.
func RemoveOperationFile(t *testing.T, store *taskdir.Store, operationID, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(OperationDirectory(store, operationID), name)); err != nil {
		t.Fatal(err)
	}
}

// MakeBareOperationDirectory creates an operation directory with no records,
// as left by a crash before its request was published.
func MakeBareOperationDirectory(t *testing.T, store *taskdir.Store, operationID string) {
	t.Helper()
	if err := os.Mkdir(OperationDirectory(store, operationID), 0o700); err != nil {
		t.Fatal(err)
	}
}

// WriteOperationArtifact publishes complete artifact bytes where a worker
// would before naming them in its result, and returns their descriptor.
func WriteOperationArtifact(t *testing.T, store *taskdir.Store, operationID, name, content string) workspace.Artifact {
	t.Helper()
	directory := workspace.ScratchPath(store.Root, operationID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return workspace.Artifact{Name: name, Length: int64(len(content)), SHA256: task.ComputeSHA256([]byte(content))}
}
