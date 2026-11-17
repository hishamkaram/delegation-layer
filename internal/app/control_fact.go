package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The owner validates this diagnostic DTO before it reaches the public error.
// Its safe metadata never reconstructs the owner's private error graph.
func supervisorUpgradeFactCause(fact infrastructure.UpgradeFailureFact, cause error) error {
	if fact == (infrastructure.UpgradeFailureFact{}) {
		return cause
	}
	return errors.Join(cause, &task.ControlFactError{ResourceKind: fact.ResourceKind, ResourceID: fact.ResourceID, FactKind: fact.FactKind, Name: fact.Name, ReasonCode: fact.ReasonCode, NextAction: fact.NextAction, Cause: task.ErrEvidenceFault})
}

func localTaskTargetError(ctx context.Context, store *taskdir.Store, id, command string, cause error) error {
	if !task.RecordAbsent(cause) {
		return cause
	}
	ids, err := store.InspectionOperationIDs(ctx)
	if err != nil {
		return errors.Join(cause, err)
	}
	for _, candidate := range ids {
		if candidate == id {
			if command == "status" {
				return &task.ControlFactError{ResourceKind: "inspection", ResourceID: id, FactKind: "target_kind", Name: "inspection", ReasonCode: "inspection_not_task", NextAction: "inspect_inspection", Cause: cause}
			}
			return &task.ControlFactError{ResourceKind: "inspection", ResourceID: id, FactKind: "target_kind", Name: "inspection", ReasonCode: "inspection_requires_kind", NextAction: "settle_with_kind_inspection", Cause: cause}
		}
	}
	return &task.ControlFactError{ResourceKind: "task", ResourceID: id, FactKind: "task_record", Name: "tasks/" + id, ReasonCode: "task_not_found", NextAction: "check_task_id", Cause: cause}
}

func withSettlementBootstrap(ctx context.Context, store *taskdir.Store, action func(context.Context) error) error {
	entered := false
	err := pueue.WithExistingBootstrapLockContext(ctx, store.Root, func(locked context.Context) error { entered = true; return action(locked) })
	if !entered && task.RecordAbsent(err) {
		return &task.ControlFactError{ResourceKind: "root", ResourceID: store.RootID, FactKind: "evidence_record", Name: ".supervisor/bootstrap.lock", ReasonCode: "supervisor_not_initialized", NextAction: "inspect_supervisor", Cause: err}
	}
	return err
}
