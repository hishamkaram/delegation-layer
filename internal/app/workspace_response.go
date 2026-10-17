package app

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func fillManagedTaskWorkspace(store *taskdir.Store, td *taskdir.TaskDir, result *commandResult) {
	request, _, err := td.PreparedRecords()
	if err != nil || request.Workspace == nil {
		return
	}
	response := WorkspaceResponse{ID: request.Workspace.ID, Isolation: "git-worktree", State: "unreadable", ReasonCode: "workspace_observation_failed", NextAction: "stop_and_report", EvidenceState: "unknown"}
	result.response.Workspace = &response
	wakeContext, finishWake := context.WithTimeout(context.Background(), time.Second)
	wakeErr := wakeManagedTaskWorkspace(wakeContext, store, td)
	finishWake()
	if wakeErr != nil && !workspaces.PendingError(wakeErr) && !workspaceViewRetryable(wakeErr) {
		result.response.Workspace.ReasonCode = workspaceObservationReason("wake", wakeErr)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resource, err := workspace.LoadResource(ctx, store, request.Workspace.ID)
	if err != nil {
		result.response.Workspace.ReasonCode = workspaceObservationReason("load", err)
		return
	}
	description, describeErr := workspaces.Describe(ctx, resource)
	closeErr := resource.Close()
	if err = errors.Join(describeErr, closeErr); err != nil {
		result.response.Workspace.ReasonCode = workspaceObservationReason("describe", err)
		return
	}
	result.response.Workspace = &description
	continuation := result.response.Continuation
	if continuation != nil && continuation.Resumable && description.State != "retained" {
		continuation.Resumable, continuation.ContinueCommand = false, ""
		continuation.Reason = "managed workspace is " + description.State
	}
}

// Expose the concrete observation boundary and typed cause without copying
// filesystem paths, record contents, credentials or arbitrary error text.
func workspaceObservationReason(stage string, err error) string {
	cause := "failed"
	switch {
	case errors.Is(err, task.ErrIdentityMismatch), errors.Is(err, workspace.ErrIdentity):
		cause = "identity_mismatch"
	case errors.Is(err, task.ErrEvidenceFault):
		cause = "evidence_fault"
	case errors.Is(err, pueue.ErrBinding):
		cause = "supervisor_binding_changed"
	case errors.Is(err, pueue.ErrConfiguration):
		cause = "supervisor_configuration_invalid"
	case errors.Is(err, os.ErrPermission):
		cause = "permission_denied"
	case errors.Is(err, os.ErrNotExist):
		cause = "state_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		cause = "timed_out"
	}
	return "workspace_" + stage + "_" + cause
}

func setManagedAdmissionPendingFailure(root, operationID string, description WorkspaceResponse, result *commandResult) {
	if workspaces.PendingError(result.err) && result.response.TaskRecord == "not_created" {
		if description.State == "preparation_failed" {
			result.response.Failure = &DispatchFailureResponse{Code: "workspace_preparation_failed", Stage: "prepare-workspace", NextAction: "stop_and_report"}
			result.code = 1
			return
		}
		code := "workspace_busy"
		if operationID != "" && description.Path == "" {
			code = "workspace_provision_pending"
		}
		result.response.Failure = &DispatchFailureResponse{Code: code, Stage: "prepare-workspace", NextAction: "watch_workspace", RecoveryCommand: workspaceStatusCommand(root, description.ID)}
		result.code = 3
	}
}

func workspaceStatusCommand(root, id string) string {
	return "delegate --root " + shellQuote(root) + " workspace status " + id + " --watch 30s --json"
}
