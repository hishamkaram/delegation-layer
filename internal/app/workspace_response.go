package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// WorkspaceResponse is independent from provider outcome and publication.
// It reports resource availability and Git-content evidence only.
type WorkspaceResponse struct {
	ID                    string                      `json:"id"`
	Isolation             string                      `json:"isolation"`
	State                 string                      `json:"state"`
	Path                  string                      `json:"path,omitempty"`
	CanonicalCwd          string                      `json:"canonical_cwd,omitempty"`
	SourceRoot            string                      `json:"source_root,omitempty"`
	SourceChangesExcluded bool                        `json:"source_changes_excluded"`
	BaseOID               string                      `json:"base_oid,omitempty"`
	BaseRef               string                      `json:"base_ref,omitempty"`
	OperationID           string                      `json:"operation_id,omitempty"`
	Operation             *workspace.OperationResult  `json:"operation,omitempty"`
	Capture               *workspace.CaptureReceipt   `json:"capture,omitempty"`
	NextAction            string                      `json:"next_action,omitempty"`
	ReasonCode            string                      `json:"reason_code,omitempty"`
	EvidenceState         string                      `json:"evidence_state"`
	Artifacts             []WorkspaceArtifactResponse `json:"artifacts,omitempty"`
}

// WorkspaceArtifactResponse derives a usable location from immutable evidence.
// Retained describes inventory; consumers still verify length and SHA-256 when
// reading bytes. Forgetting never rewrites the underlying capture receipt.
type WorkspaceArtifactResponse struct {
	workspace.Artifact
	OperationID   string `json:"operation_id"`
	Path          string `json:"path,omitempty"`
	EvidenceState string `json:"evidence_state"`
}

func describeWorkspace(ctx context.Context, resource *workspace.Resource) (WorkspaceResponse, error) {
	response := WorkspaceResponse{ID: resource.Request().WorkspaceID, Isolation: "git-worktree", State: "provisioning", EvidenceState: "retained"}
	observers := []func(*WorkspaceResponse) error{
		func(current *WorkspaceResponse) error { return describeWorkspaceRegistration(ctx, resource, current) },
		func(current *WorkspaceResponse) error { return describeWorkspaceRelease(ctx, resource, current) },
		func(current *WorkspaceResponse) error { return describeWorkspaceForget(ctx, resource, current) },
		func(current *WorkspaceResponse) error { return describeWorkspaceReservation(ctx, resource, current) },
	}
	for _, observe := range observers {
		if err := observe(&response); err != nil {
			return response, err
		}
	}
	if response.Capture != nil {
		appendWorkspaceArtifactResponses(resource.StateRoot(), response.Capture.OperationID, response.Capture.Artifacts, &response)
	}
	return response, nil
}

func appendWorkspaceArtifactResponses(root, operationID string, artifacts []workspace.Artifact, response *WorkspaceResponse) {
	for _, artifact := range artifacts {
		descriptor := WorkspaceArtifactResponse{Artifact: artifact, OperationID: operationID, EvidenceState: response.EvidenceState}
		if artifact.Omitted != "" {
			descriptor.EvidenceState = "omitted"
		} else if response.EvidenceState == "retained" {
			descriptor.Path = filepath.Join(workspace.ScratchPath(root, operationID), artifact.Name)
		}
		response.Artifacts = append(response.Artifacts, descriptor)
	}
}

func describeWorkspaceRegistration(ctx context.Context, resource *workspace.Resource, response *WorkspaceResponse) error {
	registration, err := resource.Registration(ctx)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	response.State, response.Path, response.CanonicalCwd = "retained", registration.CheckoutPath, registration.CanonicalCwd
	response.SourceRoot, response.SourceChangesExcluded, response.BaseOID, response.BaseRef = registration.SourceRoot, registration.SourceChangesExcluded, registration.BaseOID, registration.BaseRef
	return nil
}

func describeWorkspaceRelease(ctx context.Context, resource *workspace.Resource, response *WorkspaceResponse) error {
	if _, err := resource.Release(ctx); workspace.OperationAbsent(err) {
		return nil
	} else if err != nil {
		return err
	}
	response.State, response.NextAction = "cleanup_pending", "retry_release"
	if _, err := resource.Removed(ctx); err == nil {
		response.State, response.Path, response.CanonicalCwd, response.NextAction = "released", "", "", ""
		return nil
	} else if !workspace.OperationAbsent(err) {
		return err
	}
	return nil
}

