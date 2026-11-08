package taskdir

import (
	"context"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// OpenRemoteReferenceContext opens origin bookkeeping, never an executable task.
// The remote coordinator owns record values; this store owns their durability.
func (s *Store) OpenRemoteReferenceContext(ctx context.Context, id string, create bool) (*ControlDir, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "remote-refs", id), create)
}

// OpenRemoteCaptureContext opens post-closure capture receipts for one task.
func (s *Store) OpenRemoteCaptureContext(ctx context.Context, id string, create bool) (*ControlDir, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "remote-captures", id), create)
}

// OpenRemoteTrustContext serializes explicit Delegate-owned host enrollment.
func (s *Store) OpenRemoteTrustContext(ctx context.Context, create bool) (*ControlDir, error) {
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "remote-trust"), create)
}

// ReadTaskRequest passively reads one immutable request without opening task
// locks, initialization, probing, recovery or execution authority.
func (r *ReadOnlyStore) ReadTaskRequest(ctx context.Context, id string) (*task.TaskRecord, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	td := &TaskDir{store: r.store, TaskID: id, Dir: filepath.Join(r.store.Root, "tasks", id)}
	return td.RecordedRequest()
}

// ReadTaskInteraction reads existing interaction evidence without task locks.
func (r *ReadOnlyStore) ReadTaskInteraction(ctx context.Context, id string, after uint64) (task.InteractionState, []task.InteractionEvent, uint64, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return task.InteractionState{}, nil, after, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return task.InteractionState{}, nil, after, err
	}
	td := &TaskDir{store: r.store, TaskID: id, Dir: filepath.Join(r.store.Root, "tasks", id)}
	records, state, err := td.interactionRecords()
	if err != nil {
		return state.InteractionState, nil, after, err
	}
	events, cursor, err := interactionEventPage(records, state.InteractionState, after)
	return state.InteractionState, events, cursor, err
}

// OpenRemoteInputJournalContext owns destination byte staging independently of
// origin references, including self-SSH requests using the same execution root.
func (s *Store) OpenRemoteInputJournalContext(ctx context.Context, id string, create bool) (*ControlDir, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "remote-inputs", id), create)
}
