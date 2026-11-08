package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func WriteFailureDiagnostic(store *taskdir.Store, operation *workspace.Operation, cause error) (workspace.Artifact, error) {
	directory := workspace.ScratchPath(store.Root, operation.Request().OperationID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return workspace.Artifact{}, err
	}
	writer, err := newWorkspaceDiagnostic(directory, workspace.FailureDiagnosticName)
	if err != nil {
		return workspace.Artifact{}, err
	}
	_, writeErr := fmt.Fprintln(writer, cause)
	if err = errors.Join(writeErr, writer.Close()); err != nil {
		return workspace.Artifact{}, errors.Join(err, os.Remove(filepath.Join(directory, workspace.FailureDiagnosticName)))
	}
	return writer.Artifact(), nil
}

func FailureReason(kind string, err error) string {
	if errors.Is(err, workspace.ErrIdentity) || errors.Is(err, workspace.ErrIdentityMismatch) || errors.Is(err, task.ErrIdentityMismatch) {
		return "workspace_identity_changed"
	}
	if errors.Is(err, exec.ErrNotFound) {
		if kind == workspace.OperationVerify {
			return "verification_command_unavailable"
		}
		return "git_executable_unavailable"
	}
	switch kind {
	case workspace.OperationProvision:
		return "workspace_provision_failed"
	case workspace.OperationCapture:
		return "workspace_capture_failed"
	case workspace.OperationVerify:
		return "workspace_verification_failed"
	case workspace.OperationRemove:
		return "workspace_removal_failed"
	case workspace.OperationForget:
		return "workspace_forget_failed"
	case workspace.OperationCheckpoint:
		return "checkpoint_failed"
	case workspace.OperationCheckpointRemove:
		return "checkpoint_removal_failed"
	default:
		return "workspace_operation_failed"
	}
}
