package workspace

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// AnchorRecord pins the workspace's private parent before Git modifies the
// repository. Its symlink marker points at this journal, not a second owner.
type AnchorRecord struct {
	SchemaVersion int               `json:"schema_version"`
	WorkspaceID   string            `json:"workspace_id"`
	Path          string            `json:"path"`
	Identity      DirectoryIdentity `json:"identity"`
}

func (r *Resource) RecordAnchor(ctx context.Context, record AnchorRecord) error {
	if err := r.validateAnchor(record); err != nil {
		return err
	}
	_, err := r.control.PutContext(ctx, "anchor.json", record)
	return err
}

func (r *Resource) Anchor(ctx context.Context) (record AnchorRecord, resultErr error) {
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

func (r *Resource) validateAnchor(record AnchorRecord) error {
	if record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != r.request.WorkspaceID || record.Path != filepath.Join(r.request.Container, r.request.WorkspaceID) || record.Identity.Inode == 0 {
		return errors.Join(ErrIdentity, task.ErrIdentityMismatch)
	}
	return nil
}
