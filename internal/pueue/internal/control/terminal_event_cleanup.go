package control

import (
	"context"
	"path/filepath"
)

// ClearPrivateTerminalEvents resets callback classification after an exact row
// was removed or before a newly allocated numeric ID is enqueued. The shared
// lock closes the callback/classification race.
func (c *Client) ClearPrivateTerminalEvents(ctx context.Context, ids ...int64) error {
	stateRoot, enabled, err := privateTerminalEventsEnabled(ctx, c)
	if err != nil || !enabled || len(ids) == 0 {
		return err
	}
	return withPrivateTerminalEventLock(ctx, stateRoot, func(base string) error {
		changed, err := removePrivateTerminalEventRecords(base, ids)
		if err != nil {
			return err
		}
		return syncPrivateTerminalEventDirectories(base, changed)
	})
}

func removePrivateTerminalEventRecords(base string, ids []int64) (map[string]bool, error) {
	return removePrivateTerminalEventRecordsFromDirectories(base, ids, privateTerminalEventTicketDir, privateTerminalEventRecordDir)
}

func removePrivateTerminalEventRecordsFromDirectories(base string, ids []int64, directories ...string) (map[string]bool, error) {
	changed := make(map[string]bool, 2)
	for _, id := range ids {
		if err := removePrivateTerminalEventRecordsForID(base, id, changed, directories); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

func removePrivateTerminalEventRecordsForID(base string, id int64, changed map[string]bool, directories []string) error {
	if id < 0 {
		return ErrUnknown
	}
	for _, directory := range directories {
		if directory != privateTerminalEventTicketDir && directory != privateTerminalEventRecordDir {
			return ErrConfiguration
		}
		removed, err := removeTerminalEventRecord(base, directory, id)
		if err != nil {
			return err
		}
		changed[directory] = changed[directory] || removed
	}
	return nil
}

func syncPrivateTerminalEventDirectories(base string, changed map[string]bool) error {
	for directory, modified := range changed {
		if !modified {
			continue
		}
		if err := syncPrivateDirectory(filepath.Join(base, directory)); err != nil {
			return err
		}
	}
	return nil
}
