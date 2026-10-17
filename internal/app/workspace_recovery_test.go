package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func recoveryRegistration(f *workspacefixture.RecoveryFixture, source workspace.Source) workspace.Registration {
	checkout := filepath.Join(f.Resource.Request().Container, f.Resource.Request().WorkspaceID, "checkout")
	return workspace.Registration{
		SchemaVersion: task.SchemaVersion, WorkspaceID: f.Resource.Request().WorkspaceID,
		SourceRoot: source.Root, CommonDir: source.CommonDir, GitDir: source.GitDir,
		AdminDir: filepath.Join(checkout, ".git"), AdminBacklink: filepath.Join(checkout, ".git"),
		CheckoutPath: checkout, CanonicalCwd: checkout, RelativeCwd: source.RelativeCwd,
		BaseOID: source.BaseOID, ObjectFormat: source.ObjectFormat,
		ContainerIdentity: workspace.DirectoryIdentity{Device: 1, Inode: 2},
		CheckoutIdentity:  workspace.DirectoryIdentity{Device: 1, Inode: 3},
	}
}

func seedWorkspaceRegistration(t *testing.T, store *taskdir.Store, workspaceID string, record workspace.Registration) {
	t.Helper()
	control, err := store.OpenWorkspace(t.Context(), workspaceID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	var created bool
	if err = control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		var putErr error
		created, putErr = tx.Put("registration.json", record)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("registration seed did not create a durable winner")
	}
}

func seedWorkspaceRemoved(t *testing.T, store *taskdir.Store, workspaceID string, record workspace.RemovedRecord) {
	t.Helper()
	control, err := store.OpenWorkspace(t.Context(), workspaceID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	var created bool
	if err = control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		var putErr error
		created, putErr = tx.Put("removed.json", record)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("removed seed did not create a durable winner")
	}
}

func seedWorkspaceForgotten(t *testing.T, store *taskdir.Store, workspaceID string, record workspace.ForgottenRecord) {
	t.Helper()
	control, err := store.OpenWorkspace(t.Context(), workspaceID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	var created bool
	if err = control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		var putErr error
		created, putErr = tx.Put("forgotten.json", record)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("forgotten seed did not create a durable winner")
	}
}

func seedRealWorkspaceOperationClosure(t *testing.T, store *taskdir.Store, operation *workspace.Operation) guardian.ClosureProof {
	return workspacefixture.SeedRealOperationClosure(t, store, operation, workspacescore.OperationClosed)
}

func prepareRegistrationRecovery(t *testing.T, fixture *workspacefixture.RecoveryFixture, name string) {
	t.Helper()
	source := workspacefixture.RecoverySource(fixture)
	if err := fixture.Resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	registration := recoveryRegistration(fixture, source)
	switch name {
	case "matching":
		if err := fixture.Resource.RecordRegistration(t.Context(), registration); err != nil {
			t.Fatal(err)
		}
		fixture.Restart(t)
	case "conflicting":
		conflict := registration
		conflict.BaseOID = strings.Repeat("b", 40)
		seedWorkspaceRegistration(t, fixture.Store, fixture.Resource.Request().WorkspaceID, conflict)
		fixture.Restart(t)
	case "unreadable":
		if err := fixture.Resource.RecordRegistration(t.Context(), registration); err != nil {
			t.Fatal(err)
		}
		fixture.CorruptResourceRecord(t, "registration.json")
	}
}

func requireEvidenceFault(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("%s=%v, want evidence fault", label, err)
	}
}

func assertWorkspaceState(t *testing.T, response WorkspaceResponse, err error, wantState string, wantError bool) {
	t.Helper()
	if wantError {
		requireEvidenceFault(t, "description error", err)
		return
	}
	if err != nil || response.State != wantState {
		t.Fatalf("description=%+v err=%v, want state %q", response, err, wantState)
	}
}

func TestWorkspaceRegistrationRecoveryClassifiesRecordStates(t *testing.T) {
	cases := []struct {
		name      string
		wantState string
		wantError bool
	}{
		{name: "absent", wantState: "provisioning"},
		{name: "matching", wantState: "retained"},
		{name: "conflicting", wantError: true},
		{name: "unreadable", wantError: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			prepareRegistrationRecovery(t, fixture, testCase.name)
			response, err := workspacescore.Describe(t.Context(), fixture.Resource)
			assertWorkspaceState(t, response, err, testCase.wantState, testCase.wantError)
		})
	}
}

