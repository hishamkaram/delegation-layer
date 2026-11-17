package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type taskClosureObservation struct {
	RootID          string                                `json:"root_id"`
	TaskID          string                                `json:"task_id"`
	Closed          bool                                  `json:"closed"`
	Kind            taskdir.ConnectorClosureKind          `json:"kind,omitempty"`
	ReferenceSHA256 string                                `json:"reference_sha256,omitempty"`
	PendingReason   taskdir.ConnectorClosurePendingReason `json:"pending_reason,omitempty"`
}

// observeTaskClosure delegates closure authority and existing-inode exclusions
// to taskdir. The production compiled registry includes actual Pi and retained
// fixture predicates; this observer cannot prepare or execute any provider.
func observeTaskClosure(ctx context.Context, root, id string) (result taskClosureObservation, resultErr error) {
	if err := task.ValidateTaskID(id); err != nil {
		return result, err
	}
	store, err := taskdir.OpenExistingStoreMetadataContext(ctx, root, app.NativePredicates())
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	observed, err := store.ReadTaskExecutionClosureContext(ctx, id)
	if err != nil {
		return result, err
	}
	result = taskClosureObservation{RootID: store.RootID, TaskID: id, PendingReason: observed.PendingReason}
	if observed.Proof != nil {
		result.Closed = true
		result.Kind = observed.Proof.Kind
		result.ReferenceSHA256 = observed.Proof.ReferenceSHA256
	}
	return result, nil
}

func encodeTaskClosure(ctx context.Context, root, id string) error {
	result, err := observeTaskClosure(ctx, root, id)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
