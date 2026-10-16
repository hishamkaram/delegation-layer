package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestWorkspaceWatchDeadlineRemainsPending(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	err := watchManagedWorkspace(ctx, store, resource, Dependencies{}, Arguments{Watch: time.Second})
	if !errors.Is(err, workspace.ErrPending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("observer deadline became an operation failure: %v", err)
	}
	result := workspaceCommandResponse(ctx, store, resource, "", newResponse("workspace"), err)
	if result.code != 3 || result.response.Workspace == nil || result.response.Workspace.NextAction != "watch_workspace" {
		t.Fatalf("observer deadline did not retain the workspace: %+v", result)
	}
}

func TestWorkspaceAbortedPreparationDoesNotRemainProvisioning(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	subject := workspace.Subject{Kind: workspace.SubjectAdmission, ID: resource.Request().TaskID, SHA256: resource.Request().RequestSHA256}
	reservation, err := resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordAdmissionAborted(t.Context(), reservation); err != nil {
		t.Fatal(err)
	}
	description, err := describeWorkspace(t.Context(), resource)
	if err != nil || description.State != "preparation_failed" || description.NextAction != "stop_and_report" || description.Path != "" {
		t.Fatalf("closed no-launch preparation remained active: %+v, %v", description, err)
	}
	result := commandResult{response: newResponse("dispatch"), err: workspace.ErrPending, code: 3}
	result.response.TaskRecord = "not_created"
	setManagedAdmissionPendingFailure(store.Root, strings.Repeat("a", 32), description, &result)
	if result.code != 1 || result.response.Failure == nil || result.response.Failure.NextAction != "stop_and_report" {
		t.Fatalf("settled preparation failure told the caller to keep watching: %+v", result)
	}
}

func TestManagedTaskWorkspaceFailureReportsTypedCause(t *testing.T) {
	store, original, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, original)
	request, metadata, err := original.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	clonedRequest, clonedMetadata := *request, *metadata
	clonedRequest.TaskID = strings.Repeat("d", 32)
	clonedMetadata.TaskID = clonedRequest.TaskID
	clonedRequest.Workspace = &task.WorkspaceBinding{
		ID: strings.Repeat("e", 32), Reservation: 1,
		RequestSHA256: task.ComputeSHA256([]byte("missing workspace")), BudgetNanos: int64(time.Minute),
	}
	td, err := store.CreateTask(clonedRequest.TaskID, &clonedRequest, []byte("brief"), &clonedMetadata)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	result := commandResult{response: newResponse("status")}
	fillManagedTaskWorkspace(store, td, &result)
	if result.code != 0 || result.response.Workspace == nil || result.response.Workspace.ReasonCode != "workspace_wake_state_unavailable" || result.response.Workspace.NextAction != "stop_and_report" {
		t.Fatalf("workspace failure lost its cause or changed task authority: %+v", result)
	}
	if code := workspaceObservationReason("describe", errors.Join(context.DeadlineExceeded, task.ErrIdentityMismatch)); code != "workspace_describe_identity_mismatch" {
		t.Fatalf("deadline hid a concrete workspace identity fault: %s", code)
	}
}

