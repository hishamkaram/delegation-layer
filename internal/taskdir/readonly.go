package taskdir

import (
	"context"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ReadOnlyStore owns a rooted evidence reader, not execution or repair
// authority. Its private Store reuses the canonical record readers; callers
// cannot obtain its mutation methods or task control handles.
type ReadOnlyStore struct {
	store *Store
}

// OpenReadOnlyStoreContext validates an existing root without filesystem
// capability probes, lock creation, initialization or repair.
func OpenReadOnlyStoreContext(ctx context.Context, root string, registry predicate.Registry) (*ReadOnlyStore, error) {
	store, err := readExistingStoreMetadataContext(ctx, root, registry)
	if err != nil {
		return nil, err
	}
	return &ReadOnlyStore{store: store}, nil
}

// Root and RootID are the immutable validated scheduler storage binding.
func (r *ReadOnlyStore) Root() string { return r.store.Root }

func (r *ReadOnlyStore) RootID() string { return r.store.RootID }

// OpenTaskPayload opens only a result payload through this reader's retained
// root. The caller validates publication first and owns the read-only file.
func (r *ReadOnlyStore) OpenTaskPayload(ctx context.Context, id string, descriptor task.PayloadDescriptor) (*os.File, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	if err := task.ValidatePayloadDescriptor(descriptor); err != nil {
		return nil, err
	}
	return r.store.openFile(filepath.Join(r.store.Root, "tasks", id, descriptor.Basename), os.O_RDONLY)
}

func (r *ReadOnlyStore) Close() error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.store.closed {
		return nil
	}
	r.store.closed = true
	return r.store.rootHandle.Close()
}

func (r *ReadOnlyStore) checkReadContext(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.store.closed {
		return os.ErrClosed
	}
	return nil
}