func recordCaptureAndClose(t *testing.T, fixture *workspacefixture.RecoveryFixture, reservation workspace.Reservation, receipt workspace.CaptureReceipt) {
	t.Helper()
	if err := fixture.Resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Resource.CloseReservation(t.Context(), reservation, receipt.Status, receipt.OperationID); err != nil {
		t.Fatal(err)
	}
}

func prepareCaptureReceiptRecovery(t *testing.T, fixture *workspacefixture.RecoveryFixture, name string) (workspace.Reservation, workspace.CaptureReceipt) {
	t.Helper()
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: fixture.Resource.Request().TaskID, SHA256: fixture.Resource.Request().RequestSHA256}
	reservation, err := fixture.Resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.Resource.RecordCaptureObligation(t.Context(), reservation, subject); err != nil {
		t.Fatal(err)
	}
	receipt := workspace.CaptureReceipt{
		SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID, OperationID: strings.Repeat("d", 32),
		Subject: subject, Status: "unavailable", ReasonCode: "capture_failed",
		TerminationSHA256: task.ComputeSHA256([]byte("termination proof")), WorkerWaitSHA256: task.ComputeSHA256([]byte("worker wait proof")),
	}
	switch name {
	case "absent":
		fixture.Restart(t)
	case "matching", "conflicting":
		recordCaptureAndClose(t, fixture, reservation, receipt)
		fixture.Restart(t)
	case "unreadable":
		recordCaptureAndClose(t, fixture, reservation, receipt)
		fixture.CorruptResourceRecord(t, "capture-receipt-"+receipt.OperationID+".json")
	}
	return reservation, receipt
}

func settleCaptureReceiptRecovery(t *testing.T, fixture *workspacefixture.RecoveryFixture, name string, reservation workspace.Reservation, receipt workspace.CaptureReceipt) {
	t.Helper()
	switch name {
	case "absent":
		busy, busyErr := workspacescore.Describe(t.Context(), fixture.Resource)
		if busyErr != nil || busy.State != "busy" || busy.NextAction != "watch_workspace" {
			t.Fatalf("unpublished capture obligation description=%+v err=%v", busy, busyErr)
		}
		recordCaptureAndClose(t, fixture, reservation, receipt)
	case "matching":
		recordCaptureAndClose(t, fixture, reservation, receipt)
	case "conflicting":
		conflict := receipt
		conflict.ReasonCode = "different_capture"
		requireEvidenceFault(t, "conflicting receipt write", fixture.Resource.RecordCaptureReceipt(t.Context(), conflict))
	case "unreadable":
		requireEvidenceFault(t, "unreadable receipt replay", fixture.Resource.RecordCaptureReceipt(t.Context(), receipt))
	}
}

func assertCaptureDescription(t *testing.T, response WorkspaceResponse, err error, receipt workspace.CaptureReceipt, wantError bool) {
	t.Helper()
	if wantError {
		requireEvidenceFault(t, "description error", err)
		return
	}
	if err != nil || response.Capture == nil || response.Capture.OperationID != receipt.OperationID || response.Capture.Status != receipt.Status {
		t.Fatalf("capture description=%+v err=%v", response, err)
	}
}

func TestWorkspaceCaptureReceiptRecoveryClassifiesRecordStates(t *testing.T) {
	cases := []struct {
		name      string
		wantError bool
	}{
		{name: "absent"},
		{name: "matching"},
		{name: "conflicting"},
		{name: "unreadable", wantError: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			reservation, receipt := prepareCaptureReceiptRecovery(t, fixture, testCase.name)
			settleCaptureReceiptRecovery(t, fixture, testCase.name, reservation, receipt)
			response, err := workspacescore.Describe(t.Context(), fixture.Resource)
			assertCaptureDescription(t, response, err, receipt, testCase.wantError)
		})
	}
}

