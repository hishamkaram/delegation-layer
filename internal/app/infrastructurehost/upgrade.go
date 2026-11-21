package infrastructurehost

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var _ infrastructure.UpgradeHost = Host{}

// UpgradeOwnRoot is application composition over the canonical Drain/migration
// owners. The resident supplies authentication, commit guards and actual Wait
// custody. A control timeout never authorizes another daemon Start.
func (h Host) UpgradeOwnRoot(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, request infrastructure.UpgradeRequest, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (outcome infrastructure.UpgradeOutcome, resultErr error) {
	outcome.Preflight.State = infrastructure.UpgradePreflightPassed
	metadata, pin, err := openUpgradeMetadata(ctx, operation, selected, request)
	if err != nil {
		return outcome, err
	}
	defer func() { resultErr = errors.Join(resultErr, metadata.Close()) }()
	if request.Action == infrastructure.UpgradeComplete {
		observed, done, observeErr := observeUpgradeCompletion(ctx, pin, request.OperationID)
		if done || observeErr != nil {
			return upgradeOutcome(observed, observeErr)
		}
	}
	if reason, pendingErr := unlinkedMigrationPending(ctx, pin); pendingErr != nil || reason != "" {
		if pendingErr != nil {
			return outcome, pendingErr
		}
		outcome.Condition = infrastructure.UpgradePending
		outcome.ReasonCode = reason
		return outcome, nil
	}
	// Normal opening establishes durability support. This happens only after
	// the passive subject/phase check, including for metadata-born residents.
	preparation, err := h.openPreparation(ctx, operation, selected, guards, wait)
	defer func() {
		outcome.Disposition = preparation.disposition()
		resultErr = preparation.close(resultErr)
	}()
	if err != nil {
		return outcome, err
	}
	return preparation.performUpgrade(ctx, pin, request, selected)
}

func openUpgradeMetadata(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, request infrastructure.UpgradeRequest) (store *taskdir.Store, pin taskdir.RootPin, resultErr error) {
	if err := errors.Join(operation.Validate(), request.Validate()); err != nil {
		return nil, pin, err
	}
	inputs := operation.Intent().Inputs
	if err := request.Context.ValidateContext(selected, inputs.Root, inputs.RootID); err != nil {
		return nil, pin, err
	}
	if request.Action == infrastructure.UpgradeStatus {
		return nil, pin, infrastructure.ErrOwnerRequest
	}
	store, resultErr = taskdir.OpenExistingStoreMetadataContext(ctx, inputs.Root, predicate.Default())
	if resultErr != nil {
		return nil, pin, resultErr
	}
	pin, resultErr = store.PinRootContext(ctx)
	if resultErr == nil && (pin.RootID() != inputs.RootID || pin.Device() != inputs.RootDevice || pin.Inode() != inputs.RootInode) {
		resultErr = infrastructure.ErrIdentity
	}
	if resultErr != nil {
		return nil, pin, errors.Join(resultErr, store.Close())
	}
	return store, pin, nil
}

func (o *infrastructurePreparationOwner) performUpgrade(ctx context.Context, pin taskdir.RootPin, request infrastructure.UpgradeRequest, selected infrastructure.LaunchContext) (infrastructure.UpgradeOutcome, error) {
	ordinary, err := ordinaryInstalledEnsure(ctx, pin, request, selected)
	if err != nil {
		return infrastructure.UpgradeOutcome{}, err
	}
	if ordinary {
		client, prepareErr := supervisor.PreparePrivateAdmissionSupervisorContext(ctx, o.lookup, o.store, o.options)
		if client != nil && prepareErr == nil {
			o.observation.RecordReady()
		}
		return infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradeOK, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}}, supervisor.ClosePrivateAdmissionTransition(client, prepareErr)
	}
	var observed pueue.PrivateUpgradeObservation
	err = pueue.WithExistingBootstrapLockContext(ctx, o.store.Root, func(locked context.Context) error {
		var transitionErr error
		observed, transitionErr = o.upgradeLocked(locked, pin, request)
		return transitionErr
	})
	return upgradeOutcome(observed, err)
}

