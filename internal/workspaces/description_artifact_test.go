package workspaces

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// sealedCaptureWithArtifacts commits a capture whose receipt and helper result
// name the same retained artifacts, through the production capture recorder.
func sealedCaptureWithArtifacts(t *testing.T) (*workspacefixture.RecoveryFixture, string) {
	t.Helper()
	fixture := workspacefixture.NewRecoveryFixture(t)
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: fixture.Resource.Request().TaskID, SHA256: fixture.Resource.Request().RequestSHA256}
	reservation, err := fixture.Resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.Resource.RecordCaptureObligation(t.Context(), reservation, subject); err != nil {
		t.Fatal(err)
	}
	operationID := strings.Repeat("d", 32)
	operation := workspacefixture.OpenRecoveryOperation(t, fixture, operationID, workspace.OperationCapture, subject)
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	source := workspacefixture.RecoverySource(fixture)
	if err = fixture.Resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	artifacts := []workspace.Artifact{
		workspacefixture.WriteOperationArtifact(t, fixture.Store, operationID, workspace.CapturePatchName, "patch\n"),
		workspacefixture.WriteOperationArtifact(t, fixture.Store, operationID, workspace.CaptureNameStatusName, "M\x00file\x00"),
	}
	workspacefixture.RunRealGuardianLifetime(t, fixture.Store, operation, func() {
		candidate := workspace.CaptureCandidate{
			SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID, OperationID: operationID, Subject: subject,
			BaseOID: source.BaseOID, ObservedHEAD: strings.Repeat("e", 40), TreeOID: strings.Repeat("f", 40), ObjectFormat: source.ObjectFormat,
			SnapshotRef: "refs/delegate/capture-" + operationID,
		}
		if recordErr := fixture.Resource.RecordCaptureCandidate(t.Context(), candidate); recordErr != nil {
			t.Fatal(recordErr)
		}
		result := workspacefixture.SucceededResult(operation)
		result.Artifacts = artifacts
		if recordErr := operation.RecordResult(t.Context(), result); recordErr != nil {
			t.Fatal(recordErr)
		}
	})
	result, proof, acceptErr := AcceptedOperation(t.Context(), fixture.Store, operation)
	if _, err = recordWorkspaceCapture(t.Context(), fixture.Resource, operation.Request(), reservation, result, proof, acceptErr); err != nil {
		t.Fatal(err)
	}
	return fixture, operationID
}

func TestDescribeOperationShowsCaptureArtifactsOnceInFlatProjection(t *testing.T) {
	fixture, operationID := sealedCaptureWithArtifacts(t)
	description, err := Describe(t.Context(), fixture.Resource)
	if err != nil || description.Capture == nil || len(description.Artifacts) != 2 {
		t.Fatalf("resource description = %+v, %v", description, err)
	}
	if err = DescribeOperation(t.Context(), fixture.Store, fixture.Resource, operationID, &description); err != nil {
		t.Fatal(err)
	}
	if len(description.Artifacts) != 2 || len(description.Capture.Artifacts) != 2 || description.Operation == nil || len(description.Operation.Artifacts) != 2 {
		t.Fatalf("flat=%d capture=%d operation=%+v; want flat artifacts once and nested lists unchanged", len(description.Artifacts), len(description.Capture.Artifacts), description.Operation)
	}
	for index, artifact := range description.Artifacts {
		if artifact.OperationID != operationID || artifact.Path == "" || artifact.Name != description.Capture.Artifacts[index].Name {
			t.Fatalf("flat artifact %d = %+v", index, artifact)
		}
	}
}

func TestArtifactProjectionKeepsDistinctAndSameSourceDuplicates(t *testing.T) {
	root := "/state"
	captureA, operationB := strings.Repeat("a", 32), strings.Repeat("b", 32)
	patch := workspace.Artifact{Name: workspace.CapturePatchName, Length: 6, SHA256: task.ComputeSHA256([]byte("patch\n"))}
	changed := patch
	changed.SHA256 = task.ComputeSHA256([]byte("other\n"))
	response := Description{EvidenceState: "retained"}
	appendArtifactDescriptions(root, captureA, []workspace.Artifact{patch, patch}, &response)
	if len(response.Artifacts) != 2 {
		t.Fatalf("one source's repeated descriptor was hidden: %+v", response.Artifacts)
	}
	appendArtifactDescriptions(root, captureA, []workspace.Artifact{patch, changed}, &response)
	appendArtifactDescriptions(root, operationB, []workspace.Artifact{patch}, &response)
	if len(response.Artifacts) != 4 || response.Artifacts[2].SHA256 != changed.SHA256 || response.Artifacts[3].OperationID != operationB {
		t.Fatalf("distinct descriptors were not preserved in first-seen order: %+v", response.Artifacts)
	}
	omitted := response
	omitted.Artifacts = append([]ArtifactDescription(nil), response.Artifacts...)
	appendArtifactDescriptions(root, operationB, []workspace.Artifact{{Name: workspace.CapturePatchName, Omitted: "limit"}}, &omitted)
	if len(omitted.Artifacts) != 5 || omitted.Artifacts[4].EvidenceState != "omitted" {
		t.Fatalf("an omission differing in evidence was hidden: %+v", omitted.Artifacts)
	}
}

func TestOrdinaryCheckpointMissingDiagnosticKeepsOriginalError(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openOrdinaryCheckpoint(t, store, strings.Repeat("1", 32), time.Now().Add(time.Hour))
	// The helper's diagnostic directory exists; only the named file is gone.
	workspacefixture.WriteOperationArtifact(t, store, operation.Request().OperationID, "unrelated", "kept\n")
	diagnostic := workspace.Artifact{Name: workspace.FailureDiagnosticName, Length: 5, SHA256: task.ComputeSHA256([]byte("gone\n"))}
	closeCheckpointLifetime(t, store, operation, checkpointNative, func() {
		failed := workspacefixture.SucceededResult(operation)
		failed.Status, failed.ReasonCode, failed.Artifacts = "failed", "checkpoint_failed", []workspace.Artifact{diagnostic}
		if err := operation.RecordResult(t.Context(), failed); err != nil {
			t.Fatal(err)
		}
	})
	// Every cause must be the original absence: no workspace load is joined.
	err := ValidateArtifact(t.Context(), store, operation.Request().OperationID, diagnostic)
	if !workspace.OperationAbsent(err) || errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("missing ordinary diagnostic = %v, want only the original missing-file error", err)
	}
	if _, err = QualifyCheckpoint(t.Context(), store, operation); !workspace.OperationAbsent(err) {
		t.Fatalf("checkpoint with a missing diagnostic = %v, want the missing-file fault", err)
	}
	if _, err = operation.CheckpointReceipt(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("missing diagnostic was qualified: %v", err)
	}
}