func prepareOperationResultRecovery(t *testing.T, fixture *workspacefixture.RecoveryFixture, name string) (string, workspace.OperationResult) {
	t.Helper()
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: fixture.Resource.Request().TaskID, SHA256: fixture.Resource.Request().RequestSHA256}
	operationID := strings.Repeat("e", 32)
	operation := workspacefixture.OpenRecoveryOperation(t, fixture, operationID, workspace.OperationCapture, subject)
	seedRealWorkspaceOperationClosure(t, fixture.Store, operation)
	failure := workspace.OperationResult{
		SchemaVersion: task.SchemaVersion, OperationID: operationID, RequestSHA256: operation.Digest(),
		Status: "failed", ReasonCode: "capture_failed",
	}
	success := workspacefixture.SuccessfulCaptureResult(operation)
	switch name {
	case "matching", "unreadable":
		if err := operation.RecordResult(t.Context(), success); err != nil {
			t.Fatal(err)
		}
	case "conflicting":
		if err := operation.RecordResult(t.Context(), failure); err != nil {
			t.Fatal(err)
		}
	case "absent":
	}
	if err := operation.Close(); err != nil {
		t.Fatal(err)
	}
	switch name {
	case "unreadable":
		fixture.CorruptOperationRecord(t, operationID, "result.json")
	default:
		fixture.Restart(t)
	}
	return operationID, success
}

func replayOperationResult(t *testing.T, fixture *workspacefixture.RecoveryFixture, name, operationID string, success workspace.OperationResult) {
	t.Helper()
	loaded, err := workspace.LoadOperation(t.Context(), fixture.Store, operationID)
	if err != nil {
		t.Fatal(err)
	}
	switch name {
	case "matching":
		if err = loaded.RecordResult(t.Context(), success); err != nil {
			t.Fatalf("matching result replay=%v", err)
		}
	case "conflicting":
		requireEvidenceFault(t, "conflicting result replay", loaded.RecordResult(t.Context(), success))
	case "unreadable":
		requireEvidenceFault(t, "unreadable result replay", loaded.RecordResult(t.Context(), success))
	case "absent":
	}
	if err = loaded.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertOperationDescription(t *testing.T, name string, response WorkspaceResponse, err error) {
	t.Helper()
	switch name {
	case "absent":
		if !errors.Is(err, workspacescore.ErrOperationResultUnavailable) || response.NextAction == "watch_workspace" || response.ReasonCode != "operation_result_unavailable" {
			t.Fatalf("closed worker with missing result err=%v response=%+v, want unavailable", err, response)
		}
	case "matching":
		if err != nil || response.Operation == nil || response.Operation.Status != "succeeded" {
			t.Fatalf("matching operation description=%+v err=%v", response, err)
		}
	case "conflicting":
		if err == nil || response.Operation == nil || response.Operation.Status != "failed" || response.Operation.ReasonCode != "capture_failed" {
			t.Fatalf("conflicting operation description err=%v response=%+v, want retained failed winner", err, response)
		}
	case "unreadable":
		requireEvidenceFault(t, "unreadable operation description", err)
	}
}

func TestWorkspaceOperationResultRecoveryClassifiesRecordStates(t *testing.T) {
	cases := []struct {
		name string
	}{
		{name: "absent"},
		{name: "matching"},
		{name: "conflicting"},
		{name: "unreadable"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			operationID, success := prepareOperationResultRecovery(t, fixture, testCase.name)
			replayOperationResult(t, fixture, testCase.name, operationID, success)
			response := WorkspaceResponse{}
			err := workspacescore.DescribeOperation(t.Context(), fixture.Store, fixture.Resource, operationID, &response)
			assertOperationDescription(t, testCase.name, response, err)
		})
	}
}

func recordRemoved(t *testing.T, fixture *workspacefixture.RecoveryFixture, operationID string) {
	t.Helper()
	if err := fixture.Resource.RecordRemoved(t.Context(), operationID); err != nil {
		t.Fatal(err)
	}
}

func prepareReleaseRecovery(t *testing.T, fixture *workspacefixture.RecoveryFixture, name string) {
	t.Helper()
	releaseID := strings.Repeat("e", 32)
	release := workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID, OperationID: releaseID}
	if err := fixture.Resource.RecordRelease(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	removed := workspace.RemovedRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID, OperationID: releaseID}
	switch name {
	case "matching":
		recordRemoved(t, fixture, releaseID)
		fixture.Restart(t)
		recordRemoved(t, fixture, releaseID)
	case "conflicting":
		removed.OperationID = strings.Repeat("f", 32)
		seedWorkspaceRemoved(t, fixture.Store, fixture.Resource.Request().WorkspaceID, removed)
		fixture.Restart(t)
		requireEvidenceFault(t, "conflicting removed replay", fixture.Resource.RecordRemoved(t.Context(), releaseID))
	case "unreadable":
		recordRemoved(t, fixture, releaseID)
		fixture.CorruptResourceRecord(t, "removed.json")
	case "absent":
		fixture.Restart(t)
	}
}

