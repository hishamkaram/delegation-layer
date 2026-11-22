package control

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// PrivateUpgradeInspectionTargetWithLock derives eligibility from the exact
// completed record, never from a transitive successor. Historical A with a
// different current target or a current Drain is a read-only non-candidate.
// An empty operation selects ordinary current-generation maintenance only.
func PrivateUpgradeInspectionTargetWithLock(ctx context.Context, root taskdir.RootPin, operation UpgradeOperationID) (task.SupervisorRef, bool, error) {
	if err := requirePrivateMaintenanceLease(ctx, root.Root()); err != nil {
		return task.SupervisorRef{}, false, err
	}
	if err := checkPrivateUpgradeRoot(ctx, root); err != nil {
		return task.SupervisorRef{}, false, err
	}
	if operation != "" {
		if _, err := ParseUpgradeOperationID(string(operation)); err != nil {
			return task.SupervisorRef{}, false, err
		}
	}
	_, active, err := readPrivateUpgrade(root, "")
	if err != nil || active {
		return task.SupervisorRef{}, false, err
	}
	published, found, err := loadPrivateSupervisorBinding(root.Root())
	if err != nil || !found {
		return task.SupervisorRef{}, false, err
	}
	current, err := inspectionUpgradeOperationCurrent(ctx, root, operation, published)
	if err != nil || !current {
		return task.SupervisorRef{}, false, err
	}
	facts, err := loadPrivateGenerationFacts(root.Root())
	if err != nil {
		return task.SupervisorRef{}, false, err
	}
	ready, err := inspectionGenerationReady(facts, published)
	if err != nil || !ready {
		return task.SupervisorRef{}, false, err
	}
	return published, true, ctx.Err()
}

func inspectionGenerationReady(facts privateGenerationFacts, published task.SupervisorRef) (bool, error) {
	// Canonical complete() may short-circuit on a valid alias or pending phase.
	// Independent read/validation faults must survive that noncandidate result.
	if err := privateInspectionReadFaults(facts); err != nil {
		return false, err
	}
	if facts.runtimeRestartPending || facts.aliasPublishing {
		return false, nil
	}
	return facts.complete(published, false)
}

// The canonical writer appends every completed native transition. A later
// ordinary transition also prevents an earlier upgrade from lending authority.
func inspectionUpgradeOperationCurrent(ctx context.Context, root taskdir.RootPin, operation UpgradeOperationID, published task.SupervisorRef) (bool, error) {
	if operation == "" {
		return true, nil
	}
	observed, retained, err := ObservePrivateUpgrade(ctx, root, operation)
	if err != nil || !retained || !observed.Completed {
		return false, errors.Join(ErrPrivateUpgradeNotRetained, err)
	}
	if observed.Target != published {
		return false, nil
	}
	latest, err := latestInspectionUpgrade(root.Root())
	return err == nil && latest.UpgradeGeneration.OperationID == operation, err
}

// ProbePrivateUpgradeInspectionWithLock never prepares or starts a daemon.
// The selected pair and installed callback must still be the exact completed
// target. A nil eligible client means the existing endpoint is not ready.
func ProbePrivateUpgradeInspectionWithLock(ctx context.Context, root taskdir.RootPin, operation UpgradeOperationID, selectPair CompatiblePairSelector, options Options) (*Client, bool, error) {
	if err := requirePrivateUpgradeAuthority(ctx, root, options); err != nil {
		return nil, false, err
	}
	target, eligible, err := PrivateUpgradeInspectionTargetWithLock(ctx, root, operation)
	if err != nil || !eligible {
		return nil, eligible, err
	}
	if err = selectedInspectionTarget(ctx, root.Root(), target, selectPair, options); err != nil {
		return nil, false, err
	}
	client, err := ProbeSavedPrivateMaintenanceWithLock(ctx, root.Root(), target, options, selectPair)
	if client != nil && client.Binding() != target {
		err = errors.Join(ErrBinding, err)
		return nil, true, errors.Join(err, client.CloseAfterPending(err))
	}
	if client != nil && err == nil && operation != "" {
		latest, readErr := latestInspectionUpgrade(root.Root())
		err = readErr
		if err == nil {
			err = client.verifyPrivateUpgradeSuccessor(ctx, root.Root(), latest)
		}
		if err != nil {
			return nil, true, errors.Join(err, client.CloseAfterPending(err))
		}
	}
	return client, true, err
}

