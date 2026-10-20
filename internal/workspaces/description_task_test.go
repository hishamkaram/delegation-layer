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

type seededTaskUse struct {
	request     task.TaskRecord
	reservation workspace.Reservation
	subject     workspace.Subject
}

func seedTaskUse(t *testing.T, resource *workspace.Resource, taskID string) seededTaskUse {
	t.Helper()
	digest := task.ComputeSHA256([]byte("admission " + taskID))
	reservation, err := resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectAdmission, ID: taskID, SHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: resource.Request().RootID, TaskID: taskID, Provider: "fixture", Mode: "write",
		CanonicalCwd: "/fixture", BudgetNanos: int64(time.Minute), BriefSHA256: digest,
		Workspace: &task.WorkspaceBinding{ID: resource.Request().WorkspaceID, Reservation: reservation.Sequence, RequestSHA256: digest, BudgetNanos: int64(time.Minute)},
	}
	subject, err := TaskSubject(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordCaptureObligationWithBudget(t.Context(), reservation, subject, int64(time.Minute)); err != nil {
		t.Fatal(err)
	}
	return seededTaskUse{request: request, reservation: reservation, subject: subject}
}

func sealTaskUse(t *testing.T, resource *workspace.Resource, use seededTaskUse, captureID string) {
	t.Helper()
	source, err := resource.Source(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	candidate := workspace.CaptureCandidate{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: captureID, Subject: use.subject,
		BaseOID: source.BaseOID, ObservedHEAD: strings.Repeat("b", 40), TreeOID: strings.Repeat(captureID[:1], 40),
		SnapshotRef: "refs/delegate/capture-" + captureID, ObjectFormat: source.ObjectFormat,
		Turn: &workspace.TurnObservation{Status: workspace.TurnUnavailable, ReasonCode: workspace.TurnBaselineUnavailable},
	}
	if err = resource.RecordCaptureCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	receipt := workspace.CaptureReceipt{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: captureID, Subject: use.subject,
		Status: "captured", Candidate: &candidate, Artifacts: []workspace.Artifact{{Name: "diff.patch", Omitted: "limit"}},
		TerminationSHA256: task.ComputeSHA256([]byte("termination")), WorkerWaitSHA256: task.ComputeSHA256([]byte("wait")),
	}
	if err = resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if err = resource.CloseReservation(t.Context(), use.reservation, "captured", captureID); err != nil {
		t.Fatal(err)
	}
}

func newDescribedTaskResource(t *testing.T) *workspace.Resource {
	t.Helper()
	_, resource := workspacefixture.NewFinalizeFixture(t)
	if err := resource.RecordSource(t.Context(), workspace.Source{Root: "/fixture/source", CommonDir: "/fixture/source/.git", GitDir: "/fixture/source/.git", RelativeCwd: ".", BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1"}); err != nil {
		t.Fatal(err)
	}
	return resource
}

func TestDescribeTaskKeepsOwnCaptureWhileSuccessorUsesWorkspace(t *testing.T) {
	resource := newDescribedTaskResource(t)
	first := seedTaskUse(t, resource, strings.Repeat("1", 32))
	firstCapture := strings.Repeat("3", 32)
	sealTaskUse(t, resource, first, firstCapture)
	second := seedTaskUse(t, resource, strings.Repeat("2", 32))

	assertBusySuccessorProjection(t, resource, first, second, firstCapture)
	secondCapture := strings.Repeat("4", 32)
	sealTaskUse(t, resource, second, secondCapture)
	own, err := DescribeTask(t.Context(), resource, first.request)
	if err != nil || own.Capture == nil || own.Capture.OperationID != firstCapture {
		t.Fatalf("later capture replaced the first task's evidence: %+v, %v", own, err)
	}
	latest, err := Describe(t.Context(), resource)
	if err != nil || latest.Capture == nil || latest.Capture.OperationID != secondCapture {
		t.Fatalf("resource view did not select the latest capture: %+v, %v", latest, err)
	}
}

func assertBusySuccessorProjection(t *testing.T, resource *workspace.Resource, first, second seededTaskUse, firstCapture string) {
	t.Helper()
	own, err := DescribeTask(t.Context(), resource, first.request)
	if err != nil || own.Capture == nil || own.Capture.OperationID != firstCapture || own.Capture.Candidate.Turn == nil {
		t.Fatalf("first task lost its own capture while a successor is busy: %+v, %v", own, err)
	}
	if own.State != "busy" || own.NextAction != "watch_workspace" || own.OperationID != "" {
		t.Fatalf("sealed predecessor evidence made the busy workspace look usable or adopted the successor helper: %+v", own)
	}
	latest, err := Describe(t.Context(), resource)
	if err != nil || latest.State != "busy" || latest.Capture != nil {
		t.Fatalf("resource view = %+v, %v; want busy successor without the predecessor capture", latest, err)
	}
	successor, err := DescribeTask(t.Context(), resource, second.request)
	if err != nil || successor.Capture != nil || successor.State != "busy" {
		t.Fatalf("successor task adopted predecessor evidence: %+v, %v", successor, err)
	}
}

func TestDescribeTaskReportsReleasedAndForgottenResourceState(t *testing.T) {
	resource := newDescribedTaskResource(t)
	first := seedTaskUse(t, resource, strings.Repeat("1", 32))
	captureID := strings.Repeat("3", 32)
	sealTaskUse(t, resource, first, captureID)
	releaseID := strings.Repeat("5", 32)
	if err := resource.RecordRelease(t.Context(), workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: releaseID}); err != nil {
		t.Fatal(err)
	}
	if err := resource.RecordRemoved(t.Context(), releaseID); err != nil {
		t.Fatal(err)
	}
	released, err := DescribeTask(t.Context(), resource, first.request)
	if err != nil || released.State != "released" || released.Capture == nil || released.Capture.OperationID != captureID {
		t.Fatalf("released view = %+v, %v", released, err)
	}
	intent := workspace.ForgetIntent{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: strings.Repeat("6", 32),
		BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}
	if err = resource.RecordForgetIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordForgotten(t.Context(), intent.OperationID); err != nil {
		t.Fatal(err)
	}
	forgotten, err := DescribeTask(t.Context(), resource, first.request)
	if err != nil || forgotten.State != "forgotten" || forgotten.EvidenceState != "forgotten" || forgotten.Capture == nil {
		t.Fatalf("forgotten view = %+v, %v", forgotten, err)
	}
	for _, artifact := range forgotten.Artifacts {
		if artifact.Path != "" {
			t.Fatalf("forgotten view advertised an artifact path: %+v", artifact)
		}
	}
}

func TestDescribeTaskRejectsMissingOrForeignReservation(t *testing.T) {
	resource := newDescribedTaskResource(t)
	first := seedTaskUse(t, resource, strings.Repeat("1", 32))
	missing := first.request
	binding := *missing.Workspace
	binding.Reservation = 9
	missing.Workspace = &binding
	if _, err := DescribeTask(t.Context(), resource, missing); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("missing bound reservation error = %v, want evidence fault", err)
	}
	foreign := first.request
	foreign.TaskID = strings.Repeat("7", 32)
	if _, err := DescribeTask(t.Context(), resource, foreign); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("foreign task selected another task's reservation: %v", err)
	}
	changed := first.request
	changed.BudgetNanos++
	sealTaskUse(t, resource, first, strings.Repeat("3", 32))
	if _, err := DescribeTask(t.Context(), resource, changed); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("changed task request adopted the bound capture: %v", err)
	}
}

