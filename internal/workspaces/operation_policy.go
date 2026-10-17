package workspaces

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func PendingError(err error) bool {
	if !errors.Is(err, workspace.ErrPending) && !errors.Is(err, workspace.ErrBusy) {
		return false
	}
	return task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, workspace.ErrPending) || errors.Is(cause, workspace.ErrBusy) ||
			errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, task.ErrLockBusy) ||
			errors.Is(cause, pueue.ErrInFlight) || errors.Is(cause, pueue.ErrSubmissionUncertain) || errors.Is(cause, pueue.ErrEnqueueUncertain)
	})
}

func ForgetPurpose(intent workspace.ForgetIntent) string {
	return "forget-" + intent.OperationID + ".json"
}

func ForgetSubject(intent workspace.ForgetIntent) (workspace.Subject, error) {
	data, err := task.MarshalCanonical(intent)
	if err != nil {
		return workspace.Subject{}, err
	}
	return workspace.Subject{Kind: workspace.SubjectOperation, ID: intent.OperationID, SHA256: task.ComputeSHA256(data)}, nil
}

func RemovalSubject(release workspace.ReleaseRecord) (workspace.Subject, error) {
	data, err := task.MarshalCanonical(release)
	if err != nil {
		return workspace.Subject{}, err
	}
	return workspace.Subject{Kind: workspace.SubjectOperation, ID: release.OperationID, SHA256: task.ComputeSHA256(data)}, nil
}

func resolvedWorkspaceCaptureResult(err error) bool {
	if err == nil {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !resolvedWorkspaceCaptureResult(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		return cause != nil && resolvedWorkspaceCaptureResult(cause)
	}
	var failure *OperationError
	return errors.As(err, &failure) || errors.Is(err, ErrOperationResultUnavailable) || errors.Is(err, task.ErrLifetimeUnlaunched)
}

func sortWorkspaceForgetInventory(intent *workspace.ForgetIntent) {
	slices.SortFunc(intent.Refs, func(first, second workspace.ForgetRef) int { return strings.Compare(first.Ref, second.Ref) })
	slices.SortFunc(intent.Artifacts, func(first, second workspace.ForgetArtifact) int {
		return strings.Compare(first.OperationID+"/"+first.Name, second.OperationID+"/"+second.Name)
	})
}

func ManagedBaseRef(rootID, workspaceID string) string {
	return "refs/delegate/" + rootID + "/workspaces/" + workspaceID + "/base"
}

func validateWorkspaceOperationSpec(request workspace.OperationRequest, spec OperationSpec) error {
	if request.Kind != spec.Kind || request.Subject != spec.Subject || request.Discard != spec.Discard || !slices.Equal(request.Arguments, spec.Arguments) {
		return task.ErrIdentityMismatch
	}
	if spec.Deadline != "" && request.Deadline != spec.Deadline {
		return task.ErrIdentityMismatch
	}
	return nil
}

// A guardian can close its native scope between observation and the scheduling
// gate. Its launch refusal is pending until the next canonical closure read;
// it must neither fail a completed helper nor authorize another enqueue.
func LaunchPermissionError(err error) error {
	if errors.Is(err, task.ErrTerminalTask) || errors.Is(err, task.ErrLifetimeUnlaunched) {
		return workspace.ErrPending
	}
	return err
}

func ControlError(err error) error {
	if errors.Is(err, pueue.ErrInFlight) || errors.Is(err, pueue.ErrSubmissionUncertain) || errors.Is(err, pueue.ErrEnqueueUncertain) {
		return errors.Join(workspace.ErrPending, err)
	}
	return err
}

func (e *OperationError) Error() string {
	if e.reason == "" {
		return "workspace operation failed"
	}
	return "workspace operation failed: " + e.reason
}

func isWorkspaceArtifactMissing(err error) bool {
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return false
	}
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		return false
	}
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == syscall.ENOENT
}

func validateWorkspaceOmittedArtifact(artifact workspace.Artifact) error {
	return workspace.ValidateArtifact(artifact)
}