func latestInspectionUpgrade(root string) (privateMigrationRecord, error) {
	history, found, err := loadPrivateMigrationHistory(filepath.Join(root, privateSupervisorDirectory))
	if err != nil || !found || len(history.Migrations) == 0 {
		return privateMigrationRecord{}, errors.Join(ErrBinding, err)
	}
	// The canonical writer appends completed transitions, including same-target
	// births. The final immutable record is the current completed native edge.
	return history.Migrations[len(history.Migrations)-1], nil
}

func selectedInspectionTarget(ctx context.Context, root string, target task.SupervisorRef, selectPair CompatiblePairSelector, options Options) error {
	if selectPair == nil {
		return ErrConfiguration
	}
	clientPath, daemonPath, err := selectPair(ctx)
	if err != nil {
		return err
	}
	if clientPath != target.ClientExecutable || daemonPath != target.DaemonExecutable {
		return ErrBinding
	}
	if options.PrivateTerminalEventRunner == "" {
		return nil
	}
	data, err := readRegular(target.ConfigPath, MaxControlBytes)
	if err != nil {
		return err
	}
	_, changed, err := privateInstalledCallbackConfig(data, filepath.Join(root, privateSupervisorDirectory), options.PrivateTerminalEventRunner)
	if err != nil || changed {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

// CheckPrivateUpgradeReadyCurrentWithLock is the pre-effect fence for the
// explicit current-admission intent. No publication replay or preparation may
// precede it. A's completed process birth is history; ordinary restart owns the
// current runtime's Start/Wait after this exact target/currency check.
func CheckPrivateUpgradeReadyCurrentWithLock(ctx context.Context, root taskdir.RootPin, operation UpgradeOperationID, selectPair CompatiblePairSelector, options Options) (observed PrivateUpgradeObservation, resultErr error) {
	if err := requirePrivateUpgradeAuthority(ctx, root, options); err != nil {
		return observed, err
	}
	var found bool
	observed, found, resultErr = ObservePrivateUpgrade(ctx, root, operation)
	if resultErr != nil || !found || !observed.Completed || operation == "" {
		return PrivateUpgradeObservation{}, errors.Join(ErrPrivateUpgradeOperation, resultErr)
	}
	latest, historyErr := latestInspectionUpgrade(root.Root())
	facts, factsErr := loadPrivateGenerationFacts(root.Root())
	bootstrap, bootstrapFound, bootstrapErr := readPrivateBootstrapRecord(root.Root())
	publication, publishing, publicationErr := readPrivateIdentityPublication(root.Root())
	if err := errors.Join(historyErr, factsErr, bootstrapErr, publicationErr, privateInspectionReadFaults(facts)); err != nil {
		return observed, err
	}
	if err := checkReadyCurrentState(facts, bootstrap, bootstrapFound, publication, publishing, observed, latest); err != nil {
		return observed, err
	}
	if err := selectedInspectionTarget(ctx, root.Root(), observed.Target, selectPair, options); err != nil {
		return observed, err
	}
	client, err := prepareExactPublishedPair(ctx, observed.Target, options)
	if err != nil {
		return observed, err
	}
	return observed, errors.Join(checkPrivateUpgradeRoot(ctx, root), client.Close())
}

func checkReadyCurrentState(facts privateGenerationFacts, bootstrap privateBootstrapRecord, bootstrapFound bool, publication privateIdentityPublicationRecord, publishing bool, observed PrivateUpgradeObservation, latest privateMigrationRecord) error {
	if latest.UpgradeGeneration.OperationID != observed.OperationID || readyCurrentCompetingAuthority(facts, bootstrap, bootstrapFound) {
		return ErrPrivateUpgradeOperation
	}
	if !facts.publishedFound || facts.published != observed.Target || publishing && publication.Binding != observed.Target {
		return ErrBinding
	}
	if !publishing {
		complete, err := facts.complete(observed.Target, false)
		if err != nil || !complete {
			return errors.Join(ErrBinding, err)
		}
	}
	return nil
}

func readyCurrentCompetingAuthority(facts privateGenerationFacts, bootstrap privateBootstrapRecord, bootstrapFound bool) bool {
	return facts.drain != nil || facts.runtimeRestartPending || facts.migrationPresent || facts.aliasPresent || facts.aliasPublishing || bootstrapFound && bootstrap.RuntimeRestart == nil
}

func privateInspectionReadFaults(facts privateGenerationFacts) error {
	return errors.Join(facts.publishedErr, facts.identityErr, facts.migrationErr, facts.aliasErr, facts.aliasPublicationErr, facts.transitionErr, facts.identityPublicationReadErr)
}
