package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// seedAdmission creates a normal consumed admission permit, without ever
// enqueueing or launching a task. Its obsolete runner path is deliberately absent.
func seedAdmission(ctx context.Context, store *taskdir.Store, workspace, id string) (resultErr error) {
	binding, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil || !found {
		return errors.Join(errors.New("admission fixture requires existing supervisor binding"), err)
	}
	brief := []byte("Owned never-launched upgrade fixture")
	requested := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	req := &task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id,
		Provider: "fixture:test", Mode: "read-only", CanonicalCwd: workspace, RequestedConfig: requested,
		BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
	}
	digest := task.ComputeSHA256([]byte("owned admission fixture policy"))
	meta := &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id,
		RequestedConfig: requested, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment: "fixture-only", Approval: "never", ProviderExecutable: filepath.Join(workspace, "removed-0.5.0", "provider"),
		RunnerExecutable: filepath.Join(workspace, "removed-0.5.0", "delegate-run"), RunnerOwnership: task.RunnerOwnershipManaged,
		ProviderVersion: "fixture-v2", PublisherBuild: "upgrade-fixture", PublisherVersion: "0.5.0",
		Predicate: task.FixturePredicateRef(), SupervisorConfig: binding, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := store.CreateTaskContext(ctx, id, req, brief, meta)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	permit, err := td.PrepareSubmissionContext(ctx, binding)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, permit.Release()) }()
	return permit.Consume()
}
