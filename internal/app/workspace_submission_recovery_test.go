package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	"github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestWorkspaceCleanupResumesExistingStagedHelper(t *testing.T) {
	for _, kind := range []string{workspace.OperationRemove, workspace.OperationForget} {
		t.Run(kind, func(t *testing.T) { verifyWorkspaceCleanupSubmissionRecovery(t, kind) })
	}
}

func verifyWorkspaceCleanupSubmissionRecovery(t *testing.T, kind string) {
	t.Helper()
	fixture := testsupport.NewRecoveryFixture(t)
	supervisor, _ := newTerminalEventInspectionSupervisor(t, fixture.Store)
	var adds, enqueues atomic.Int32
	options := pueue.Options{Environment: os.Environ(), Observer: workspaceRecoveryCommandObserver(&adds, &enqueues)}
	statusPath := filepath.Join(filepath.Dir(supervisor.Binding().ConfigPath), "terminal-event-status.json")
	request := workspaceCleanupRecoveryRequest(t, fixture, kind)
	request.Supervisor = supervisor.Binding()
	operation := testsupport.OpenSelectionOperation(t, fixture, request)
	if kind == workspace.OperationForget {
		intent, err := fixture.Resource.ForgetIntent(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.Resource.AssociateOperation(t.Context(), workspaces.ForgetPurpose(intent), request); err != nil {
			t.Fatal(err)
		}
	}
	prepareWorkspaceStagedRecovery(t, operation)
	for _, stashed := range []bool{true, false} {
		writeWorkspaceRecoveryQueue(t, statusPath, operation, stashed)
		err := observeWorkspaceCleanupRecovery(t, fixture, kind, Dependencies{InitialSupervisorExecutable: supervisor.Binding().ClientExecutable, SupervisorOptions: options})
		if !workspaces.PendingError(err) {
			t.Fatalf("existing helper recovery failed: %v", err)
		}
	}
	if adds.Load() != 0 || enqueues.Load() != 1 {
		t.Fatalf("recovery added %d new jobs and enqueued %d times", adds.Load(), enqueues.Load())
	}
	ids, err := fixture.Resource.OperationIDs(t.Context())
	if err != nil || len(ids) != 1 || ids[0] != request.OperationID || operation.Request().Deadline != request.Deadline {
		t.Fatalf("recovery replaced the authorized helper: ids=%v error=%v", ids, err)
	}
}

func workspaceRecoveryCommandObserver(adds, enqueues *atomic.Int32) func(pueue.CommandEvent) {
	return func(event pueue.CommandEvent) {
		if event.Stage != "entry" || slices.Contains(event.Argv, "--help") {
			return
		}
		if slices.Contains(event.Argv, "add") {
			adds.Add(1)
		}
		if slices.Contains(event.Argv, "enqueue") {
			enqueues.Add(1)
		}
	}
}

func workspaceCleanupRecoveryRequest(t *testing.T, fixture *testsupport.RecoveryFixture, kind string) workspace.OperationRequest {
	t.Helper()
	deadline := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	var subject workspace.Subject
	var err error
	if kind == workspace.OperationRemove {
		release := workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID, OperationID: strings.Repeat("a", 32), Discard: true}
		if err = fixture.Resource.RecordRelease(t.Context(), release); err != nil {
			t.Fatal(err)
		}
		subject, err = workspaces.RemovalSubject(release)
	} else {
		intent := workspace.ForgetIntent{SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID, OperationID: strings.Repeat("a", 32), BudgetNanos: int64(time.Minute), Deadline: deadline}
		if err = fixture.Resource.RecordForgetIntent(t.Context(), intent); err != nil {
			t.Fatal(err)
		}
		subject, err = workspaces.ForgetSubject(intent)
	}
	if err != nil {
		t.Fatal(err)
	}
	request := testsupport.RecoveryOperationRequest(t, fixture, strings.Repeat("b", 32), kind, subject)
	request.Deadline = deadline
	request.Discard = kind == workspace.OperationRemove
	return request
}

func prepareWorkspaceStagedRecovery(t *testing.T, operation *workspace.Operation) {
	t.Helper()
	permit, err := operation.SubmissionPermit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
}

func writeWorkspaceRecoveryQueue(t *testing.T, path string, operation *workspace.Operation, stashed bool) {
	t.Helper()
	request := operation.Request()
	if stashed {
		writeRunnerStashedStatus(t, path, request.RootID, request.OperationID, 7)
	} else {
		writeRunnerStatus(t, path, request.RootID, request.OperationID, 7)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	identity := pueue.InspectionIdentity{RootID: request.RootID, TaskID: request.OperationID, QueueGroup: "default"}
	ordinaryLabel := "delegate:" + request.RootID + ":" + request.OperationID
	status := strings.ReplaceAll(string(data), ordinaryLabel, identity.Label())
	status = strings.Replace(status, `"groups":{}`, `"groups":{"default":{"status":"Running","parallel_tasks":0}}`, 1)
	writeAppTestFile(t, path, []byte(status))
}

func observeWorkspaceCleanupRecovery(t *testing.T, fixture *testsupport.RecoveryFixture, kind string, deps Dependencies) error {
	t.Helper()
	if kind == workspace.OperationRemove {
		return reconcileWorkspaceRemoval(t.Context(), fixture.Store, fixture.Resource, deps, false)
	}
	_, handled, err := reconcileWorkspaceForgetOperation(t.Context(), fixture.Store, fixture.Resource, deps, false, 0)
	if !handled {
		return errors.New("forget recovery ignored its existing intent")
	}
	return err
}
