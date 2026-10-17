package guardian

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (g *Guardian) prepareRunScope(ctx context.Context) (string, Scope, error) {
	bootID, err := g.options.BootID()
	if err != nil {
		return "", nil, fmt.Errorf("read lifetime boot identity: %w", err)
	}
	if bootID == "" {
		return "", nil, fmt.Errorf("%w: empty lifetime boot identity", errLifetimeGuardianInvalid)
	}
	plan, err := g.options.PlanFactory(g.options.RootID, g.options.OwnerKind, g.options.TaskID)
	if err != nil {
		return "", nil, fmt.Errorf("plan lifetime scope: %w", err)
	}
	if err = g.recordIntent(ctx, plan, bootID); err != nil {
		return "", nil, err
	}
	scope, err := g.options.ScopeFactory(plan)
	if errors.Is(err, lifetime.ErrScopeExists) {
		recoveryCtx, cancel := g.recoveryContext(ctx)
		recoveryErr := g.options.ReconcileOwned(recoveryCtx)
		cancel()
		if recoveryErr != nil {
			return "", nil, fmt.Errorf("reconcile existing lifetime scope: %w", recoveryErr)
		}
		if terminalErr := g.refuseTerminalReplay(ctx); terminalErr != nil {
			return "", nil, terminalErr
		}
		scope, err = g.options.ScopeFactory(plan)
	}
	if err != nil {
		return "", nil, fmt.Errorf("create lifetime scope: %w", err)
	}
	if scope == nil {
		return "", nil, fmt.Errorf("%w: scope factory returned nil scope", errLifetimeGuardianInvalid)
	}
	return bootID, scope, nil
}

func (g *Guardian) bindAndRecordRunScope(ctx context.Context, bootID string) error {
	if err := g.scope.BindOwnerProcess(); err != nil {
		cleanupCtx, cancel := g.recoveryContext(ctx)
		cleanupErr := g.resolveCreatedScopeFailure(cleanupCtx, bootID)
		cancel()
		return errors.Join(fmt.Errorf("bind lifetime scope owner: %w", err), cleanupErr)
	}
	if err := g.recordRegistration(ctx, bootID); err != nil {
		cleanupCtx, cancel := g.recoveryContext(ctx)
		cleanupErr := g.resolveCreatedScopeFailure(cleanupCtx, bootID)
		cancel()
		return errors.Join(err, cleanupErr)
	}
	return nil
}

// resolveCreatedScopeFailure is the sole setup-failure resolver. Its caller
// retains the lifetime-owner lease. A scope is destroyed without a terminal
// record only after a fresh registration read proves that this exact scope was
// never published.
func (g *Guardian) resolveCreatedScopeFailure(ctx context.Context, bootID string) error {
	registration, err := g.options.Records.ReadLifetimeRegistrationContext(ctx)
	if task.RecordAbsent(err) {
		return g.disposeUnregisteredScope(ctx)
	}
	if err != nil {
		return fmt.Errorf("read lifetime registration after write failure: %w", err)
	}
	if !g.registrationMatchesScope(registration, bootID) {
		disposeErr := g.disposeUnregisteredScope(ctx)
		return errors.Join(task.ErrIdentityMismatch, disposeErr)
	}
	if err = g.recordRegistration(ctx, bootID); err != nil {
		return fmt.Errorf("acknowledge lifetime registration after setup failure: %w", err)
	}
	return g.terminateAndRecord(ctx, task.LifetimeTerminationRecovery)
}

func (g *Guardian) disposeUnregisteredScope(ctx context.Context) error {
	err := disposeUnregisteredLifetimeScope(ctx, g.scope, g.scope.Reference().Assurance())
	if err == nil {
		g.unregisteredDisposed = true
		g.scopeDestroyed = true
	}
	return err
}

func (g *Guardian) registrationMatchesScope(registration *task.LifetimeRegistrationRecord, bootID string) bool {
	if registration == nil || g.scope == nil {
		return false
	}
	ref := g.scope.Reference()
	return registration.RootID == g.options.RootID &&
		registration.TaskID == g.options.TaskID &&
		registration.OwnerKind == g.options.OwnerKind &&
		registration.ScopeKind == string(ref.Kind()) &&
		registration.Assurance == string(ref.Assurance()) &&
		registration.ScopeID == ref.Identifier() &&
		registration.BootID == bootID &&
		registration.Deadline == g.options.Deadline.UTC().Format(time.RFC3339Nano)
}

func (g *Guardian) recordIntent(ctx context.Context, plan lifetime.Plan, bootID string) error {
	record, err := g.options.Records.RecordLifetimeIntentContext(ctx, taskdir.LifetimeIntentFacts{
		ScopeKind: string(plan.Kind()),
		Assurance: string(plan.Assurance()),
		ScopePlan: plan.Identifier(),
		BootID:    bootID,
		Deadline:  g.options.Deadline,
	})
	if err != nil {
		return fmt.Errorf("record lifetime intent: %w", err)
	}
	if record == nil || record.TaskID != g.options.TaskID || record.ScopePlan != plan.Identifier() {
		return fmt.Errorf("%w: intent writer returned invalid record", errLifetimeGuardianInvalid)
	}
	return nil
}

func (g *Guardian) recordRegistration(ctx context.Context, bootID string) error {
	ref := g.scope.Reference()
	record, err := g.options.Records.RecordLifetimeRegistrationContext(ctx, taskdir.LifetimeRegistrationFacts{
		ScopeKind: string(ref.Kind()),
		Assurance: string(ref.Assurance()),
		ScopeID:   ref.Identifier(),
		BootID:    bootID,
		Deadline:  g.options.Deadline,
	})
	if err != nil {
		return fmt.Errorf("record lifetime registration: %w", err)
	}
	if !g.registrationMatchesScope(record, bootID) {
		return fmt.Errorf("%w: registration writer returned invalid record", errLifetimeGuardianInvalid)
	}
	g.registrationRecorded = true
	return nil
}
