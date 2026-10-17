package journal

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

func (r *Resource) RecordAnchor(ctx context.Context, record contract.AnchorRecord) error {
	if err := r.validateAnchor(record); err != nil {
		return err
	}
	_, err := r.control.PutContext(ctx, "anchor.json", record)
	return err
}

func (r *Resource) Anchor(ctx context.Context) (record contract.AnchorRecord, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := tx.Read("anchor.json", &record); err != nil {
			return err
		}
		if err := r.validateAnchor(record); err != nil {
			return err
		}
		return tx.Verify("anchor.json", record)
	})
	return record, resultErr
}

func (r *Resource) validateAnchor(record contract.AnchorRecord) error {
	if record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != r.request.WorkspaceID || record.Path != filepath.Join(r.request.Container, r.request.WorkspaceID) || record.Identity.Inode == 0 {
		return errors.Join(contract.ErrIdentity, task.ErrIdentityMismatch)
	}
	return nil
}
