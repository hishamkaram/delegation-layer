package infrastructurehost

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var _ supervisor.UpgradeRouting = Routing{}

const UpgradeOwnerRefreshSuccessorStartAccepted = "owner_refresh_successor_start_accepted"

// UpgradeOwn stages one context and performs one authenticated v5 exchange.
// Any helper replacement precedes v5 contact and requires positive legacy
// authentication; an EOF or timeout from v5 never grants fallback authority.
func (r Routing) UpgradeOwn(ctx context.Context, target *taskdir.Store, selected infrastructure.LaunchContext, request infrastructure.UpgradeRequest, refresh bool) (outcome infrastructure.UpgradeOutcome, resultErr error) {
	if err := r.validateUpgradeInvocation(ctx, target); err != nil {
		return outcome, err
	}
	pin, err := target.PinRootContext(ctx)
	if err != nil {
		return outcome, err
	}
	if request.Action == infrastructure.UpgradeComplete {
		observed, done, observeErr := observeUpgradeCompletion(ctx, pin, request.OperationID)
		if done || observeErr != nil {
			return upgradeOutcome(observed, observeErr)
		}
	}
	selection, host, err := r.selectPreparation(ctx, pin, selected, infrastructure.InitialOperation{Kind: infrastructure.InitialUpgradePreflight})
	if err != nil {
		return outcome, err
	}
	// All executable/context validation precedes serving release or staging.
	currentRunner, err := host.PrepareOutsideRunner(ctx, target)
	if err != nil {
		return outcome, err
	}
	resources := &infrastructureRoutingResources{target: target}
	defer func() { resultErr = resources.finishPreparation(resultErr, selection.probeErr) }()
	resources.reader, err = taskdir.OpenReadOnlyStoreContext(ctx, target.Root, predicate.Default())
	if err != nil {
		return outcome, err
	}
	if err = requireInfrastructureRoutingPin(ctx, resources.reader, pin); err != nil {
		return outcome, err
	}
	owner, refresh, err := observeUpgradeOwner(ctx, resources.reader, currentRunner, refresh)
	selection.owner, selection.probeErr = owner.owner, owner.probeErr
	if err != nil {
		return outcome, err
	}
	selection.reference, err = infrastructure.StageLaunchContext(ctx, target, selected)
	if err != nil {
		return outcome, err
	}
	request.Context = selection.reference
	contact := infrastructurePreparationContact{routing: r, resources: resources, selection: selection}
	return contact.requestUpgrade(ctx, pin, request, refresh)
}

func (c infrastructurePreparationContact) requestUpgrade(ctx context.Context, pin taskdir.RootPin, request infrastructure.UpgradeRequest, refresh bool) (infrastructure.UpgradeOutcome, error) {
	owner := c.selection.owner
	if owner != (infrastructure.StartupReference{}) && refresh {
		var operation infrastructure.UpgradeOperationID
		if request.Action == infrastructure.UpgradeComplete {
			operation = request.OperationID
		}
		observed, terminal, releaseErr := releaseUpgradeOwnerRefreshServing(ctx, c.resources.reader, pin, operation, owner)
		if terminal {
			return upgradeOutcome(observed, releaseErr)
		}
		if releaseErr != nil {
			outcome := infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradeBusy, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPending}, ReasonCode: "owner_refresh_busy"}
			return normalizeUpgradeContact(outcome, releaseErr)
		}
		owner = infrastructure.StartupReference{}
	}
	established := infrastructure.EstablishedOwner{Owner: owner}
	if owner == (infrastructure.StartupReference{}) {
		var err error
		established, err = c.EstablishReplacement(ctx)
		if err != nil {
			return infrastructure.UpgradeOutcome{}, err
		}
	}
	if err := established.Validate(); err != nil {
		return infrastructure.UpgradeOutcome{}, err
	}
	if request.Action == infrastructure.UpgradeStatus {
		preflight, err := infrastructure.ProbeBoundUpgrade(ctx, c.resources.reader, established.Owner)
		condition := infrastructure.UpgradeOK
		if preflight.State == infrastructure.UpgradePreflightPending {
			condition = infrastructure.UpgradePending
		}
		return normalizeUpgradeContact(infrastructure.UpgradeOutcome{Condition: condition, Preflight: preflight}, err)
	}
	outcome, err := infrastructure.RequestBoundUpgrade(ctx, c.resources.reader, established.Owner, request)
	return normalizeUpgradeContact(outcome, err)
}

