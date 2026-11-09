package payload

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func retirementBlockers(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, role remotedata.Role, layout task.RemoteDataLayout, checkExecution bool) ([]string, error) {
	if record, retired, err := ReadRetirement(ctx, reader, id, role); err != nil {
		return nil, err
	} else if retired {
		if record.StorageLayout != layout {
			return nil, task.ErrIdentityMismatch
		}
		return []string{}, nil
	}
	blockers := []string{}
	if role == remotedata.Origin {
		facts, err := remote.ReadOriginAdmission(ctx, reader, id)
		if err != nil {
			return nil, err
		}
		if !facts.Sent && !facts.Rejected {
			blockers = append(blockers, "origin_not_sent")
		}
	} else {
		taskBlockers, err := destinationBlockers(ctx, reader, id, layout, checkExecution)
		if err != nil {
			return nil, err
		}
		blockers = append(blockers, taskBlockers...)
	}
	consumers, err := consumerBlockers(ctx, reader, id, role, checkExecution)
	return append(blockers, consumers...), err
}

func destinationBlockers(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, layout task.RemoteDataLayout, checkExecution bool) ([]string, error) {
	request, err := reader.ReadTaskRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	if request.Remote == nil {
		return nil, task.ErrIdentityMismatch
	}
	blockers := []string{}
	if layout == task.RemoteDataLegacy && request.Remote.InputSHA256 != "" {
		blockers = append(blockers, "legacy_inputs_consumers_unknown")
	}
	closed, err := reader.RemoteTaskClosed(ctx, id)
	if err != nil {
		return nil, err
	}
	if !closed {
		blockers = append(blockers, "task_not_closed")
	}
	if closed && checkExecution {
		released, releaseErr := reader.RemoteExecutionReleased(ctx, id)
		if releaseErr != nil {
			return nil, releaseErr
		}
		if !released {
			blockers = append(blockers, "execution_owned")
		}
	}
	if len(request.Remote.OutputPaths) > 0 {
		if err = hasCompleteCapture(ctx, reader, id); onlyMissing(err) {
			blockers = append(blockers, "capture_pending")
		} else if err != nil {
			return nil, err
		}
	}
	return blockers, nil
}

func hasCompleteCapture(ctx context.Context, reader *taskdir.ReadOnlyStore, id string) error {
	root, err := os.OpenRoot(reader.Root())
	if err != nil {
		return err
	}
	_, err = ReadCapturePassive(ctx, root, reader.RootID(), id)
	return errors.Join(err, root.Close())
}

func consumerBlockers(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, role remotedata.Role, checkExecution bool) ([]string, error) {
	names, err := reader.RemoteRecordNames(ctx, id, role == remotedata.Origin)
	if errors.Is(err, os.ErrNotExist) && role == remotedata.Destination {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	blockers := []string{}
	for _, name := range names {
		if !strings.HasPrefix(name, "use-") {
			continue
		}
		blocker, e := consumerBlocker(ctx, reader, id, role, name, checkExecution)
		if e != nil {
			if blocker != "" {
				blockers = append(blockers, blocker)
			}
			return blockers, e
		}
		if blocker != "" {
			blockers = append(blockers, blocker)
		}
	}
	return blockers, nil
}

func consumerBlocker(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, role remotedata.Role, name string, checkExecution bool) (string, error) {
	var use taskdir.RemoteUseRecord
	if err := reader.ReadRemoteRecord(ctx, id, role == remotedata.Origin, false, name, &use); err != nil {
		return "", err
	}
	expected, err := taskdir.RemoteUseName(use)
	if err != nil || expected != name {
		return "", errors.Join(task.ErrEvidenceFault, err)
	}
	consumer, err := reader.RemoteReader(ctx, use.Root)
	if err != nil {
		return "consumer_unavailable:" + use.TaskID, errors.Join(ErrRetirementBlocked, err)
	}
	err = inspectConsumer(ctx, consumer, use, checkExecution)
	err = errors.Join(err, consumer.Close())
	if task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, ErrRetirementBlocked) }) {
		return "consumer_not_closed:" + use.TaskID, nil
	}
	return "", err
}

func inspectConsumer(ctx context.Context, reader *taskdir.ReadOnlyStore, use taskdir.RemoteUseRecord, checkExecution bool) error {
	if reader.RootID() != use.RootID {
		return task.ErrIdentityMismatch
	}
	request, err := reader.ReadTaskRequest(ctx, use.TaskID)
	if errors.Is(err, os.ErrNotExist) {
		return ErrRetirementBlocked
	}
	if err != nil {
		return err
	}
	if request.CanonicalCwd != use.Cwd {
		return task.ErrIdentityMismatch
	}
	closed, err := reader.RemoteTaskClosed(ctx, use.TaskID)
	if err != nil {
		return err
	}
	if !closed {
		return ErrRetirementBlocked
	}
	if checkExecution {
		released, releaseErr := reader.RemoteExecutionReleased(ctx, use.TaskID)
		if releaseErr != nil {
			return releaseErr
		}
		if !released {
			return ErrRetirementBlocked
		}
	}
	if request.Remote != nil && len(request.Remote.OutputPaths) > 0 {
		if err = hasCompleteCapture(ctx, reader, use.TaskID); onlyMissing(err) {
			return ErrRetirementBlocked
		}
		return err
	}
	return nil
}

func onlyMissing(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, os.ErrNotExist) })
}
