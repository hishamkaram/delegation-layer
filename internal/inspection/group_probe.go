package inspection

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ProbeGroupContext checks an established current group without creating a
// journal, observation, or permit. The caller keeps its supervisor generation
// stable while obtaining the snapshot and performing this check.
func ProbeGroupContext(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, snapshot pueue.QueueSnapshot) (ready bool, resultErr error) {
	if ctx == nil {
		return false, errors.New("nil inspection context")
	}
	expected, err := expectedGroupRequest(store, binding)
	if err != nil {
		return false, err
	}
	head, _, err := ReadGroupHead(ctx, store)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !sameGroupRequest(head, expected) {
		return false, err
	}
	control, err := openMigrationControl(ctx, store, head, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, control.Close()) }()
	err = control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		return verifyExistingGroupObservation(tx, head, snapshot)
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func verifyExistingGroupObservation(tx *taskdir.ControlTransaction, head GroupRequestRecord, snapshot pueue.QueueSnapshot) error {
	var request GroupRequestRecord
	if err := tx.Read(groupRequestRecordName, &request); err != nil {
		return err
	}
	if !sameGroupRequest(request, head) {
		return task.ErrEvidenceFault
	}
	if err := tx.Verify(groupRequestRecordName, request); err != nil {
		return err
	}
	if _, found := snapshot.Groups[request.Name]; !found {
		return os.ErrNotExist
	}
	observation, err := groupObservationForSnapshot(request, groupRequestDigest(request), snapshot)
	if err != nil {
		return err
	}
	return tx.Verify(groupObservationRecordName, observation)
}