// Retirement policy stays outside canonical control. Eligibility and serving
// release share bootstrap EX; establishment and v5 contact happen after unlock.
// An exact completed operation replays before inspecting any current Start.
func releaseUpgradeOwnerRefreshServing(ctx context.Context, reader *taskdir.ReadOnlyStore, pin taskdir.RootPin, operation infrastructure.UpgradeOperationID, owner infrastructure.StartupReference) (observed pueue.PrivateUpgradeObservation, terminal bool, resultErr error) {
	terminal = true
	resultErr = pueue.WithExistingBootstrapLockContext(ctx, reader.Root(), func(locked context.Context) error {
		if err := requireInfrastructureRoutingPin(locked, reader, pin); err != nil {
			return err
		}
		if operation != "" {
			var done bool
			var err error
			observed, done, err = observeUpgradeCompletion(locked, pin, operation)
			if done || err != nil {
				return err
			}
		}
		var err error
		observed, err = observeUpgradeOwnerRefreshEligibilityWithLock(locked, pin, operation)
		if err != nil {
			return err
		}
		terminal = false
		return releaseUpgradeServing(locked, reader, owner)
	})
	return observed, terminal, resultErr
}

// Canonical control owns accepted Start, including unknown birth. Apphost owns
// retirement policy; both refresh callers already hold the actual bootstrap EX.
func observeUpgradeOwnerRefreshEligibilityWithLock(ctx context.Context, pin taskdir.RootPin, operation infrastructure.UpgradeOperationID) (pueue.PrivateUpgradeObservation, error) {
	observed, accepted, err := pueue.ObservePrivateUpgradeAcceptedStartWithLock(ctx, pin, pueue.UpgradeOperationID(operation))
	if err != nil {
		return observed, err
	}
	if accepted {
		return observed, &pueue.PrivateUpgradeError{ReasonCode: UpgradeOwnerRefreshSuccessorStartAccepted, Err: pueue.ErrPrivateUpgradePending}
	}
	return observed, nil
}

// Only an authenticated typed response and its identical pending marker may
// be normalized. Transport, cleanup, cancellation and independent faults retain
// their complete graph, even alongside a valid pending observation.
func normalizeUpgradeContact(outcome infrastructure.UpgradeOutcome, cause error) (infrastructure.UpgradeOutcome, error) {
	if cause == nil {
		return outcome, nil
	}
	marker := infrastructure.ErrOwnerPending
	if outcome.Condition == infrastructure.UpgradeBusy {
		marker = infrastructure.ErrOwnerBusy
	}
	if (outcome.Condition == infrastructure.UpgradePending || outcome.Condition == infrastructure.UpgradeBusy) && !infrastructureRoutingRetainsCleanup(cause) && !taskdir.CleanupFault(cause) && task.AllErrorCauses(cause, func(err error) bool { return errors.Is(err, marker) }) {
		return outcome, nil
	}
	return outcome, cause
}

func upgradeOwnerRefreshRequired(ctx context.Context, reader *taskdir.ReadOnlyStore, owner infrastructure.StartupReference, currentRunner string, explicit bool) (bool, error) {
	artifacts, err := infrastructure.ReadOwnerArtifacts(ctx, reader, owner)
	if err != nil {
		return false, err
	}
	selected, err := installation.ResolveRunner(artifacts.Runner, "")
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return explicit || selected != currentRunner, nil
}

func (r Routing) validateUpgradeInvocation(ctx context.Context, target *taskdir.Store) error {
	if target == nil {
		return os.ErrInvalid
	}
	return r.invocation.ValidateUpgrade(ctx)
}

func observeUpgradeOwner(ctx context.Context, reader *taskdir.ReadOnlyStore, currentRunner string, refresh bool) (infrastructureOwnerObservation, bool, error) {
	owner, probeErr := infrastructure.ProbeOwner(ctx, reader)
	if probeErr != nil {
		if refresh || infrastructureRoutingRetainsCleanup(probeErr) {
			return infrastructureOwnerObservation{}, false, probeErr
		}
		return infrastructureOwnerObservation{probeErr: probeErr}, false, nil
	}
	required, err := upgradeOwnerRefreshRequired(ctx, reader, owner, currentRunner, refresh)
	return infrastructureOwnerObservation{owner: owner}, required, err
}

func releaseUpgradeServing(ctx context.Context, reader *taskdir.ReadOnlyStore, owner infrastructure.StartupReference) error {
	released, err := infrastructure.StopOwnerServingAt(ctx, reader, owner)
	if err != nil {
		return err
	}
	if released.Startup != owner {
		return infrastructure.ErrIdentity
	}
	if released.State != infrastructure.OwnerServingReleased {
		return infrastructure.ErrOwnerBusy
	}
	return nil
}