func TestForgetInventoryIncludesBaselineRefsAndRefusesCorruptBaseline(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	resource := fixture.Resource
	source := workspacefixture.RecoverySource(fixture)
	if err := resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	use := seedTaskUse(t, resource, strings.Repeat("1", 32))
	baseline := workspace.TurnBaseline{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, Sequence: use.reservation.Sequence, Subject: use.subject,
		BaseOID: source.BaseOID, ObservedHEAD: source.BaseOID, TreeOID: strings.Repeat("d", 40), IndexSHA256: task.ComputeSHA256([]byte("index")),
		ObjectFormat: source.ObjectFormat, Ref: ManagedBaselineRef(fixture.Store.RootID, resource.Request().WorkspaceID, use.reservation.Sequence),
	}
	if err := resource.RecordTurnBaseline(t.Context(), baseline); err != nil {
		t.Fatal(err)
	}
	intent, err := ForgetInventory(t.Context(), fixture.Store, resource, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, reference := range intent.Refs {
		found = found || reference == workspace.ForgetRef{Ref: baseline.Ref, ExpectedOID: baseline.TreeOID}
	}
	if !found {
		t.Fatalf("forget inventory omitted the baseline ref: %+v", intent.Refs)
	}
	fixture.CorruptResourceRecord(t, "turn-baseline-00000000000000000002.json")
	if _, err = ForgetInventory(t.Context(), fixture.Store, fixture.Resource, time.Minute); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("corrupt baseline was skipped by forget inventory: %v", err)
	}
}