// Equality is only the ordinary installation selection rule. It does not prove
// executable bytes unchanged. Reloading a same-path daemon requires the
// explicit proactive operation; an existing Drain always takes precedence.
func ordinaryInstalledEnsure(ctx context.Context, pin taskdir.RootPin, request infrastructure.UpgradeRequest, selected infrastructure.LaunchContext) (bool, error) {
	if request.Action != infrastructure.UpgradeEnsure {
		return false, nil
	}
	_, pending, err := pueue.ObservePrivateUpgrade(ctx, pin, "")
	if err != nil || pending {
		return false, err
	}
	binding, found, err := pueue.ReadPrivateSupervisorBinding(pin.Root())
	if err != nil || !found {
		return false, errors.Join(pueue.ErrPrivateSupervisorNotInitialized, err)
	}
	return binding.ClientExecutable == selected.SupervisorExecutable && binding.DaemonExecutable == filepath.Join(filepath.Dir(selected.SupervisorExecutable), "pueued"), nil
}

// This fast path is read-only and does not inspect an installed executable.
func observeUpgradeCompletion(ctx context.Context, pin taskdir.RootPin, operation infrastructure.UpgradeOperationID) (pueue.PrivateUpgradeObservation, bool, error) {
	op, err := pueue.ParseUpgradeOperationID(string(operation))
	if err != nil {
		return pueue.PrivateUpgradeObservation{}, true, err
	}
	observed, found, err := pueue.ObservePrivateUpgrade(ctx, pin, op)
	if err != nil || !found {
		return observed, true, errors.Join(pueue.ErrPrivateUpgradeNotRetained, err)
	}
	if observed.Completed {
		return observed, true, nil
	}
	if observed.Phase == pueue.UpgradeDraining {
		return observed, true, &pueue.PrivateUpgradeError{ReasonCode: "drain_incomplete", Err: pueue.ErrPrivateUpgradePending}
	}
	return observed, false, nil
}

func (o *infrastructurePreparationOwner) upgradeLocked(ctx context.Context, pin taskdir.RootPin, request infrastructure.UpgradeRequest) (observed pueue.PrivateUpgradeObservation, resultErr error) {
	var err error
	if reason, pendingErr := unlinkedMigrationPending(ctx, pin); pendingErr != nil || reason != "" {
		if pendingErr != nil {
			return observed, pendingErr
		}
		return observed, &pueue.PrivateUpgradeError{ReasonCode: reason, Err: pueue.ErrPrivateUpgradePending}
	}
	if request.Action == infrastructure.UpgradeComplete {
		observed, err = pueue.AuthorizePrivateUpgradeCompletionWithLock(ctx, pin, pueue.UpgradeOperationID(request.OperationID), o.options)
	} else {
		mode := pueue.UpgradeReactive
		if request.Action == infrastructure.UpgradePrepare {
			mode = pueue.UpgradeOperationRequired
		}
		observed, err = pueue.BeginPrivateUpgradeWithLock(ctx, pin, mode, o.options)
		if err == nil {
			observed, err = pueue.AdvancePrivateUpgradeDrainWithLock(ctx, pin, observed.OperationID, o.options)
		}
	}
	if err != nil {
		return observed, err
	}
	if observed.Phase == pueue.UpgradeAwaitingInstallation {
		return observed, nil
	}
	selector := func(control context.Context) (string, string, error) {
		if control == nil {
			return "", "", os.ErrInvalid
		}
		if contextErr := control.Err(); contextErr != nil {
			return "", "", contextErr
		}
		return installation.ResolvePrivateSupervisorExecutables(o.lookup)
	}
	client, observed, err := pueue.CompletePrivateUpgradeWithLock(ctx, pin, observed.OperationID, selector, o.options)
	if client != nil && err == nil && observed.Completed {
		err = supervisor.EnsureOrdinarySchedulingWithBootstrapLock(ctx, o.store.Root, client)
		if err == nil {
			o.observation.RecordReady()
		}
	}
	return observed, supervisor.ClosePrivateAdmissionTransition(client, err)
}

