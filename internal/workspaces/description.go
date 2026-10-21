package workspaces

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// Describe reports the resource's current availability and the latest use's
// evidence. Workspace commands use this resource-scoped view.
func Describe(ctx context.Context, resource *workspace.Resource) (Description, error) {
	return describeResource(ctx, resource, func(current *Description) error { return DescribeLatestUse(ctx, resource, current) })
}

// DescribeTask reports the resource's current availability with evidence
// from the task's own bound reservation only. A successor's capture or helper
// is never attributed to this task, and a sealed capture of an earlier use
// never makes a busy, released or forgotten resource look usable.
func DescribeTask(ctx context.Context, resource *workspace.Resource, request task.TaskRecord) (Description, error) {
	use, err := taskUse(ctx, resource, request)
	if err != nil {
		return Description{ID: resource.Request().WorkspaceID, Isolation: "git-worktree", State: "unreadable", EvidenceState: "unknown"}, err
	}
	return describeResource(ctx, resource, func(current *Description) error { return describeUseEvidence(ctx, resource, use, current) })
}

func describeResource(ctx context.Context, resource *workspace.Resource, evidence func(*Description) error) (Description, error) {
	response := Description{ID: resource.Request().WorkspaceID, Isolation: "git-worktree", State: "provisioning", EvidenceState: "retained"}
	observers := []func(*Description) error{
		func(current *Description) error { return describeRegistration(ctx, resource, current) },
		func(current *Description) error { return describeRelease(ctx, resource, current) },
		func(current *Description) error { return describeForget(ctx, resource, current) },
		func(current *Description) error { return describeAvailability(ctx, resource, current) },
		evidence,
	}
	for _, observe := range observers {
		if err := observe(&response); err != nil {
			return response, err
		}
	}
	if response.Capture != nil {
		appendArtifactDescriptions(resource.StateRoot(), response.Capture.OperationID, response.Capture.Artifacts, &response)
	}
	return response, nil
}

// appendArtifactDescriptions is the one flattened artifact projection. A
// descriptor equal in every field to one already projected by an earlier
// source, such as a capture operation described both by its receipt and as
// the requested operation, is shown once. Distinct descriptors, including
// repeats within this one source, keep their first-seen order.
func appendArtifactDescriptions(root, operationID string, artifacts []workspace.Artifact, response *Description) {
	projected := len(response.Artifacts)
	for _, artifact := range artifacts {
		descriptor := ArtifactDescription{Artifact: artifact, OperationID: operationID, EvidenceState: response.EvidenceState}
		if artifact.Omitted != "" {
			descriptor.EvidenceState = "omitted"
		} else if response.EvidenceState == "retained" {
			descriptor.Path = filepath.Join(workspace.ScratchPath(root, operationID), artifact.Name)
		}
		if slices.Contains(response.Artifacts[:projected], descriptor) {
			continue
		}
		response.Artifacts = append(response.Artifacts, descriptor)
	}
}

func describeRegistration(ctx context.Context, resource *workspace.Resource, response *Description) error {
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

func describeRelease(ctx context.Context, resource *workspace.Resource, response *Description) error {
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

func describeForget(ctx context.Context, resource *workspace.Resource, response *Description) error {
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

// describeAvailability reflects only the current resource use, which may be
// a successor of the described evidence.
func describeAvailability(ctx context.Context, resource *workspace.Resource, response *Description) error {
	if _, err := resource.Reservation(ctx); workspace.OperationAbsent(err) {
		return nil
	} else if err != nil {
		return err
	}
	response.State, response.NextAction = "busy", "watch_workspace"
	return nil
}

// DescribeLatestUse selects the latest use's evidence, open or closed.
func DescribeLatestUse(ctx context.Context, resource *workspace.Resource, response *Description) error {
	latest, err := resource.LatestReservation(ctx)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return describeUseEvidence(ctx, resource, use{reservation: latest}, response)
}

// describeUseEvidence is the single interpretation of one use's evidence: an
// open use exposes its pending capture helper; a closed use exposes its
// receipt or its no-launch preparation outcome.
func describeUseEvidence(ctx context.Context, resource *workspace.Resource, selected use, response *Description) error {
	reservation := selected.reservation
	closed, err := resource.ReservationClosed(ctx, reservation.Sequence)
	if workspace.OperationAbsent(err) {
		return describeOpenUse(ctx, resource, selected, response)
	}
	if err != nil {
		return err
	}
	if closed.CaptureID == "" {
		if closed.Disposition == "aborted-not-launched" && reservation.Subject.Kind == workspace.SubjectAdmission && response.State == "provisioning" {
			response.State, response.ReasonCode, response.NextAction = "preparation_failed", "admission_not_launched", "stop_and_report"
		}
		return nil
	}
	receipt, err := resource.CaptureReceipt(ctx, closed.CaptureID)
	if err != nil {
		return err
	}
	if selected.subject != nil && receipt.Subject != *selected.subject {
		return task.ErrIdentityMismatch
	}
	response.Capture = &receipt
	return nil
}

func describeOpenUse(ctx context.Context, resource *workspace.Resource, selected use, response *Description) error {
	obligation, err := resource.CaptureObligation(ctx, selected.reservation.Sequence)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if selected.subject != nil && obligation.Subject != *selected.subject {
		return task.ErrIdentityMismatch
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

func DescribeOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, operationID string, response *Description) (resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, operationID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Request().WorkspaceID != resource.Request().WorkspaceID {
		return task.ErrIdentityMismatch
	}
	if operation.Request().Kind == workspace.OperationCheckpoint {
		return describeCheckpointOperation(ctx, store, operation, response)
	}
	result, _, err := AcceptedOperation(ctx, store, operation)
	if errors.Is(err, ErrOperationResultUnavailable) {
		response.ReasonCode = "operation_result_unavailable"
	}
	if result.OperationID != "" {
		response.Operation = &result
		response.ReasonCode = result.ReasonCode
	}
	if errors.Is(err, workspace.ErrPending) && PendingError(err) {
		response.NextAction = "watch_workspace"
		return nil
	}
	if err != nil {
		return err
	}
	appendArtifactDescriptions(store.Root, operationID, result.Artifacts, response)
	return nil
}

// describeCheckpointOperation never projects a generic helper result as a
// checkpoint success; only the qualified receipt describes its outcome.
func describeCheckpointOperation(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, response *Description) error {
	receipt, err := QualifyCheckpoint(ctx, store, operation)
	if errors.Is(err, workspace.ErrPending) && PendingError(err) {
		response.NextAction = "watch_workspace"
		return nil
	}
	if err != nil {
		return err
	}
	response.ReasonCode = receipt.ReasonCode
	return nil
}
