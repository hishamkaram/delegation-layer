package taskdir

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const infrastructureDirectory = "infrastructure"

// OpenInfrastructureContext grants the infrastructure runtime a private durable journal.
// The runtime owns schema, lifecycle and its instance lease; this store owns
// rooted persistence. ControlDir.WithLock is for short record transactions only,
// never native Start, readiness or external waits.
func (s *Store) OpenInfrastructureContext(ctx context.Context, create bool) (*ControlDir, error) {
	return s.openControlDirContext(ctx, filepath.Join(s.Root, infrastructureDirectory), create)
}

// InfrastructureRecordNamesContext is a passive, rooted enumeration. It opens no
// locks and creates nothing; the infrastructure runtime validates canonical record names.
func (r *ReadOnlyStore) InfrastructureRecordNamesContext(ctx context.Context) (names []string, resultErr error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	directory, err := r.store.openDir(filepath.Join(r.store.Root, infrastructureDirectory))
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	names, err = directory.Readdirnames(-1)
	return names, errors.Join(err, ctx.Err())
}

// ReadInfrastructureRecordContext returns bounded bytes without mutation
// authority. Schema, generation and process identity belong to the infrastructure runtime.
func (r *ReadOnlyStore) ReadInfrastructureRecordContext(ctx context.Context, name string) ([]byte, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := validateControlName(name); err != nil {
		return nil, err
	}
	return r.store.readBytesContext(ctx, filepath.Join(r.store.Root, infrastructureDirectory, name), task.MaxControlRecordSize)
}