func unlinkedMigrationPending(ctx context.Context, pin taskdir.RootPin) (string, error) {
	_, upgrade, err := pueue.ObservePrivateUpgrade(ctx, pin, "")
	if err != nil || upgrade {
		return "", err
	}
	phase, found, err := pueue.ObservePrivateMigrationPhase(pin.Root())
	if err != nil || !found {
		return "", err
	}
	switch phase {
	case pueue.PrivateMigrationAccepted:
		return "ordinary_migration_accepted_pending", nil
	case pueue.PrivateMigrationPredecessorShutdownRequested:
		return "ordinary_migration_predecessor_shutdown_requested_pending", nil
	case pueue.PrivateMigrationPredecessorShutdownDispatched:
		return "ordinary_migration_predecessor_shutdown_dispatched_pending", nil
	case pueue.PrivateMigrationPredecessorStopped:
		return "ordinary_migration_predecessor_stopped_pending", nil
	default:
		return "", pueue.ErrBinding
	}
}

func upgradeOutcome(observed pueue.PrivateUpgradeObservation, cause error) (infrastructure.UpgradeOutcome, error) {
	outcome := infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradeOK, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}}
	if observed.OperationID != "" {
		outcome.Report = infrastructure.UpgradeReport{
			OperationID: infrastructure.UpgradeOperationID(observed.OperationID), Mode: infrastructure.UpgradeMode(observed.Mode), Phase: infrastructure.UpgradePhase(observed.Phase), Predecessor: observed.Predecessor, Completed: observed.Completed,
		}
	}
	var progress *pueue.PrivateUpgradeError
	if errors.As(cause, &progress) {
		outcome.ReasonCode = progress.ReasonCode
	}
	if cause == nil {
		return outcome, outcome.Validate()
	}
	if infrastructureRoutingRetainsCleanup(cause) {
		outcome.Condition = infrastructure.UpgradeFault
		return outcome, errors.Join(cause, outcome.Validate())
	}
	if supervisor.MaintenanceBusy(cause) || pureUpgradePending(cause) || pureDaemonWaitPending(cause) {
		outcome.Condition = infrastructure.UpgradePending
		return outcome, outcome.Validate()
	}
	if task.AllErrorCauses(cause, func(err error) bool {
		return errors.Is(err, pueue.ErrPrivateUpgradeOperation) || errors.Is(err, pueue.ErrPrivateUpgradeNotRetained)
	}) {
		outcome.Condition = infrastructure.UpgradeRefused
		outcome.ReasonCode = "upgrade_operation_mismatch"
		return outcome, outcome.Validate()
	}
	outcome.Condition = infrastructure.UpgradeFault
	return outcome, errors.Join(cause, outcome.Validate())
}

func pureUpgradePending(cause error) bool {
	return errors.Is(cause, pueue.ErrPrivateUpgradePending) && !taskdir.CleanupFault(cause) && task.AllErrorCauses(cause, func(err error) bool {
		return errors.Is(err, pueue.ErrPrivateUpgradePending) || errors.Is(err, pueue.ErrUnknown)
	})
}

// Infrastructure alone marks a validated live historical custodian or an
// actual outstanding current Wait. Neither marker grants closure or Start.
type historicalDaemonWaitPending interface {
	HistoricalDaemonWaitPending()
}

type rootDaemonWaitQuiescencePending interface {
	RootDaemonWaitQuiescencePending()
}

func pureDaemonWaitPending(cause error) bool {
	return !taskdir.CleanupFault(cause) && daemonWaitPendingBranch(cause)
}

func daemonWaitPendingBranch(cause error) bool {
	if cause == nil {
		return false
	}
	if _, marked := cause.(historicalDaemonWaitPending); marked {
		return singleOwnerPendingCause(errors.Unwrap(cause))
	}
	if _, marked := cause.(rootDaemonWaitQuiescencePending); marked {
		return singleOwnerPendingCause(errors.Unwrap(cause))
	}
	if joined, ok := cause.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			// Each independent branch needs its own origin. An untagged
			// pending or migration-required fault cannot borrow a sibling tag.
			if !daemonWaitPendingBranch(child) {
				return false
			}
		}
		return true
	}
	return daemonWaitPendingBranch(errors.Unwrap(cause))
}

func singleOwnerPendingCause(cause error) bool {
	if cause == nil {
		return false
	}
	// The origin wraps one cause. A marked joined graph could otherwise lend
	// the tag to an independent, untagged branch with the same sentinel leaf.
	if _, joined := cause.(interface{ Unwrap() []error }); joined {
		return false
	}
	if child := errors.Unwrap(cause); child != nil {
		return singleOwnerPendingCause(child)
	}
	return errors.Is(cause, infrastructure.ErrOwnerPending)
}
