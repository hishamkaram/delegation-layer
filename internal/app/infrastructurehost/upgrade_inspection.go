package infrastructurehost

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (o *infrastructurePreparationOwner) completeUpgradeInspection(ctx context.Context, pin taskdir.RootPin, operation pueue.UpgradeOperationID) (resultErr error) {
	client, eligible, err := pueue.ProbePrivateUpgradeInspectionWithLock(ctx, pin, operation, o.selectUpgradePair, o.options)
	if err != nil || !eligible {
		return err // Historical A/current B is never a mutation candidate.
	}
	if client == nil {
		return &pueue.PrivateUpgradeError{ReasonCode: "inspection_edge_pending", Err: pueue.ErrPrivateUpgradePending}
	}
	defer func() { resultErr = supervisor.ClosePrivateAdmissionTransition(client, resultErr) }()
	if err := supervisor.CompleteUpgradeInspectionWithLock(ctx, o.store, pin, operation, client, o.options.PrivateGenerationGuard); err != nil {
		return err
	}
	o.observation.RecordReady()
	return nil
}

// Ordinary Ensure retains its existing preparation/start authority. Only after
// that owner succeeds does derived cleanup probe again under fresh EX.
func (o *infrastructurePreparationOwner) prepareOrdinaryUpgradeInspection(ctx context.Context, pin taskdir.RootPin) (infrastructure.UpgradeOutcome, error) {
	client, err := supervisor.PreparePrivateAdmissionSupervisorContext(ctx, o.lookup, o.store, o.options)
	if err == nil && client != nil {
		var due bool
		due, err = supervisor.UpgradeInspectionCleanupDue(ctx, o.store, "")
		if due && err == nil {
			err = pueue.WithExistingBootstrapLockContext(ctx, o.store.Root, func(locked context.Context) error {
				return o.completeUpgradeInspection(locked, pin, "")
			})
		} else if err == nil {
			o.observation.RecordReady()
		}
	}
	return upgradeOutcome(pueue.PrivateUpgradeObservation{}, supervisor.ClosePrivateAdmissionTransition(client, err))
}

// ReadyCurrent is a distinct, exact-A current-admission request. Historical
// Complete never reaches this path. The first fence uses metadata only; even
// the normal Store's durability probe happens after currency is established.
func (h Host) readyCurrentUpgrade(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, request infrastructure.UpgradeRequest, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner, pin taskdir.RootPin) (outcome infrastructure.UpgradeOutcome, resultErr error) {
	origin, err := infrastructure.NewOrigin(pin.Root(), pin.RootID())
	if err != nil {
		return outcome, err
	}
	options, _, err := infrastructurePreparationOptions(origin, operation.Artifacts(), selected, guards, wait, h.callbacks)
	if err != nil {
		return outcome, err
	}
	lookup := installation.ExecutableLookup{InitialExecutable: selected.SupervisorExecutable, Environment: []string{}, AcceptedBootstrapPair: installation.BootstrapPair{Client: selected.SupervisorExecutable, Daemon: filepath.Join(filepath.Dir(selected.SupervisorExecutable), "pueued")}}
	selector := func(context.Context) (string, string, error) {
		return installation.ResolvePrivateSupervisorExecutables(lookup)
	}
	var owner *infrastructurePreparationOwner
	defer func() {
		if owner != nil {
			outcome.Disposition = owner.disposition()
			resultErr = owner.close(resultErr)
		}
	}()
	var observed pueue.PrivateUpgradeObservation
	acquired, err := pueue.TryWithPrivateTransition(ctx, pin.Root(), func() error {
		return pueue.WithExistingBootstrapLockContext(ctx, pin.Root(), func(locked context.Context) error {
			var fenceErr error
			observed, fenceErr = pueue.CheckPrivateUpgradeReadyCurrentWithLock(locked, pin, pueue.UpgradeOperationID(request.OperationID), selector, options)
			if fenceErr != nil {
				return fenceErr
			}
			owner, fenceErr = h.openPreparation(locked, operation, selected, guards, wait)
			if fenceErr != nil {
				return fenceErr
			}
			return owner.prepareReadyCurrentLocked(locked, pin, observed)
		})
	})
	if !acquired && err == nil {
		err = &pueue.PrivateUpgradeError{ReasonCode: "current_admission_busy", Err: pueue.ErrPrivateUpgradePending}
	}
	return upgradeOutcome(observed, err)
}

func (o *infrastructurePreparationOwner) prepareReadyCurrentLocked(ctx context.Context, pin taskdir.RootPin, expected pueue.PrivateUpgradeObservation) (resultErr error) {
	client, err := supervisor.PreparePrivateAdmissionTransition(ctx, o.lookup, o.store, o.options)
	defer func() { resultErr = supervisor.ClosePrivateAdmissionTransition(client, resultErr) }()
	if err != nil {
		return err
	}
	if client == nil {
		return task.ErrEvidenceFault
	}
	observed, err := pueue.CheckPrivateUpgradeReadyCurrentWithLock(ctx, pin, expected.OperationID, o.selectUpgradePair, o.options)
	if err != nil || observed != expected || client.Binding() != expected.Target {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	if err = supervisor.CompleteUpgradeInspectionWithLock(ctx, o.store, pin, expected.OperationID, client, o.options.PrivateGenerationGuard); err != nil {
		return err
	}
	o.observation.RecordReady()
	return nil
}
