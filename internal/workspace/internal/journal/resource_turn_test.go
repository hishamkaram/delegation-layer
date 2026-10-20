package journal

import (
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

func candidateForTest(resource *Resource, source Source, subject Subject, operationID string) CaptureCandidate {
	return CaptureCandidate{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: operationID, Subject: subject,
		BaseOID: source.BaseOID, ObservedHEAD: strings.Repeat("b", 40), TreeOID: strings.Repeat("c", 40),
		SnapshotRef: "refs/delegate/capture-" + operationID, ObjectFormat: source.ObjectFormat,
	}
}

func TestCaptureCandidateTurnEvidenceIsOptionalTypedAndValidated(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	source := testSource(t)
	if err := resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	taskSubject := Subject{Kind: SubjectTask, ID: testID('d'), SHA256: testSHA('e')}
	legacy := candidateForTest(resource, source, taskSubject, testID('1'))
	if err := resource.RecordCaptureCandidate(t.Context(), legacy); err != nil {
		t.Fatalf("legacy-shaped candidate was refused: %v", err)
	}
	observed := candidateForTest(resource, source, taskSubject, testID('2'))
	observed.IndexSHA256 = testSHA('a')
	observed.Turn = contract.ObserveTurn(&contract.TurnBaseline{
		ObservedHEAD: observed.ObservedHEAD, TreeOID: strings.Repeat("d", 40), IndexSHA256: testSHA('a'), Ref: "refs/delegate/baselines/one",
	}, observed)
	if observed.Turn.Content != contract.TurnChanged || observed.Turn.HEAD != contract.TurnUnchanged || observed.Turn.Index != contract.TurnUnchanged {
		t.Fatalf("turn comparison=%+v", observed.Turn)
	}
	if err := resource.RecordCaptureCandidate(t.Context(), observed); err != nil {
		t.Fatal(err)
	}
	stored, err := resource.CaptureCandidate(t.Context(), observed.OperationID)
	if err != nil || !stored.Equal(observed) {
		t.Fatalf("stored candidate=%+v, %v", stored, err)
	}
	assertTurnArtifactsBoundToObservedTurn(t, resource, legacy, stored)
}

func omittedArtifacts(names ...string) []Artifact {
	artifacts := make([]Artifact, 0, len(names))
	for _, name := range names {
		artifacts = append(artifacts, Artifact{Name: name, Omitted: "limit"})
	}
	return artifacts
}

// assertTurnArtifactsBoundToObservedTurn proves the receipt joins turn files
// to the candidate's own turn: an observed turn needs the complete pair and a
// candidate without one never carries turn files.
func assertTurnArtifactsBoundToObservedTurn(t *testing.T, resource *Resource, legacy, observed CaptureCandidate) {
	t.Helper()
	cumulative := []string{contract.CapturePatchName, contract.CaptureNameStatusName}
	complete := append(append([]string(nil), cumulative...), contract.TurnPatchName, contract.TurnNameStatusName)
	receipt := func(candidate CaptureCandidate, names ...string) CaptureReceipt {
		return CaptureReceipt{
			SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: candidate.OperationID, Subject: candidate.Subject,
			Status: "captured", Candidate: &candidate, Artifacts: omittedArtifacts(names...), TerminationSHA256: testSHA('1'), WorkerWaitSHA256: testSHA('2'),
		}
	}
	refused := []CaptureReceipt{
		receipt(observed, cumulative...),
		receipt(observed, append(append([]string(nil), cumulative...), contract.TurnPatchName)...),
		receipt(observed, append(append([]string(nil), complete...), contract.TurnPatchName)...),
		receipt(legacy, complete...),
	}
	for _, candidate := range refused {
		if err := resource.RecordCaptureReceipt(t.Context(), candidate); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("receipt with artifacts %+v for turn %+v error = %v", candidate.Artifacts, candidate.Candidate.Turn, err)
		}
	}
	if err := resource.RecordCaptureReceipt(t.Context(), receipt(legacy, cumulative...)); err != nil {
		t.Fatalf("legacy receipt without turn files was refused: %v", err)
	}
	if err := resource.RecordCaptureReceipt(t.Context(), receipt(observed, complete...)); err != nil {
		t.Fatalf("receipt echo of a turn-bearing candidate was refused: %v", err)
	}
	if _, err := resource.CaptureReceipt(t.Context(), observed.OperationID); err != nil {
		t.Fatalf("receipt reread: %v", err)
	}
}

func TestCaptureCandidateRejectsInvalidOrMisattributedTurn(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	source := testSource(t)
	if err := resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	unavailable := contract.TurnObservation{Status: contract.TurnUnavailable, ReasonCode: contract.TurnBaselineUnavailable}
	operation := candidateForTest(resource, source, subjectFor('9'), testID('3'))
	operation.Turn = &unavailable
	if err := resource.RecordCaptureCandidate(t.Context(), operation); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("verification capture accepted a provider turn: %v", err)
	}
	fabricated := unavailable
	fabricated.Content = contract.TurnUnchanged
	invalid := candidateForTest(resource, source, Subject{Kind: SubjectTask, ID: testID('d'), SHA256: testSHA('e')}, testID('4'))
	invalid.Turn = &fabricated
	if err := resource.RecordCaptureCandidate(t.Context(), invalid); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("unavailable baseline was recorded as clean: %v", err)
	}
}

func TestSourceCommonDirIdentityIsOptionalAndComplete(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	source := testSource(t)
	source.CommonDirDevice = 7
	if err := resource.RecordSource(t.Context(), source); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("partial common-directory identity error=%v", err)
	}
	pinned := testSource(t).WithCommonDirIdentity(DirectoryIdentity{Device: 7, Inode: 11})
	if err := resource.RecordSource(t.Context(), pinned); err != nil {
		t.Fatal(err)
	}
	stored, err := resource.Source(t.Context())
	identity, recorded := stored.CommonDirIdentity()
	if err != nil || !recorded || identity != (DirectoryIdentity{Device: 7, Inode: 11}) {
		t.Fatalf("stored source=%+v, %v", stored, err)
	}
	if _, recorded = testSource(t).CommonDirIdentity(); recorded {
		t.Fatal("legacy source reported a recorded common-directory identity")
	}
}
