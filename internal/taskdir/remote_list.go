package taskdir

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RemoteReferenceIDs passively lists a bounded page of origin references.
func (r *ReadOnlyStore) RemoteReferenceIDs(ctx context.Context, after string) (ids []string, next string, resultErr error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, "", err
	}
	directory, err := r.store.rootHandle.Open("remote-refs")
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	for {
		entries, readErr := directory.ReadDir(100)
		for _, entry := range entries {
			if task.ValidateTaskID(entry.Name()) == nil && entry.Name() > after {
				ids = append(ids, entry.Name())
			}
		}
		sort.Strings(ids)
		if len(ids) > 101 {
			ids = ids[:101]
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, "", readErr
		}
		if err = ctx.Err(); err != nil {
			return nil, "", err
		}
	}
	sort.Strings(ids)
	if len(ids) > 100 {
		ids = ids[:100]
		next = ids[len(ids)-1]
	}
	return ids, next, nil
}
