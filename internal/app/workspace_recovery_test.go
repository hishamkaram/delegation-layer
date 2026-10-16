package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

type workspaceRecoveryFixture struct {
	store    *taskdir.Store
	resource *workspace.Resource
}

func newWorkspaceRecoveryFixture(t *testing.T) *workspaceRecoveryFixture {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(canonicalAppTestTempDir(t), "workspace-recovery-state"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &workspaceRecoveryFixture{store: store}
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
	fixture.resource, err = workspace.EstablishResource(t.Context(), store, request)
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := fixture.closeHandles(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return fixture
}

func (f *workspaceRecoveryFixture) closeHandles() error {
	var resultErr error
	if f.resource != nil {
		resultErr = errors.Join(resultErr, f.resource.Close())
		f.resource = nil
	}
	if f.store != nil {
		resultErr = errors.Join(resultErr, f.store.Close())
		f.store = nil
	}
	return resultErr
}

func (f *workspaceRecoveryFixture) restart(t *testing.T) {
	t.Helper()
	root := f.store.Root
	if err := f.closeHandles(); err != nil {
		t.Fatal(err)
	}
	f.reopen(t, root)
}

func (f *workspaceRecoveryFixture) reopen(t *testing.T, root string) {
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
	f.store, f.resource = store, resource
}

func (f *workspaceRecoveryFixture) corruptResourceRecord(t *testing.T, name string) {
	t.Helper()
	root := f.store.Root
	workspaceID := f.resource.Request().WorkspaceID
	if err := f.closeHandles(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workspaces", workspaceID, name)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.reopen(t, root)
}

func (f *workspaceRecoveryFixture) corruptOperationRecord(t *testing.T, operationID, name string) {
	t.Helper()
	root := f.store.Root
	if err := f.closeHandles(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workspace-operations", operationID, name)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.reopen(t, root)
}

func recoverySource(f *workspaceRecoveryFixture) workspace.Source {
	root := filepath.Join(filepath.Dir(f.store.Root), "workspace-recovery-git")
	return workspace.Source{
		Root: root, CommonDir: filepath.Join(root, ".git"), GitDir: filepath.Join(root, ".git"), RelativeCwd: ".",
		BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1",
	}
}

func recoveryRegistration(f *workspaceRecoveryFixture, source workspace.Source) workspace.Registration {
	checkout := filepath.Join(f.resource.Request().Container, f.resource.Request().WorkspaceID, "checkout")
	return workspace.Registration{
		SchemaVersion: task.SchemaVersion, WorkspaceID: f.resource.Request().WorkspaceID,
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

func recoveryOperationRequest(t *testing.T, fixture *workspaceRecoveryFixture, operationID, kind string, subject workspace.Subject) workspace.OperationRequest {
	t.Helper()
	workerDigest := task.ComputeSHA256([]byte("workspace-recovery-worker-" + operationID))
	return workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: fixture.store.RootID, OperationID: operationID,
		WorkspaceID: fixture.resource.Request().WorkspaceID, Kind: kind, Subject: subject,
		Deadline:         time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		WorkerExecutable: "/fixture/delegate-run", WorkerSHA256: workerDigest,
		Supervisor: continuationSupervisorRef(fixture.store.Root, workerDigest, "unix:/fixture/pueue.sock"),
	}
}

func openRecoveryOperation(t *testing.T, fixture *workspaceRecoveryFixture, operationID, kind string, subject workspace.Subject) *workspace.Operation {
	t.Helper()
	request := recoveryOperationRequest(t, fixture, operationID, kind, subject)
	operation, err := workspace.OpenOperation(t.Context(), fixture.store, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.resource.RecordOperation(t.Context(), request); err != nil {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	return operation
}

func seedRealWorkspaceOperationClosure(t *testing.T, store *taskdir.Store, operation *workspace.Operation) workspaceOperationProof {
	t.Helper()
	fixture := workspaceGuardianWithRealRecords(t, store, operation)
	workspaceGuardianCallMust(t, fixture, "hello", fixture.message(lifetimeMessageHello, 0, 0))
	workspaceGuardianCallMust(t, fixture, "begin", fixture.message(lifetimeMessageBegin, 1, 0))
	workspaceGuardianCallMust(t, fixture, "started", fixture.message(lifetimeMessageStarted, 1, 101))
	workspaceGuardianCallMust(t, fixture, "finish", fixture.message(lifetimeMessageFinish, 1, 0))
	workspaceGuardianCallMust(t, fixture, "close", fixture.message(lifetimeMessageClose, 0, 0))
	fixture.worker.release()
	if err := fixture.waitRun(t); err != nil {
		t.Fatal(err)
	}
	proof, err := workspaceOperationClosed(t.Context(), operation)
	if err != nil {
		t.Fatal(err)
	}
	if proof.terminationSHA256 == "" || proof.workerWaitSHA256 == "" {
		t.Fatal("real guardian fixture did not publish exact termination and worker-wait proofs")
	}
	return proof
}

func workspaceGuardianCallMust(t *testing.T, fixture *lifetimeGuardianTestFixture, label string, message lifetimeMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := fixture.client.callContext(ctx, message); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

func successfulCaptureResult(operation *workspace.Operation) workspace.OperationResult {
	return workspace.OperationResult{
		SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID, RequestSHA256: operation.Digest(), Status: "succeeded",
		Artifacts: []workspace.Artifact{{Name: "diff.patch", Omitted: "limit"}, {Name: "name-status.z", Omitted: "limit"}},
	}
}

type provisionRecoveryCase struct {
	name      string
	expectErr error
}

func recoverProvisionOperation(t *testing.T, fixture *workspaceRecoveryFixture, name string) (*workspace.Operation, string, error) {
	t.Helper()
	subject := workspace.Subject{Kind: workspace.SubjectAdmission, ID: fixture.resource.Request().TaskID, SHA256: fixture.resource.Request().RequestSHA256}
	operation := openRecoveryOperation(t, fixture, strings.Repeat("d", 32), workspace.OperationProvision, subject)
	request := operation.Request()
	if _, err := fixture.resource.AssociateOperation(t.Context(), "provision.json", request); err != nil {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	if err := operation.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.restart(t)

	spec := workspaceOperationSpec{purpose: "provision.json", kind: workspace.OperationProvision, subject: subject, budget: time.Minute}
	switch name {
	case "conflicting":
		spec.subject = workspace.Subject{Kind: workspace.SubjectAdmission, ID: strings.Repeat("e", 32), SHA256: task.ComputeSHA256([]byte("changed admission"))}
	case "unreadable":
		fixture.corruptResourceRecord(t, "provision.json")
	}
	recovered, err := ensureWorkspaceOperation(t.Context(), fixture.store, fixture.resource, Dependencies{}, spec)
	return recovered, request.OperationID, err
}

func assertProvisionRecovery(t *testing.T, recovered *workspace.Operation, recoverErr, expectedErr error, operationID string) {
	t.Helper()
	if expectedErr != nil {
		if recoverErr == nil || !errors.Is(recoverErr, expectedErr) {
			t.Fatalf("recovery error=%v, want %v", recoverErr, expectedErr)
		}
		if recovered != nil {
			if closeErr := recovered.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatal("recovery returned an operation after rejecting the durable purpose record")
		}
		return
	}
	if recoverErr != nil || recovered == nil {
		t.Fatalf("matching recovery operation=%v err=%v", recovered, recoverErr)
	}
	if recovered.Request().OperationID != operationID || recovered.Request().Kind != workspace.OperationProvision {
		t.Fatalf("recovered operation=%+v", recovered.Request())
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceProvisionPurposeRecoveryKeepsCreateOnceMapping(t *testing.T) {
	cases := []provisionRecoveryCase{
		{name: "matching"},
		{name: "conflicting", expectErr: task.ErrIdentityMismatch},
		{name: "unreadable", expectErr: task.ErrEvidenceFault},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newWorkspaceRecoveryFixture(t)
			recovered, operationID, recoverErr := recoverProvisionOperation(t, fixture, testCase.name)
			assertProvisionRecovery(t, recovered, recoverErr, testCase.expectErr, operationID)
		})
	}
}

func prepareRegistrationRecovery(t *testing.T, fixture *workspaceRecoveryFixture, name string) {
	t.Helper()
	source := recoverySource(fixture)
	if err := fixture.resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	registration := recoveryRegistration(fixture, source)
	switch name {
	case "matching":
		if err := fixture.resource.RecordRegistration(t.Context(), registration); err != nil {
			t.Fatal(err)
		}
		fixture.restart(t)
	case "conflicting":
		conflict := registration
		conflict.BaseOID = strings.Repeat("b", 40)
		seedWorkspaceRegistration(t, fixture.store, fixture.resource.Request().WorkspaceID, conflict)
		fixture.restart(t)
	case "unreadable":
		if err := fixture.resource.RecordRegistration(t.Context(), registration); err != nil {
			t.Fatal(err)
		}
		fixture.corruptResourceRecord(t, "registration.json")
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
			fixture := newWorkspaceRecoveryFixture(t)
			prepareRegistrationRecovery(t, fixture, testCase.name)
			response, err := describeWorkspace(t.Context(), fixture.resource)
			assertWorkspaceState(t, response, err, testCase.wantState, testCase.wantError)
		})
	}
}

func recordCaptureAndClose(t *testing.T, fixture *workspaceRecoveryFixture, reservation workspace.Reservation, receipt workspace.CaptureReceipt) {
	t.Helper()
	if err := fixture.resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.resource.CloseReservation(t.Context(), reservation, receipt.Status, receipt.OperationID); err != nil {
		t.Fatal(err)
	}
}

func prepareCaptureReceiptRecovery(t *testing.T, fixture *workspaceRecoveryFixture, name string) (workspace.Reservation, workspace.CaptureReceipt) {
	t.Helper()
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: fixture.resource.Request().TaskID, SHA256: fixture.resource.Request().RequestSHA256}
	reservation, err := fixture.resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.resource.RecordCaptureObligation(t.Context(), reservation, subject); err != nil {
		t.Fatal(err)
	}
	receipt := workspace.CaptureReceipt{
		SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.resource.Request().WorkspaceID, OperationID: strings.Repeat("d", 32),
		Subject: subject, Status: "unavailable", ReasonCode: "capture_failed",
		TerminationSHA256: task.ComputeSHA256([]byte("termination proof")), WorkerWaitSHA256: task.ComputeSHA256([]byte("worker wait proof")),
	}
	switch name {
	case "absent":
		fixture.restart(t)
	case "matching", "conflicting":
		recordCaptureAndClose(t, fixture, reservation, receipt)
		fixture.restart(t)
	case "unreadable":
		recordCaptureAndClose(t, fixture, reservation, receipt)
		fixture.corruptResourceRecord(t, "capture-receipt-"+receipt.OperationID+".json")
	}
	return reservation, receipt
}

func settleCaptureReceiptRecovery(t *testing.T, fixture *workspaceRecoveryFixture, name string, reservation workspace.Reservation, receipt workspace.CaptureReceipt) {
	t.Helper()
	switch name {
	case "absent":
		busy, busyErr := describeWorkspace(t.Context(), fixture.resource)
		if busyErr != nil || busy.State != "busy" || busy.NextAction != "watch_workspace" {
			t.Fatalf("unpublished capture obligation description=%+v err=%v", busy, busyErr)
		}
		recordCaptureAndClose(t, fixture, reservation, receipt)
	case "matching":
		recordCaptureAndClose(t, fixture, reservation, receipt)
	case "conflicting":
		conflict := receipt
		conflict.ReasonCode = "different_capture"
		requireEvidenceFault(t, "conflicting receipt write", fixture.resource.RecordCaptureReceipt(t.Context(), conflict))
	case "unreadable":
		requireEvidenceFault(t, "unreadable receipt replay", fixture.resource.RecordCaptureReceipt(t.Context(), receipt))
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
			fixture := newWorkspaceRecoveryFixture(t)
			reservation, receipt := prepareCaptureReceiptRecovery(t, fixture, testCase.name)
			settleCaptureReceiptRecovery(t, fixture, testCase.name, reservation, receipt)
			response, err := describeWorkspace(t.Context(), fixture.resource)
			assertCaptureDescription(t, response, err, receipt, testCase.wantError)
		})
	}
}

func prepareOperationResultRecovery(t *testing.T, fixture *workspaceRecoveryFixture, name string) (string, workspace.OperationResult) {
	t.Helper()
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: fixture.resource.Request().TaskID, SHA256: fixture.resource.Request().RequestSHA256}
	operationID := strings.Repeat("e", 32)
	operation := openRecoveryOperation(t, fixture, operationID, workspace.OperationCapture, subject)
	seedRealWorkspaceOperationClosure(t, fixture.store, operation)
	failure := workspace.OperationResult{
		SchemaVersion: task.SchemaVersion, OperationID: operationID, RequestSHA256: operation.Digest(),
		Status: "failed", ReasonCode: "capture_failed",
	}
	success := successfulCaptureResult(operation)
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
		fixture.corruptOperationRecord(t, operationID, "result.json")
	default:
		fixture.restart(t)
	}
	return operationID, success
}

func replayOperationResult(t *testing.T, fixture *workspaceRecoveryFixture, name, operationID string, success workspace.OperationResult) {
	t.Helper()
	loaded, err := workspace.LoadOperation(t.Context(), fixture.store, operationID)
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
		if !errors.Is(err, errWorkspaceOperationResultUnavailable) || response.NextAction == "watch_workspace" || response.ReasonCode != "operation_result_unavailable" {
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
			fixture := newWorkspaceRecoveryFixture(t)
			operationID, success := prepareOperationResultRecovery(t, fixture, testCase.name)
			replayOperationResult(t, fixture, testCase.name, operationID, success)
			response := WorkspaceResponse{}
			err := describeWorkspaceOperation(t.Context(), fixture.store, fixture.resource, operationID, &response)
			assertOperationDescription(t, testCase.name, response, err)
		})
	}
}

