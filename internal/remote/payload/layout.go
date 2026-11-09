package payload

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type originLayout = task.RemoteOriginLayout

func snapshotLayout(tx *taskdir.ControlTransaction, create bool) (task.RemoteDataLayout, error) {
	var record originLayout
	err := tx.Read("origin-layout.json", &record)
	if err == nil {
		return record.StorageLayout, record.StorageLayout.Validate()
	}
	if !task.RecordAbsent(err) {
		return 0, err
	}
	var ready remoteInputStart
	err = tx.Read("manifest-ready.json", &ready)
	if err == nil {
		return task.RemoteDataLegacy, nil
	}
	if !task.RecordAbsent(err) {
		return 0, err
	}
	if !create {
		return 0, err
	}
	record.StorageLayout = task.RemoteDataOrganized
	_, err = tx.Put("origin-layout.json", record)
	return record.StorageLayout, err
}

func CurrentLayout(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, role remotedata.Role) (task.RemoteDataLayout, error) {
	if role == remotedata.Destination {
		request, err := reader.ReadTaskRequest(ctx, id)
		if err != nil {
			return 0, err
		}
		if request.Remote == nil {
			return 0, task.ErrIdentityMismatch
		}
		return request.Remote.StorageLayout, request.Remote.StorageLayout.Validate()
	}
	if role != remotedata.Origin {
		return 0, task.ErrEvidenceFault
	}
	var record originLayout
	err := reader.ReadRemoteRecord(ctx, id, true, false, "origin-layout.json", &record)
	if err == nil {
		return record.StorageLayout, record.StorageLayout.Validate()
	}
	if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var ready remoteInputStart
	err = reader.ReadRemoteRecord(ctx, id, true, false, "manifest-ready.json", &ready)
	return task.RemoteDataLegacy, err
}

func originWithoutSnapshot(ctx context.Context, reader *taskdir.ReadOnlyStore, id string) (bool, error) {
	facts, err := remote.ReadOriginAdmission(ctx, reader, id)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return facts.Intent.InputSHA256 == "", nil
}
