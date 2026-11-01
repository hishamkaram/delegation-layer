package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type recordReader interface {
	SchedulerHostRecordNamesContext(context.Context) ([]string, error)
	ReadSchedulerHostRecordContext(context.Context, string) ([]byte, error)
}

// publishRecord preserves every original fault. Matching readback must also
// repeat the canonical durability barrier before the caller can acknowledge a
// committed stop. It never interprets an unreadable/conflicting record as absent.
func publishRecord[T epochRecord | stopRecord | attemptRecord | closedRecord | foregroundDoneRecord](ctx context.Context, control *taskdir.ControlDir, name string, record T) (bool, error) {
	_, err := control.PutContext(ctx, name, record)
	if err == nil {
		return true, nil
	}
	var winner T
	if readErr := control.Read(name, &winner); readErr != nil {
		return false, errors.Join(err, readErr)
	}
	if winner != record {
		return false, errors.Join(err, task.ErrEvidenceFault)
	}
	verifyErr := control.WithLock(context.WithoutCancel(ctx), func(tx *taskdir.ControlTransaction) error {
		return tx.Verify(name, record)
	})
	return verifyErr == nil, errors.Join(err, verifyErr)
}

func readLatest(ctx context.Context, reader recordReader, rootID string) (epochRecord, bool, error) {
	names, err := reader.SchedulerHostRecordNamesContext(ctx)
	if errors.Is(err, fs.ErrNotExist) {
		return epochRecord{}, false, nil
	}
	if err != nil {
		return epochRecord{}, false, err
	}
	sequence, err := latestSequence(names)
	if err != nil || sequence == 0 {
		return epochRecord{}, false, err
	}
	data, err := reader.ReadSchedulerHostRecordContext(ctx, epochName(sequence))
	if err != nil {
		return epochRecord{}, false, errors.Join(task.ErrEvidenceFault, err)
	}
	var epoch epochRecord
	if err = task.DecodeStrict(data, &epoch); err != nil {
		return epochRecord{}, false, err
	}
	if epoch.Sequence != sequence {
		return epochRecord{}, false, ErrIdentity
	}
	if err = epoch.validate(rootID); err != nil {
		return epochRecord{}, false, err
	}
	return epoch, true, nil
}

func nextSequence(ctx context.Context, control *taskdir.ControlDir) (uint64, error) {
	names, err := control.RecordNamesContext(ctx)
	if err != nil {
		return 0, err
	}
	sequence, err := latestSequence(names)
	if err != nil {
		return 0, err
	}
	if sequence == ^uint64(0) {
		return 0, fmt.Errorf("scheduler host generation exhausted")
	}
	return sequence + 1, nil
}
