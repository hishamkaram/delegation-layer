package taskdir

import "context"

// TaskIDs lists the sorted identities of existing task directories through
// the pinned root. Each name is only a read-only candidate, never release
// authority: the task's own records remain authoritative, and a partially
// created directory without its locks or records is still listed. It reads no
// task record and creates no directory, file or lock; an absent tasks parent
// lists nothing. Any entry that is not a canonical identity naming a private
// directory, including a symlink, is refused rather than omitted.
func (s *Store) TaskIDs(ctx context.Context) ([]string, error) {
	return s.privateDirectoryIDs(ctx, "tasks", "task")
}
