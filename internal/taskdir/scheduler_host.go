package taskdir

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const schedulerHostDirectory = "scheduler-host"

// OpenSchedulerHostContext grants the live host a nonsecret durable journal.
// Its external H lease owns transition policy; this store owns rooted writes.
func (s *Store) OpenSchedulerHostContext(ctx context.Context, create bool) (*ControlDir, error) {
	return s.openControlDirContext(ctx, filepath.Join(s.Root, schedulerHostDirectory), create)
}

// SchedulerHostRecordNamesContext is a passive, rooted enumeration. It opens no
// locks and creates nothing; the host package validates canonical record names.
func (r *ReadOnlyStore) SchedulerHostRecordNamesContext(ctx context.Context) (names []string, resultErr error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	directory, err := r.store.openDir(filepath.Join(r.store.Root, schedulerHostDirectory))
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	names, err = directory.Readdirnames(-1)
	return names, errors.Join(err, ctx.Err())
}

// ReadSchedulerHostRecordContext returns bounded bytes without mutation
// authority. Schema, generation and process identity belong to the host owner.
func (r *ReadOnlyStore) ReadSchedulerHostRecordContext(ctx context.Context, name string) ([]byte, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := validateControlName(name); err != nil {
		return nil, err
	}
	return r.store.readBytesContext(ctx, filepath.Join(r.store.Root, schedulerHostDirectory, name), task.MaxControlRecordSize)
}

// VerifySchedulerHostRecordContext is explicit stop-recovery authority only.
// It never changes content or initializes storage: exact existing bytes must
// match before the canonical directory/parent durability barriers are repeated.
// Passive observation must use the read methods instead.
func (r *ReadOnlyStore) VerifySchedulerHostRecordContext(ctx context.Context, name string, expected []byte) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return err
	}
	if err := validateControlName(name); err != nil {
		return err
	}
	if len(expected) == 0 || len(expected) > task.MaxControlRecordSize {
		return task.ErrEvidenceFault
	}
	err := r.store.verifyCanonicalControlRecord(filepath.Join(r.store.Root, schedulerHostDirectory), name, expected)
	return errors.Join(err, ctx.Err())
}