func describeWorkspaceForget(ctx context.Context, resource *workspace.Resource, response *WorkspaceResponse) error {
	if _, err := resource.ForgetIntent(ctx); workspace.OperationAbsent(err) {
		return nil
	} else if err != nil {
		return err
	}
	response.State, response.EvidenceState = "forgetting", "forgetting"
	if _, err := resource.Forgotten(ctx); err == nil {
		response.State, response.EvidenceState, response.NextAction = "forgotten", "forgotten", ""
		return nil
	} else if !workspace.OperationAbsent(err) {
		return err
	}
	return nil
}

func describeWorkspaceReservation(ctx context.Context, resource *workspace.Resource, response *WorkspaceResponse) error {
	reservation, err := resource.Reservation(ctx)
	if err == nil {
		return describeActiveWorkspaceReservation(ctx, resource, reservation, response)
	}
	if !workspace.OperationAbsent(err) {
		return err
	}
	return describeClosedWorkspaceReservation(ctx, resource, response)
}

func describeActiveWorkspaceReservation(ctx context.Context, resource *workspace.Resource, reservation workspace.Reservation, response *WorkspaceResponse) error {
	response.State, response.NextAction = "busy", "watch_workspace"
	obligation, err := resource.CaptureObligation(ctx, reservation.Sequence)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	reference, err := resource.AssociatedOperation(ctx, workspace.CapturePurpose(obligation.Sequence))
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	response.OperationID = reference.OperationID
	return nil
}

func describeClosedWorkspaceReservation(ctx context.Context, resource *workspace.Resource, response *WorkspaceResponse) error {
	latest, err := resource.LatestReservation(ctx)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	closed, err := resource.ReservationClosed(ctx, latest.Sequence)
	if err != nil {
		return err
	}
	if closed.CaptureID == "" {
		if closed.Disposition == "aborted-not-launched" && latest.Subject.Kind == workspace.SubjectAdmission && response.State == "provisioning" {
			response.State, response.ReasonCode, response.NextAction = "preparation_failed", "admission_not_launched", "stop_and_report"
		}
		return nil
	}
	receipt, err := resource.CaptureReceipt(ctx, closed.CaptureID)
	if err != nil {
		return err
	}
	response.Capture = &receipt
	return nil
}

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
	if wakeErr != nil && !workspacePendingError(wakeErr) && !workspaceViewRetryable(wakeErr) {
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
	description, describeErr := describeWorkspace(ctx, resource)
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

func recordManagedTaskCapture(store *taskdir.Store, request task.TaskRecord) (resultErr error) {
	if request.Workspace == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	resource, err := workspace.LoadResource(ctx, store, request.Workspace.ID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	data, err := task.MarshalCanonical(request)
	if err != nil {
		return err
	}
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: request.TaskID, SHA256: task.ComputeSHA256(data)}
	previous, previousErr := resource.CaptureObligation(ctx, request.Workspace.Reservation)
	if previousErr == nil {
		if previous.Subject != subject {
			return task.ErrIdentityMismatch
		}
		return nil
	}
	if !workspace.OperationAbsent(previousErr) {
		return previousErr
	}
	reservation, err := resource.Reservation(ctx)
	if err != nil {
		return err
	}
	if reservation.Sequence != request.Workspace.Reservation || reservation.Subject.Kind != workspace.SubjectAdmission || reservation.Subject.ID != request.TaskID || reservation.Subject.SHA256 != request.Workspace.RequestSHA256 {
		return task.ErrIdentityMismatch
	}
	return resource.RecordCaptureObligationWithBudget(ctx, reservation, subject, request.Workspace.BudgetNanos)
}

func finishManagedAdmission(store *taskdir.Store, managed *managedWorkspaceAdmission, result *commandResult) {
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	if result.err != nil && !managed.existing && managed.reservation.Sequence != 0 && result.response.TaskRecord != "created" {
		abortErr := reconcileUnlaunchedWorkspaceAdmission(ctx, store, managed.resource, managed.reservation)
		if abortErr != nil && !errors.Is(abortErr, workspace.ErrPending) {
			mergeCommandClose(result, func() error { return abortErr })
		}
	}
	description, err := describeWorkspace(ctx, managed.resource)
	if managed.operationID != "" {
		description.OperationID = managed.operationID
		operationErr := describeWorkspaceOperation(ctx, store, managed.resource, managed.operationID, &description)
		if operationErr != nil && !errors.Is(operationErr, workspace.ErrPending) && result.err == nil {
			err = errors.Join(err, operationErr)
		}
	}
	result.response.Workspace = &description
	if err != nil {
		mergeCommandClose(result, func() error { return err })
	}
	setManagedAdmissionPendingFailure(store.Root, managed.operationID, description, result)
	mergeCommandClose(result, managed.Close)
}

func setManagedAdmissionPendingFailure(root, operationID string, description WorkspaceResponse, result *commandResult) {
	if workspacePendingError(result.err) && result.response.TaskRecord == "not_created" {
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
