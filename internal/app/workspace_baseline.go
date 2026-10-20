package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// observeManagedTurnBaseline records the turn's actual start content, HEAD and
// real-index identity at the last preflight boundary before provider Start.
// It borrows the task's own supervised scope, so it shares the task deadline
// and command owner; any failure refuses this launch. The baseline ref keeps
// the observed tree reachable for the later capture comparison.
func observeManagedTurnBaseline(scope execution.PreflightScope, store *taskdir.Store, request task.TaskRecord) error {
	if request.Workspace == nil {
		return nil
	}
	return recordManagedTurnBaseline(scope.Context(), store, request, workspace.Git{Runner: workspaceCommandRunner{scope: scope}})
}

func recordManagedTurnBaseline(ctx context.Context, store *taskdir.Store, request task.TaskRecord, git workspace.Git) (resultErr error) {
	if request.Workspace == nil {
		return task.ErrIdentityMismatch
	}
	resource, err := workspace.LoadResource(ctx, store, request.Workspace.ID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	reservation, subject, err := workspacescore.TurnUse(ctx, resource, request)
	if err != nil {
		return err
	}
	if _, err = resource.TurnBaseline(ctx, reservation.Sequence); err == nil {
		return fmt.Errorf("%w: turn baseline already recorded", task.ErrAlreadyStarted)
	} else if !workspace.OperationAbsent(err) {
		return err
	}
	checkout, err := workspacescore.RegisteredExecutionCheckout(ctx, resource)
	if err != nil {
		return err
	}
	if checkout.Workdir != request.CanonicalCwd {
		return task.ErrIdentityMismatch
	}
	baseline, err := recordTurnBaseline(ctx, resource, git, checkout, reservation.Sequence, subject)
	if err != nil {
		return err
	}
	return git.PinTurnBaseline(ctx, checkout, baseline)
}

// The private index lives in an invocation-owned scratch directory outside
// the checkout and state; BuildCapture removes its files before returning.
func recordTurnBaseline(ctx context.Context, resource *workspace.Resource, git workspace.Git, checkout workspace.Checkout, sequence uint64, subject workspace.Subject) (baseline workspace.TurnBaseline, resultErr error) {
	scratch, err := os.MkdirTemp("", "delegate-turn-baseline-")
	if err != nil {
		return baseline, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.Remove(scratch)) }()
	observed, err := git.BuildCapture(ctx, checkout, scratch)
	if err != nil {
		return baseline, err
	}
	request := resource.Request()
	baseline = workspace.TurnBaseline{
		SchemaVersion: task.SchemaVersion, WorkspaceID: request.WorkspaceID, Sequence: sequence, Subject: subject,
		BaseOID: observed.BaseOID, ObservedHEAD: observed.ObservedHEAD, TreeOID: observed.TreeOID,
		IndexSHA256: observed.IndexSHA256, ObjectFormat: observed.ObjectFormat,
		Ref: workspacescore.ManagedBaselineRef(request.RootID, request.WorkspaceID, sequence),
	}
	return baseline, resource.RecordTurnBaseline(ctx, baseline)
}

// observeCaptureTurn compares a task capture with its own turn's baseline.
// A missing baseline is reported unavailable, never as an unchanged turn.
func observeCaptureTurn(ctx context.Context, resource *workspace.Resource, git workspace.Git, checkout workspace.Checkout, request workspace.OperationRequest, candidate *workspace.CaptureCandidate) error {
	applies, baseline, err := workspacescore.CaptureTurnBaseline(ctx, resource, request)
	if err != nil || !applies {
		return err
	}
	if baseline != nil {
		// Re-pinning is create-only and accepts the prelaunch winner. It needs
		// the recorded tree, not the baseline HEAD commit, which a rewritten
		// history may have pruned.
		if err = git.PinTurnBaseline(ctx, checkout, *baseline); err != nil {
			return err
		}
	}
	candidate.Turn = workspace.ObserveTurn(baseline, *candidate)
	return nil
}
