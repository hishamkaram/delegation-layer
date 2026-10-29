package taskdir

import "context"

// WorkspaceOperationIDs lists the sorted identities of existing workspace
// operation directories through the pinned root. Each name is only a
// candidate index: the operation's own request record remains authoritative,
// and a directory whose request is not yet committed is still listed. An
// absent operations parent lists nothing and creates nothing. Any entry that
// is not a canonical identity naming a private directory, including a
// symlink, is refused rather than omitted.
func (s *Store) WorkspaceOperationIDs(ctx context.Context) ([]string, error) {
	return s.privateDirectoryIDs(ctx, workspaceOperationsDirectory, "workspace operation")
}