func TestWorkspaceResponseViewDeadlineDoesNotFailPendingOperation(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	control, err := store.OpenWorkspace(t.Context(), resource.Request().WorkspaceID, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	held, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- control.WithLock(t.Context(), func(*taskdir.ControlTransaction) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	result := workspaceCommandResponse(t.Context(), store, resource, "", newResponse("workspace"), workspace.ErrPending)
	close(release)
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	if result.code != 3 || result.response.Workspace.State != "undetermined" || result.response.Workspace.NextAction != "watch_workspace" || result.response.Workspace.Capture != nil {
		t.Fatalf("view timeout fabricated an operation failure or readable state: %+v", result)
	}
}

func TestWorkspaceResponseDoesNotHideFaultsJoinedWithViewDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	for _, faults := range [][2]error{{workspace.ErrPending, errors.Join(context.DeadlineExceeded, task.ErrEvidenceFault)}, {task.ErrIdentityMismatch, context.DeadlineExceeded}} {
		response := newResponse("workspace")
		response.Workspace = &WorkspaceResponse{ID: "fixture-workspace"}
		result := finishWorkspaceCommandResponse(ctx, response, faults[0], faults[1])
		if result.code != 1 {
			t.Fatalf("view deadline hid a concrete fault: %+v", result)
		}
	}
}

func TestWorkspaceControlUncertaintyKeepsObservationPending(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	for _, cause := range []error{pueue.ErrInFlight, pueue.ErrSubmissionUncertain, pueue.ErrEnqueueUncertain} {
		err := workspaceControlError(cause)
		if !errors.Is(err, workspace.ErrPending) || !errors.Is(err, cause) {
			t.Fatalf("control uncertainty lost its cause: %v", err)
		}
		result := workspaceCommandResponse(t.Context(), store, resource, "", newResponse("workspace"), err)
		if result.code != 3 || result.response.Workspace.NextAction != "watch_workspace" {
			t.Fatalf("control uncertainty became a failed or successful operation: %+v", result)
		}
	}
	result := workspaceCommandResponse(t.Context(), store, resource, "", newResponse("workspace"), workspaceControlError(task.ErrEvidenceFault))
	if result.code != 1 {
		t.Fatalf("evidence fault was classified as pending: %+v", result)
	}
}

func TestWorkspaceStatusDoesNotWaitForActiveUseOwner(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	owner, err := resource.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	result := workspaceCommand(Arguments{Root: store.Root, WorkspaceAction: "status", WorkspaceID: resource.Request().WorkspaceID, Watch: time.Minute}, Dependencies{})
	if result.code != 3 || result.response.Workspace == nil || result.response.Workspace.NextAction != "watch_workspace" {
		t.Fatalf("active workspace owner became a status failure: %+v", result)
	}
}

func TestWorkspaceBusyMutationRequiresExactActionRetry(t *testing.T) {
	for _, action := range []string{"exec", "release", "forget"} {
		t.Run(action, func(t *testing.T) {
			store, resource := newAppWorkspaceResource(t)
			owner, err := resource.AcquireUseOwner(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if releaseErr := owner.Release(); releaseErr != nil {
					t.Error(releaseErr)
				}
			})
			result := workspaceCommand(Arguments{Root: store.Root, WorkspaceAction: action, WorkspaceID: resource.Request().WorkspaceID}, Dependencies{})
			response := result.response.Workspace
			if result.code != 3 || response == nil || response.ReasonCode != "requested_action_not_authorized" || response.NextAction != "retry_"+action || response.OperationID != "" || response.Operation != nil {
				t.Fatalf("busy mutation was acknowledged as accepted: %+v", result)
			}
		})
	}
}

func TestWorkspacePendingActionDoesNotHideJoinedFault(t *testing.T) {
	for _, pending := range []error{workspace.ErrBusy, workspace.ErrPending} {
		response := newResponse("workspace")
		response.Workspace = &WorkspaceResponse{ID: "fixture-workspace"}
		result := finishWorkspaceCommandResponse(t.Context(), response, errors.Join(pending, task.ErrEvidenceFault), nil)
		if result.code != 1 || !errors.Is(result.err, task.ErrEvidenceFault) {
			t.Fatalf("pending classification hid an evidence fault: %+v", result)
		}
	}
}

func TestWorkspaceAcknowledgementSurvivesViewTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	for _, authorized := range []bool{false, true} {
		response := newResponse("workspace")
		response.Workspace = &WorkspaceResponse{ID: "fixture-workspace", NextAction: "retry_release"}
		ack := workspaceActionAcknowledgement{authorized: authorized}
		result := finishWorkspaceActionResponse(ctx, response, "release", ack, workspace.ErrPending, context.DeadlineExceeded)
		wantNext, wantReason := "retry_release", "requested_action_not_authorized"
		if authorized {
			wantNext, wantReason = "watch_workspace", ""
		}
		if result.code != 3 || result.response.Workspace.State != "undetermined" || result.response.Workspace.NextAction != wantNext || result.response.Workspace.ReasonCode != wantReason {
			t.Fatalf("view timeout changed established authorization=%v: %+v", authorized, result)
		}
	}
}

