package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	"github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestWorkspaceFailureDiagnosticSurvivesClosureAndReopen(t *testing.T) {
	store, operation := testsupport.NewOperation(t)
	cause := errors.New("git worktree add: exit status 128: distinctive native diagnostic")
	if err := recordWorkspaceWorkerFailure(t.Context(), store, operation, cause); !errors.Is(err, cause) {
		t.Fatalf("failure lost its original cause: %v", err)
	}
	seedRealWorkspaceOperationClosure(t, store, operation)
	if err := workspaces.ReconcileEndedWorkspaceOperation(t.Context(), operation, pueue.Job{}); err != nil {
		t.Fatalf("closed helper was not eligible for queue cleanup: %v", err)
	}
	reopened, err := workspace.LoadOperation(t.Context(), store, operation.Request().OperationID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := reopened.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	result, proof, resultErr := workspaces.AcceptedOperation(t.Context(), store, reopened)
	var failure *workspaces.OperationError
	if !errors.As(resultErr, &failure) || result.Status != "failed" || len(result.Artifacts) != 1 || proof.WorkerWaitSHA256() == "" {
		t.Fatalf("reopened failure lost its qualified evidence: %+v %+v %v", result, proof, resultErr)
	}
	artifact := result.Artifacts[0]
	if artifact.Name != workspace.FailureDiagnosticName || artifact.Truncated {
		t.Fatalf("failure artifact changed: %+v", artifact)
	}
	content, err := os.ReadFile(filepath.Join(workspace.ScratchPath(store.Root, operation.Request().OperationID), artifact.Name))
	if err != nil || string(content) != cause.Error()+"\n" {
		t.Fatalf("durable diagnostic=%q error=%v", content, err)
	}
}

func TestWorkspaceFailureDiagnosticIsBoundedAndExplicitlyTruncated(t *testing.T) {
	store, operation := testsupport.NewOperation(t)
	cause := errors.New(strings.Repeat("x", int(workspace.ArtifactLimit)+32))
	if err := recordWorkspaceWorkerFailure(t.Context(), store, operation, cause); !errors.Is(err, cause) {
		t.Fatalf("truncated diagnostic lost failure authority: %v", err)
	}
	result, err := operation.Result(t.Context())
	if err != nil || len(result.Artifacts) != 1 {
		t.Fatalf("bounded diagnostic result=%+v error=%v", result, err)
	}
	artifact := result.Artifacts[0]
	if !artifact.Truncated || artifact.Length != workspace.ArtifactLimit {
		t.Fatalf("overflow did not retain an explicit bounded prefix: %+v", artifact)
	}
	if err = workspaces.ValidateArtifact(t.Context(), store, operation.Request().OperationID, artifact); err != nil {
		t.Fatalf("bounded prefix is not digest-qualified: %v", err)
	}
}

func TestWorkspaceDiagnosticWriteFaultStillRecordsKnownFailure(t *testing.T) {
	store, operation := testsupport.NewOperation(t)
	path := workspace.ScratchPath(store.Root, operation.Request().OperationID)
	if err := os.WriteFile(path, []byte("existing non-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("known command failure")
	err := recordWorkspaceWorkerFailure(t.Context(), store, operation, cause)
	var pathErr *os.PathError
	if !errors.Is(err, cause) || !errors.As(err, &pathErr) {
		t.Fatalf("diagnostic IO fault was not reported with original failure: %v", err)
	}
	result, err := operation.Result(t.Context())
	if err != nil || result.Status != "failed" || len(result.Artifacts) != 0 {
		t.Fatalf("diagnostic IO fault suppressed failure or invented an artifact: %+v %v", result, err)
	}
}

func TestWorkspaceArtifactDirectoryBarrierPrecedesResultPublication(t *testing.T) {
	for _, status := range []string{"succeeded", "failed"} {
		t.Run(status, func(t *testing.T) {
			store, operation := testsupport.NewOperation(t)
			artifact := workspace.Artifact{Name: "diff.patch", SHA256: task.ComputeSHA256(nil)}
			result := workspace.OperationResult{SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID, RequestSHA256: operation.Digest(), Status: status}
			if status == "failed" {
				result.ReasonCode, artifact.Name = "injected_failure", workspace.FailureDiagnosticName
				result.Artifacts = []workspace.Artifact{artifact}
			} else {
				result.Artifacts = []workspace.Artifact{artifact, {Name: "name-status.z", SHA256: task.ComputeSHA256(nil)}}
			}
			// Simulate loss of the artifact directory after file closure and
			// before the durable result commit. No result may name those bytes.
			if err := recordWorkspaceWorkerResult(t.Context(), store, operation, result); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing artifact entry did not fail the barrier: %v", err)
			}
			stored, err := operation.Result(t.Context())
			if status == "failed" {
				if err != nil || stored.Status != "failed" || len(stored.Artifacts) != 0 {
					t.Fatalf("known failure was lost on barrier failure: %+v %v", stored, err)
				}
			} else if !workspace.OperationAbsent(err) {
				t.Fatalf("success named artifacts before their directory was durable: %+v %v", stored, err)
			}
		})
	}
}

func TestUnavailableCaptureDoesNotClaimFailureDiagnosticAsGitContent(t *testing.T) {
	fixture := testsupport.NewRecoveryFixture(t)
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: strings.Repeat("c", 32), SHA256: task.ComputeSHA256([]byte("task"))}
	operation := testsupport.OpenRecoveryOperation(t, fixture, strings.Repeat("d", 32), workspace.OperationCapture, subject)
	defer func() {
		if err := operation.Close(); err != nil {
			t.Error(err)
		}
	}()
	cause := errors.New("capture command failed with a durable diagnostic")
	if err := recordWorkspaceWorkerFailure(t.Context(), fixture.Store, operation, cause); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	proof := seedRealWorkspaceOperationClosure(t, fixture.Store, operation)
	result, _, resultErr := workspaces.AcceptedOperation(t.Context(), fixture.Store, operation)
	if len(result.Artifacts) != 1 || resultErr == nil {
		t.Fatalf("failed capture lacks its independent diagnostic: %+v %v", result, resultErr)
	}
	receipt, err := workspaces.BuildCaptureReceipt(t.Context(), fixture.Resource, operation.Request(), result, proof, resultErr)
	if err != nil || receipt.Status != "unavailable" || len(receipt.Artifacts) != 0 {
		t.Fatalf("failure bytes became captured Git content: %+v %v", receipt, err)
	}
	if err = fixture.Resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
		t.Fatalf("unavailable receipt was not a valid durable verdict: %v", err)
	}
}
