package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

type nativeTurn struct {
	request     task.TaskRecord
	reservation workspace.Reservation
	subject     workspace.Subject
}

func (l nativeLineage) beginTurn(t *testing.T, resource *workspace.Resource, registration workspace.Registration) nativeTurn {
	t.Helper()
	taskID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	digest := task.ComputeSHA256([]byte("turn " + taskID))
	reservation, err := resource.Reserve(l.scope.ctx, workspace.Subject{Kind: workspace.SubjectAdmission, ID: taskID, SHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: l.store.RootID, TaskID: taskID, Provider: "fixture", Mode: "read-only",
		CanonicalCwd: registration.CanonicalCwd, BudgetNanos: int64(time.Minute), BriefSHA256: digest,
		Workspace: &task.WorkspaceBinding{ID: resource.Request().WorkspaceID, Reservation: reservation.Sequence, RequestSHA256: digest, BudgetNanos: int64(time.Minute)},
	}
	subject, err := workspacescore.TaskSubject(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordCaptureObligationWithBudget(l.scope.ctx, reservation, subject, int64(time.Minute)); err != nil {
		t.Fatal(err)
	}
	return nativeTurn{request: request, reservation: reservation, subject: subject}
}

func (l nativeLineage) baseline(t *testing.T, resource *workspace.Resource, turn nativeTurn) workspace.TurnBaseline {
	t.Helper()
	if err := l.observeBaseline(turn.request, os.Environ()); err != nil {
		t.Fatalf("prelaunch baseline: %v", err)
	}
	baseline, err := resource.TurnBaseline(l.scope.ctx, turn.reservation.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	return baseline
}

// nativeCapture is one recorded turn candidate with the artifacts its capture
// produced.
type nativeCapture struct {
	workspace.CaptureCandidate
	artifacts []workspace.Artifact
	operation *workspace.Operation
}

func (l nativeLineage) captureTurn(t *testing.T, resource *workspace.Resource, turn nativeTurn) nativeCapture {
	t.Helper()
	operation := l.captureOperation(t, resource, turn.subject)
	if _, err := resource.AssociateOperation(l.scope.ctx, workspace.CapturePurpose(turn.reservation.Sequence), operation.Request()); err != nil {
		t.Fatal(err)
	}
	result, err := l.runOperation(operation, os.Environ())
	if err != nil {
		t.Fatalf("turn capture: %v", err)
	}
	candidate, err := resource.CaptureCandidate(l.scope.ctx, operation.Request().OperationID)
	if err != nil || candidate.Turn == nil {
		t.Fatalf("turn capture candidate = %+v, %v", candidate, err)
	}
	return nativeCapture{CaptureCandidate: candidate, artifacts: result.Artifacts, operation: operation}
}

func (l nativeLineage) sealTurn(t *testing.T, resource *workspace.Resource, turn nativeTurn, capture nativeCapture) {
	t.Helper()
	candidate := capture.CaptureCandidate
	receipt := workspace.CaptureReceipt{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: candidate.OperationID, Subject: turn.subject,
		Status: "captured", Candidate: &candidate, Artifacts: capture.artifacts,
		TerminationSHA256: task.ComputeSHA256([]byte("termination")), WorkerWaitSHA256: task.ComputeSHA256([]byte("wait")),
	}
	if err := resource.RecordCaptureReceipt(l.scope.ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := resource.CloseReservation(l.scope.ctx, turn.reservation, "captured", candidate.OperationID); err != nil {
		t.Fatal(err)
	}
}

func assertTurn(t *testing.T, turn *workspace.TurnObservation, content, head, index string) {
	t.Helper()
	if turn == nil || turn.Status != workspace.TurnObserved || turn.Content != content || turn.HEAD != head || turn.Index != index {
		t.Fatalf("turn observation = %+v, want content=%s head=%s index=%s", turn, content, head, index)
	}
}

func TestNativeTurnBaselinesObserveActualStartAndProjectExactCaptures(t *testing.T) {
	lineage := newNativeLineage(t)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	first, firstCapture := runNativeWorkerTurn(t, lineage, resource, registration)
	writeLineageFile(t, filepath.Join(registration.CheckoutPath, "lead.txt"), "lead edit between turns\n")
	second := lineage.beginTurn(t, resource, registration)
	secondBaseline := lineage.baseline(t, resource, second)
	if secondBaseline.TreeOID == firstCapture.TreeOID || secondBaseline.ObservedHEAD != firstCapture.ObservedHEAD {
		t.Fatalf("second baseline was inferred from the previous capture instead of observed: %+v", secondBaseline)
	}
	if files := lineage.gitIn(t, lineage.main, "ls-tree", "-r", "--name-only", secondBaseline.TreeOID); !strings.Contains(files, "lead.txt") {
		t.Fatalf("second baseline lacks the inter-turn Lead edit:\n%s", files)
	}
	assertProjectedCapture(t, lineage, resource, first, firstCapture.OperationID, "busy")
	tracked := filepath.Join(registration.CheckoutPath, "tracked.txt")
	original, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatal(err)
	}
	writeLineageFile(t, tracked, "staged only\n")
	lineage.gitIn(t, registration.CheckoutPath, "add", "tracked.txt")
	writeLineageFile(t, tracked, string(original))
	secondCapture := lineage.captureTurn(t, resource, second)
	assertTurn(t, secondCapture.Turn, workspace.TurnUnchanged, workspace.TurnUnchanged, workspace.TurnChanged)
	lineage.sealTurn(t, resource, second, secondCapture)
	assertProjectedCapture(t, lineage, resource, first, firstCapture.OperationID, "retained")
	assertLegacyTurnUnavailableAndForgotten(t, lineage, resource, registration, []string{firstCapture.Turn.BaselineRef, secondBaseline.Ref})
}

func runNativeWorkerTurn(t *testing.T, lineage nativeLineage, resource *workspace.Resource, registration workspace.Registration) (nativeTurn, nativeCapture) {
	t.Helper()
	turn := lineage.beginTurn(t, resource, registration)
	indexPath := filepath.Join(registration.AdminDir, "index")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	baseline := lineage.baseline(t, resource, turn)
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(indexBefore, indexAfter) || baseline.ObservedHEAD != registration.BaseOID {
		t.Fatalf("baseline changed the real index or observed the wrong HEAD: %+v, %v", baseline, err)
	}
	if err = lineage.observeBaseline(turn.request, os.Environ()); !errors.Is(err, task.ErrAlreadyStarted) {
		t.Fatalf("second baseline for one launch error = %v, want already started", err)
	}
	if refs := lineage.ownedRefs(t, resource); !strings.Contains(refs, baseline.Ref+" "+baseline.TreeOID) {
		t.Fatalf("baseline tree is not pinned:\n%s", refs)
	}
	writeLineageFile(t, filepath.Join(registration.CheckoutPath, "checkpoint.txt"), "worker checkpoint\n")
	lineage.gitIn(t, registration.CheckoutPath, "add", "checkpoint.txt")
	lineage.commit(t, registration.CheckoutPath, "read-only turn checkpoint")
	writeLineageFile(t, filepath.Join(registration.CheckoutPath, "tracked.txt"), "worker dirty edit\n")
	capture := lineage.captureTurn(t, resource, turn)
	assertTurn(t, capture.Turn, workspace.TurnChanged, workspace.TurnChanged, workspace.TurnChanged)
	if capture.Turn.BaselineRef != baseline.Ref || capture.Turn.StartTreeOID != baseline.TreeOID {
		t.Fatalf("capture did not compare with its own baseline: %+v", capture.Turn)
	}
	lineage.sealTurn(t, resource, turn, capture)
	return turn, capture
}

func assertProjectedCapture(t *testing.T, lineage nativeLineage, resource *workspace.Resource, turn nativeTurn, captureID, state string) {
	t.Helper()
	description, err := workspacescore.DescribeTask(lineage.scope.ctx, resource, turn.request)
	if err != nil || description.Capture == nil || description.Capture.OperationID != captureID || description.State != state {
		t.Fatalf("first task projection = %+v, %v; want its own capture with resource state %s", description, err, state)
	}
}

func assertLegacyTurnUnavailableAndForgotten(t *testing.T, lineage nativeLineage, resource *workspace.Resource, registration workspace.Registration, baselineRefs []string) {
	t.Helper()
	legacy := lineage.beginTurn(t, resource, registration)
	capture := lineage.captureTurn(t, resource, legacy)
	if *capture.Turn != (workspace.TurnObservation{Status: workspace.TurnUnavailable, ReasonCode: workspace.TurnBaselineUnavailable}) {
		t.Fatalf("turn without a baseline = %+v, want unavailable without verdicts", capture.Turn)
	}
	lineage.sealTurn(t, resource, legacy, capture)
	refs := lineage.ownedRefs(t, resource)
	for _, ref := range baselineRefs {
		if !strings.Contains(refs, ref+" ") {
			t.Fatalf("baseline ref %s missing before forget:\n%s", ref, refs)
		}
	}
	releaseAndForgetNativeChild(t, lineage, resource)
	if refs = lineage.ownedRefs(t, resource); refs != "" {
		t.Fatalf("forget left baseline or capture refs:\n%s", refs)
	}
}

func TestNativeTurnBaselineHonorsExpiredTaskDeadline(t *testing.T) {
	lineage := newNativeLineage(t)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	turn := lineage.beginTurn(t, resource, registration)
	expired, cancel := context.WithDeadline(lineage.scope.ctx, time.Now().Add(-time.Second))
	defer cancel()
	expiredLineage := lineage
	expiredLineage.scope.ctx = expired
	if err := expiredLineage.observeBaseline(turn.request, os.Environ()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("baseline after the task deadline error = %v", err)
	}
	if _, err := resource.TurnBaseline(lineage.scope.ctx, turn.reservation.Sequence); !workspace.OperationAbsent(err) {
		t.Fatalf("expired baseline recorded evidence: %v", err)
	}
}