func TestWorkspaceReleaseAcknowledgementRequiresLinkedReservation(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	subject, err := resource.RecordReleaseAction(t.Context(), workspace.ReleaseAction{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, ID: strings.Repeat("a", 32), BudgetNanos: int64(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorized, readErr := workspaceReleaseAcknowledged(t.Context(), resource, false); readErr != nil || authorized {
		t.Fatalf("unlinked release action was acknowledged: authorized=%v error=%v", authorized, readErr)
	}
	if _, err = resource.Reserve(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	result := workspaceCommandActionResponse(t.Context(), store, resource, "release", false, workspaceActionAcknowledgement{}, newResponse("workspace"), workspace.ErrPending)
	if result.code != 3 || result.response.Workspace.NextAction != "watch_workspace" || result.response.Workspace.OperationID != "" {
		t.Fatalf("helperless accepted release was not acknowledged: %+v", result)
	}
}

func TestWorkspaceReleaseAcknowledgementIncludesClosedCaptureGap(t *testing.T) {
	_, resource := newWorkspaceFinalizeFixture(t)
	seedCanonicalReleaseCapture(t, resource)
	if _, err := resource.Release(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("fixture already published release: %v", err)
	}
	if authorized, err := workspaceReleaseAcknowledged(t.Context(), resource, false); err != nil || !authorized {
		t.Fatalf("successful closed capture lost release authorization: authorized=%v error=%v", authorized, err)
	}
}

func TestWorkspaceReleaseAcknowledgementDoesNotReviveFailedCapture(t *testing.T) {
	for _, discard := range []bool{false, true} {
		_, resource := newAppWorkspaceResource(t)
		subject, err := resource.RecordReleaseAction(t.Context(), workspace.ReleaseAction{
			SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, ID: strings.Repeat("b", 32), BudgetNanos: int64(time.Minute), Discard: discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		reservation, err := resource.Reserve(t.Context(), subject)
		if err != nil {
			t.Fatal(err)
		}
		captureID := strings.Repeat("c", 32)
		if err = resource.RecordCaptureReceipt(t.Context(), workspace.CaptureReceipt{
			SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: captureID, Subject: subject,
			Status: "unavailable", TerminationSHA256: task.ComputeSHA256([]byte("termination")), WorkerWaitSHA256: task.ComputeSHA256([]byte("wait")),
		}); err != nil {
			t.Fatal(err)
		}
		if err = resource.CloseReservation(t.Context(), reservation, "unavailable", captureID); err != nil {
			t.Fatal(err)
		}
		if authorized, err := workspaceReleaseAcknowledged(t.Context(), resource, discard); err != nil || authorized != discard {
			t.Fatalf("failed capture authorization=%v discard=%v error=%v", authorized, discard, err)
		}
	}
}

func TestWorkspaceAcknowledgementReadFaultDoesNotClaimNotAuthorized(t *testing.T) {
	fixture := newWorkspaceRecoveryFixture(t)
	prepareForgetRecovery(t, fixture, "absent")
	fixture.corruptResourceRecord(t, "forget-intent.json")
	result := workspaceCommandActionResponse(t.Context(), fixture.store, fixture.resource, "forget", false, workspaceActionAcknowledgement{}, newResponse("workspace"), workspace.ErrPending)
	if result.code != 1 || result.response.Workspace.ReasonCode == "requested_action_not_authorized" || result.response.Workspace.NextAction == "retry_forget" {
		t.Fatalf("unreadable authorization became a negative acknowledgement: %+v", result)
	}
}

func TestWorkspaceReleaseAcknowledgementPreservesRequestedDiscard(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	subject, err := resource.RecordReleaseAction(t.Context(), workspace.ReleaseAction{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID,
		ID: strings.Repeat("a", 32), BudgetNanos: int64(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resource.Reserve(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	result := workspaceCommandActionResponse(t.Context(), store, resource, "release", true, workspaceActionAcknowledgement{}, newResponse("workspace"), workspace.ErrBusy)
	if result.code != 3 || result.response.Workspace.NextAction != "retry_release" || result.response.Workspace.ReasonCode != "requested_action_not_authorized" {
		t.Fatalf("a prior nondiscard release acknowledged a new discard request: %+v", result)
	}
	if authorized, readErr := workspaceReleaseAcknowledged(t.Context(), resource, false); readErr != nil || !authorized {
		t.Fatalf("mismatching request changed original release authority: %v %v", authorized, readErr)
	}
}

func TestWorkspaceKnownAcknowledgementDoesNotRequireAnotherAuthorizationRead(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	control, err := store.OpenWorkspace(t.Context(), resource.Request().WorkspaceID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	defer func() {
		close(release)
		select {
		case lockErr := <-done:
			if lockErr != nil {
				t.Error(lockErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("workspace lock fixture did not finish")
		}
	}()
	go func() {
		done <- control.WithLock(t.Context(), func(*taskdir.ControlTransaction) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("workspace lock fixture did not enter")
	}
	result := workspaceCommandActionResponse(t.Context(), store, resource, "forget", false, workspaceActionAcknowledgement{authorized: true}, newResponse("workspace"), workspace.ErrPending)
	if result.code != 3 || result.response.Workspace.NextAction != "watch_workspace" || result.response.Workspace.State != "undetermined" {
		t.Fatalf("known durable authorization was lost at a later view timeout: %+v", result)
	}
}
