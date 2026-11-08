package app

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspaceworker "github.com/hishamkaram/delegation-layer/internal/workspaces/worker"
)

func loadWorkspaceGuardianState(store *taskdir.Store, id string) (lifetimeGuardianRunState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	operation, err := workspace.LoadOperation(ctx, store, id)
	if err != nil {
		return lifetimeGuardianRunState{}, err
	}
	return lifetimeGuardianRunState{
		ownerKind: task.LifetimeOwnerWorkspace,
		records:   operation,
		acquireOwner: func(ownerContext context.Context) (guardian.Owner, error) {
			return operation.AcquireLifetimeOwner(ownerContext)
		},
		reconcileOwned: func(ownerContext context.Context) error {
			return guardian.ReconcileRecordsOwned(ownerContext, operation)
		},
		deadline:   operation.Deadline(),
		closeOwner: operation.Close,
	}, nil
}

func runWorkspaceOperationWorker(parsed runnerArguments, deps Dependencies) (resultErr error) {
	deps = deps.withCapturedEnvironment()
	if !parsed.WorkspaceOperation || !parsed.Supervised || !parsed.LifetimeChild {
		return task.ErrInvalidPermit
	}
	root, err := tasklifecycle.ResolveRoot(parsed.Root)
	if err != nil {
		return err
	}
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	loadContext, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	operation, err := workspace.LoadOperation(loadContext, store, parsed.TaskID)
	cancel()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	return runLoadedWorkspaceWorker(parsed, store, operation, deps.Environment)
}

func runLoadedWorkspaceWorker(parsed runnerArguments, store *taskdir.Store, operation *workspace.Operation, environment []string) (resultErr error) {
	ctx, cancel := context.WithDeadline(context.Background(), operation.Deadline())
	defer cancel()
	if err := validateWorkspaceWorker(ctx, operation); err != nil {
		return err
	}
	child, err := openLifetimeChild(parsed, operation)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, child.Close()) }()
	created, err := operation.Claim(ctx, "start.json")
	if err != nil || !created {
		return errors.Join(task.ErrInvalidPermit, err)
	}
	var candidate workspace.OperationResult
	stopper := &workspaceBudgetStopper{operation: operation, lifetime: child.Lifetime()}
	workErr, stopErr := execution.RunSupervised(operation.Deadline(), execution.SupervisedOptions{Stopper: stopper, Lifetime: child.Lifetime(), PrepareEnvironment: operationalEnvironmentPreparation(store)}, func(scope execution.PreflightScope) error {
		var actionErr error
		candidate, actionErr = workspaceworker.RunAction(scope, store, operation, environment)
		return actionErr
	})
	if workErr != nil || stopErr != nil {
		failureErr := errors.Join(workErr, stopErr)
		if ctx.Err() == nil {
			return recordWorkspaceWorkerFailure(ctx, store, operation, failureErr)
		}
		return failureErr
	}
	// The worker publishes only a candidate. The coordinator separately checks
	// this helper's guardian termination and exact child Wait before acceptance.
	return recordWorkspaceWorkerResult(ctx, store, operation, candidate)
}

// The worker owns artifact publication ordering. Files are closed and synced
// before their directory entries, then the immutable result may name them.
func recordWorkspaceWorkerResult(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, result workspace.OperationResult) error {
	if len(result.Artifacts) != 0 {
		if err := taskdir.SyncDirectoryEntry(workspace.ScratchPath(store.Root, operation.Request().OperationID)); err != nil {
			if result.Status != "failed" {
				return err
			}
			// A diagnostic fault must not suppress an already known failure.
			// The result cannot claim artifacts whose entries were not durable.
			result.Artifacts = nil
			return errors.Join(err, operation.RecordResult(ctx, result))
		}
	}
	return operation.RecordResult(ctx, result)
}

func recordWorkspaceWorkerFailure(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, cause error) error {
	result := workspace.OperationResult{SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID, RequestSHA256: operation.Digest(), Status: "failed", ReasonCode: workspaceworker.FailureReason(operation.Request().Kind, cause)}
	artifact, diagnosticErr := workspaceworker.WriteFailureDiagnostic(store, operation, cause)
	if diagnosticErr == nil {
		result.Artifacts = []workspace.Artifact{artifact}
	}
	return errors.Join(cause, diagnosticErr, recordWorkspaceWorkerResult(ctx, store, operation, result))
}

func validateWorkspaceWorker(ctx context.Context, operation *workspace.Operation) error {
	current, err := os.Executable()
	if err != nil {
		return err
	}
	current, err = config.CanonicalizePath(current)
	if err != nil {
		return err
	}
	request := operation.Request()
	if current != request.WorkerExecutable || !time.Now().Before(operation.Deadline()) {
		return task.ErrIdentityMismatch
	}
	_, err = operation.Receipt(ctx)
	return err
}

type workspaceBudgetStopper struct {
	operation *workspace.Operation
	lifetime  execution.Lifetime
}

func (s *workspaceBudgetStopper) PrepareBudget(deadline time.Time) (execution.BudgetRequest, error) {
	if s == nil || s.operation == nil || s.lifetime == nil || !deadline.Equal(s.operation.Deadline()) {
		return nil, task.ErrInvalidPermit
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	if _, err := s.operation.Claim(ctx, "stop.json"); err != nil {
		return nil, err
	}
	return func(stopContext context.Context) error {
		return s.lifetime.Terminate(execution.DetachBudgetRequestContext(stopContext))
	}, nil
}
