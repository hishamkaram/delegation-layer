package taskdir

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// TaskEntryExists reports whether the name tasks/<id> is present in the store.
// It is a read-only observation: it creates no directory or lock, writes
// nothing, and never follows the entry or its parent. A directory, a partial
// directory missing its lock files, a regular file, or a symlink (including a
// dangling one) at that name all report true. A missing tasks directory
// reports false.
//
// The result is name presence only. It does not prove the entry is a prepared
// task, that its records are valid, or that the caller may execute it; use
// OpenTask and the validated readers for that. It is also a point-in-time
// observation, not an atomic absence claim: a concurrent CreateTask can make a
// false result stale at once, so a caller must still rely on CreateTask's
// create-once recovery and treat an explicit task ID that races as unknown.
//
// An invalid ID, a closed store, an unsafe or replaced tasks directory, and any
// other read failure return a non-nil error and never report absence.
func (s *Store) TaskEntryExists(id string) (bool, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return false, err
	}
	exists, err := s.entryExists(filepath.Join(s.Root, "tasks", id))
	if errors.Is(err, fs.ErrNotExist) {
		// entryExists maps a missing leaf to (false, nil), so a not-exist error
		// here means the tasks directory itself is absent.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("observe task entry %s: %w", id, err)
	}
	return exists, nil
}
