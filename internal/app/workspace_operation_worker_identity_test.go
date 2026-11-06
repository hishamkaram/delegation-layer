package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func workspaceWorkerIdentityOperation(t *testing.T, legacy bool, path string, deadline time.Time) *workspace.Operation {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	request := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: strings.Repeat("a", 32), WorkspaceID: strings.Repeat("b", 32),
		Kind: workspace.OperationCapture, Subject: workspace.Subject{Kind: workspace.SubjectTask, ID: strings.Repeat("c", 32), SHA256: strings.Repeat("d", 64)},
		Deadline: deadline.UTC().Format(time.RFC3339Nano), WorkerExecutable: path,
		ExecutableIdentity: task.ExecutableIdentityInstalledPathV1,
		Supervisor:         supervisorfixture.ContinuationSupervisorRef(store.Root, strings.Repeat("e", 64), "unix:/fixture/pueue.sock"),
	}
	if legacy {
		request.ExecutableIdentity = ""
		// Canonical historical data, unrelated to the loaded test executable.
		request.WorkerSHA256 = strings.Repeat("f", 64)
	}
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

func TestWorkspaceLoadedWorkerUsesRecordedPathDeadlineAndReceiptWithoutHash(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "installed", true: "historical"}[legacy], func(t *testing.T) {
			operation := workspaceWorkerIdentityOperation(t, legacy, executablePath(t), time.Now().Add(time.Minute))
			digest := operation.Digest()
			if err := validateWorkspaceWorker(t.Context(), operation); !workspace.OperationAbsent(err) {
				t.Fatalf("worker without accepted receipt ran: %v", err)
			}
			if err := operation.RecordReceipt(t.Context(), 9); err != nil {
				t.Fatal(err)
			}
			if err := validateWorkspaceWorker(t.Context(), operation); err != nil {
				t.Fatalf("loaded selected worker required executable bytes: %v", err)
			}
			if operation.Digest() != digest {
				t.Fatal("worker validation rewrote accepted operation")
			}
		})
	}
}

func TestWorkspaceLoadedWorkerRefusesDifferentPathAndExpiredDeadline(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "different path", true: "expired"}[expired], func(t *testing.T) {
			path, deadline := "/another/delegate-run", time.Now().Add(time.Minute)
			if expired {
				path, deadline = executablePath(t), time.Now().Add(-time.Second)
			}
			operation := workspaceWorkerIdentityOperation(t, false, path, deadline)
			if err := validateWorkspaceWorker(t.Context(), operation); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("unqualified loaded worker accepted: %v", err)
			}
		})
	}
}