func recordRemoved(t *testing.T, fixture *workspaceRecoveryFixture, operationID string) {
	t.Helper()
	if err := fixture.resource.RecordRemoved(t.Context(), operationID); err != nil {
		t.Fatal(err)
	}
}

func prepareReleaseRecovery(t *testing.T, fixture *workspaceRecoveryFixture, name string) {
	t.Helper()
	releaseID := strings.Repeat("e", 32)
	release := workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.resource.Request().WorkspaceID, OperationID: releaseID}
	if err := fixture.resource.RecordRelease(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	removed := workspace.RemovedRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.resource.Request().WorkspaceID, OperationID: releaseID}
	switch name {
	case "matching":
		recordRemoved(t, fixture, releaseID)
		fixture.restart(t)
		recordRemoved(t, fixture, releaseID)
	case "conflicting":
		removed.OperationID = strings.Repeat("f", 32)
		seedWorkspaceRemoved(t, fixture.store, fixture.resource.Request().WorkspaceID, removed)
		fixture.restart(t)
		requireEvidenceFault(t, "conflicting removed replay", fixture.resource.RecordRemoved(t.Context(), releaseID))
	case "unreadable":
		recordRemoved(t, fixture, releaseID)
		fixture.corruptResourceRecord(t, "removed.json")
	case "absent":
		fixture.restart(t)
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
			fixture := newWorkspaceRecoveryFixture(t)
			prepareReleaseRecovery(t, fixture, testCase.name)
			response, err := describeWorkspace(t.Context(), fixture.resource)
			assertReleaseDescription(t, testCase.name, response, err, testCase.wantState, testCase.wantError)
		})
	}
}

