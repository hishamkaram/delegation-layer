package taskdir

import "context"

// WorkspaceResourceIDs lists sorted physical candidates, including orphan and
// partial directories. It creates nothing and reads no inventory/request/state
// records. Workspace policy compares these candidates with its committed
// inventory; neither directory presence nor absence proves closure or custody.
// Malformed, unsafe and symlink entries refuse the whole listing.
func (s *Store) WorkspaceResourceIDs(ctx context.Context) ([]string, error) {
	return s.privateDirectoryIDs(ctx, "workspaces", "workspace resource")
}
