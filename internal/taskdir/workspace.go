package taskdir

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// StateRoot returns the root owning this pinned control directory.
func (d *ControlDir) StateRoot() string { return d.store.Root }

// SyncDirectoryEntry uses the same native durability barrier as control
// records for the managed container's journal-pointer directory entry.
func SyncDirectoryEntry(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return task.ErrIdentityMismatch
	}
	return errors.Join(platformBarrierDir(path), platformBarrierDir(filepath.Dir(path)))
}

// OpenWorkspaceRegistry opens the root-owned workspace identity inventory.
// The application owns its record schemas and transition decisions.
func (s *Store) OpenWorkspaceRegistry(ctx context.Context, create bool) (*ControlDir, error) {
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "workspace-registry"), create)
}

// OpenWorkspace opens one workspace resource journal, independent of tasks.
func (s *Store) OpenWorkspace(ctx context.Context, id string, create bool) (*ControlDir, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "workspaces", id), create)
}

// OpenWorkspaceOperation opens one finite workspace operation journal. Native
// lifecycle records share the existing control-directory durability protocol.
func (s *Store) OpenWorkspaceOperation(ctx context.Context, id string, create bool) (*ControlDir, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "workspace-operations", id), create)
}

// WorkspaceArtifactDirectory is the one layout owner for optional workspace
// bytes. Callers validate operation identities before constructing locations.
func WorkspaceArtifactDirectory(root, operationID string) string {
	return filepath.Join(root, "workspace-operations", operationID, "artifacts")
}