func recordForgotten(t *testing.T, fixture *workspaceRecoveryFixture, operationID string) {
	t.Helper()
	if err := fixture.resource.RecordForgotten(t.Context(), operationID); err != nil {
		t.Fatal(err)
	}
}

func prepareForgetRecovery(t *testing.T, fixture *workspaceRecoveryFixture, name string) {
	t.Helper()
	releaseID := strings.Repeat("e", 32)
	forgetID := strings.Repeat("f", 32)
	if err := fixture.resource.RecordRelease(t.Context(), workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.resource.Request().WorkspaceID, OperationID: releaseID}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.resource.RecordRemoved(t.Context(), releaseID); err != nil {
		t.Fatal(err)
	}
	intent := workspace.ForgetIntent{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.resource.Request().WorkspaceID, OperationID: forgetID, BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	if err := fixture.resource.RecordForgetIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	forgotten := workspace.ForgottenRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.resource.Request().WorkspaceID, OperationID: forgetID}
	switch name {
	case "matching":
		recordForgotten(t, fixture, forgetID)
		fixture.restart(t)
		recordForgotten(t, fixture, forgetID)
	case "conflicting":
		forgotten.OperationID = strings.Repeat("a", 32)
		seedWorkspaceForgotten(t, fixture.store, fixture.resource.Request().WorkspaceID, forgotten)
		fixture.restart(t)
		requireEvidenceFault(t, "conflicting forgotten replay", fixture.resource.RecordForgotten(t.Context(), forgetID))
	case "unreadable":
		recordForgotten(t, fixture, forgetID)
		fixture.corruptResourceRecord(t, "forgotten.json")
	case "absent":
		fixture.restart(t)
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
			fixture := newWorkspaceRecoveryFixture(t)
			prepareForgetRecovery(t, fixture, testCase.name)
			response, err := describeWorkspace(t.Context(), fixture.resource)
			assertForgetDescription(t, response, err, testCase.wantState, testCase.wantEvidence, testCase.wantError)
		})
	}
}
