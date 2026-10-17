package workspaces

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func Describe(ctx context.Context, resource *workspace.Resource) (Description, error) {
	response := Description{ID: resource.Request().WorkspaceID, Isolation: "git-worktree", State: "provisioning", EvidenceState: "retained"}
	observers := []func(*Description) error{
		func(current *Description) error { return describeRegistration(ctx, resource, current) },
		func(current *Description) error { return describeRelease(ctx, resource, current) },
		func(current *Description) error { return describeForget(ctx, resource, current) },
		func(current *Description) error { return describeReservation(ctx, resource, current) },
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

func appendArtifactDescriptions(root, operationID string, artifacts []workspace.Artifact, response *Description) {
	for _, artifact := range artifacts {
		descriptor := ArtifactDescription{Artifact: artifact, OperationID: operationID, EvidenceState: response.EvidenceState}
		if artifact.Omitted != "" {
			descriptor.EvidenceState = "omitted"
		} else if response.EvidenceState == "retained" {
			descriptor.Path = filepath.Join(workspace.ScratchPath(root, operationID), artifact.Name)
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

func describeReservation(ctx context.Context, resource *workspace.Resource, response *Description) error {
	reservation, err := resource.Reservation(ctx)
	if err == nil {
		return describeActiveReservation(ctx, resource, reservation, response)
	}
	if !workspace.OperationAbsent(err) {
		return err
	}
	return DescribeClosedReservation(ctx, resource, response)
}

func describeActiveReservation(ctx context.Context, resource *workspace.Resource, reservation workspace.Reservation, response *Description) error {
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

func DescribeClosedReservation(ctx context.Context, resource *workspace.Resource, response *Description) error {
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

func DescribeOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, operationID string, response *Description) (resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, operationID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Request().WorkspaceID != resource.Request().WorkspaceID {
		return task.ErrIdentityMismatch
	}
	result, _, err := AcceptedOperation(ctx, store, operation)
	if errors.Is(err, ErrOperationResultUnavailable) {
		response.ReasonCode = "operation_result_unavailable"
	}
	if result.OperationID != "" {
		response.Operation = &result
		response.ReasonCode = result.ReasonCode
	}
	if errors.Is(err, workspace.ErrPending) {
		response.NextAction = "watch_workspace"
		return nil
	}
	if err != nil {
		return err
	}
	appendArtifactDescriptions(store.Root, operationID, result.Artifacts, response)
	return nil
}