func assertReleaseDescription(t *testing.T, name string, response WorkspaceResponse, err error, wantState string, wantError bool) {
	t.Helper()
	assertWorkspaceState(t, response, err, wantState, wantError)
	if !wantError && name == "absent" && response.NextAction != "retry_release" {
		t.Fatalf("absent cleanup next action=%q, want retry_release", response.NextAction)
	}
}

func TestWorkspaceReleaseRecoveryClassifiesCleanupReceiptStates(t *testing.T) {
	cases := []struct {
		name      string
		wantState string
		wantError bool
	}{
		{name: "absent", wantState: "cleanup_pending"},
		{name: "matching", wantState: "released"},
		{name: "conflicting", wantError: true},
		{name: "unreadable", wantError: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			prepareReleaseRecovery(t, fixture, testCase.name)
			response, err := workspacescore.Describe(t.Context(), fixture.Resource)
			assertReleaseDescription(t, testCase.name, response, err, testCase.wantState, testCase.wantError)
		})
	}
}

func recordForgotten(t *testing.T, fixture *workspacefixture.RecoveryFixture, operationID string) {
	t.Helper()
	if err := fixture.Resource.RecordForgotten(t.Context(), operationID); err != nil {
		t.Fatal(err)
	}
}

func prepareForgetRecovery(t *testing.T, fixture *workspacefixture.RecoveryFixture, name string) {
	t.Helper()
	releaseID := strings.Repeat("e", 32)
	forgetID := strings.Repeat("f", 32)
	if err := fixture.Resource.RecordRelease(t.Context(), workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID, OperationID: releaseID}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Resource.RecordRemoved(t.Context(), releaseID); err != nil {
		t.Fatal(err)
	}
	intent := workspace.ForgetIntent{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID, OperationID: forgetID, BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	if err := fixture.Resource.RecordForgetIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	forgotten := workspace.ForgottenRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID, OperationID: forgetID}
	switch name {
	case "matching":
		recordForgotten(t, fixture, forgetID)
		fixture.Restart(t)
		recordForgotten(t, fixture, forgetID)
	case "conflicting":
		forgotten.OperationID = strings.Repeat("a", 32)
		seedWorkspaceForgotten(t, fixture.Store, fixture.Resource.Request().WorkspaceID, forgotten)
		fixture.Restart(t)
		requireEvidenceFault(t, "conflicting forgotten replay", fixture.Resource.RecordForgotten(t.Context(), forgetID))
	case "unreadable":
		recordForgotten(t, fixture, forgetID)
		fixture.CorruptResourceRecord(t, "forgotten.json")
	case "absent":
		fixture.Restart(t)
	}
}

func assertForgetDescription(t *testing.T, response WorkspaceResponse, err error, wantState, wantEvidence string, wantError bool) {
	t.Helper()
	if wantError {
		requireEvidenceFault(t, "description error", err)
		return
	}
	if err != nil || response.State != wantState || response.EvidenceState != wantEvidence {
		t.Fatalf("description=%+v err=%v, want state/evidence %q/%q", response, err, wantState, wantEvidence)
	}
}

func TestWorkspaceForgetRecoveryClassifiesTombstoneStates(t *testing.T) {
	cases := []struct {
		name         string
		wantState    string
		wantEvidence string
		wantError    bool
	}{
		{name: "absent", wantState: "forgetting", wantEvidence: "forgetting"},
		{name: "matching", wantState: "forgotten", wantEvidence: "forgotten"},
		{name: "conflicting", wantError: true},
		{name: "unreadable", wantError: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			prepareForgetRecovery(t, fixture, testCase.name)
			response, err := workspacescore.Describe(t.Context(), fixture.Resource)
			assertForgetDescription(t, response, err, testCase.wantState, testCase.wantEvidence, testCase.wantError)
		})
	}
}
