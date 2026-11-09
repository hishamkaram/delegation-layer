package taskdir

import (
	"context"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ReadRemoteIntentBytes reads only the bounded committed origin intent. It
// creates no locks, probes, directories or observation records.
func (r *ReadOnlyStore) ReadRemoteIntentBytes(ctx context.Context, id string) ([]byte, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	return r.store.readBytes(filepath.Join(r.store.Root, "remote-refs", id, "intent.json"), task.MaxControlRecordSize)
}
