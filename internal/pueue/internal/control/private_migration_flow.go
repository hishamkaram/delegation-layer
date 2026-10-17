package control

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// MigratePrivateSupervisorWithLock reconciles a changed private daemon launch
// identity while the caller holds the state-root bootstrap lock. It preserves
// the queue and config, refuses to stop active work, and only clears the
// durable legacy alias after the current daemon is ready and its identity has
// been published.
func (c *Client) MigratePrivateSupervisorWithLock(ctx context.Context, stateRoot string) (resultErr error) {
	if err := validatePrivateMigrationClient(c, stateRoot); err != nil {
		return err
	}
	if _, err := recoverPrivateIdentityPublication(stateRoot); err != nil {
		return fmt.Errorf("%w: recover private identity publication: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	base := filepath.Dir(c.binding.ConfigPath)
	identity, found, pending, err := privateMigrationState(base)
	if err != nil {
		return fmt.Errorf("%w: read private supervisor migration state: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	persistedBinding, bindingFound, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		return fmt.Errorf("%w: read private supervisor logical binding: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	current := privateDaemonIdentity{Executable: c.runtimeBinding.DaemonExecutable, SHA256: c.runtimeBinding.DaemonSHA256}
	if err = validatePrivateDaemonIdentity(current); err != nil {
		return fmt.Errorf("%w: current private daemon identity is unavailable: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	currentMigrationIdentity := privateDaemonIdentityForBinding(c.binding)
	if pending != nil {
		if !bindingFound {
			persistedBinding, err = c.resolveMissingMigrationBinding(ctx, stateRoot, identity)
			if err != nil {
				return err
			}
			if err = publishPrivateSupervisorBindingIfAbsentContext(ctx, stateRoot, persistedBinding); err != nil {
				return fmt.Errorf("%w: persist recovered private supervisor binding: %w", ErrPrivateSupervisorMigrationRequired, err)
			}
			bindingFound = true
		}
		return c.resumePrivateMigration(ctx, stateRoot, base, persistedBinding, bindingFound, identity, found, current, currentMigrationIdentity, *pending)
	}
	if !found {
		return c.retryIncompletePrivateBootstrap(ctx, stateRoot)
	}
	if !bindingFound {
		persistedBinding, err = c.resolveMissingMigrationBinding(ctx, stateRoot, identity)
		if err != nil {
			return err
		}
		if err = publishPrivateSupervisorBindingIfAbsentContext(ctx, stateRoot, persistedBinding); err != nil {
			return fmt.Errorf("%w: persist recovered private supervisor binding: %w", ErrPrivateSupervisorMigrationRequired, err)
		}
	}
	return c.reconcilePrivateSupervisor(ctx, stateRoot, persistedBinding, identity, current, currentMigrationIdentity)
}

func validatePrivateMigrationClient(c *Client, stateRoot string) error {
	if c == nil || !IsPrivateConfig(stateRoot, c.binding.ConfigPath) {
		return ErrConfiguration
	}
	return c.verifyBinding()
}

func (c *Client) retryIncompletePrivateBootstrap(ctx context.Context, stateRoot string) error {
	bootstrapPending, bootstrapErr := PrivateBootstrapPending(stateRoot)
	if bootstrapErr != nil {
		return fmt.Errorf("%w: read private bootstrap state: %w", ErrPrivateSupervisorMigrationRequired, bootstrapErr)
	}
	if !bootstrapPending {
		return fmt.Errorf("%w: private supervisor identity is unavailable for retry", ErrPrivateSupervisorMigrationRequired)
	}
	recovery, recoveryErr := PrivateRecoveryBindingPresent(stateRoot)
	if recoveryErr != nil {
		return fmt.Errorf("%w: read private supervisor recovery state: %w", ErrPrivateSupervisorMigrationRequired, recoveryErr)
	}
	if recovery {
		return fmt.Errorf("%w: private supervisor recovery predecessor is unavailable", ErrPrivateSupervisorMigrationRequired)
	}
	if startErr := c.StartPrivate(ctx, stateRoot); startErr != nil {
		return fmt.Errorf("%w: retry incomplete private supervisor bootstrap: %w", ErrPrivateSupervisorMigrationRequired, startErr)
	}
	return nil
}

func (c *Client) resolveMissingMigrationBinding(ctx context.Context, stateRoot string, identity privateDaemonIdentity) (task.SupervisorRef, error) {
	if recovery, found, err := ReadPrivateRecoveryBinding(stateRoot); err != nil {
		return task.SupervisorRef{}, fmt.Errorf("%w: read private supervisor recovery binding: %w", ErrPrivateSupervisorMigrationRequired, err)
	} else if found {
		if !privateDaemonIdentityMatchesBinding(identity, recovery) {
			return task.SupervisorRef{}, fmt.Errorf("%w: persisted private supervisor recovery binding does not own the live daemon", ErrPrivateSupervisorMigrationRequired)
		}
		return recovery, nil
	}
	if !privateLogicalIdentityPresent(identity) {
		return task.SupervisorRef{}, fmt.Errorf("%w: private supervisor logical predecessor provenance is unavailable", ErrPrivateSupervisorMigrationRequired)
	}
	if !IsPrivateConfig(stateRoot, identity.ConfigPath) || identity.ConfigPath != c.binding.ConfigPath {
		return task.SupervisorRef{}, fmt.Errorf("%w: private supervisor logical predecessor provenance is outside the active state root", ErrPrivateSupervisorMigrationRequired)
	}
	if !privateLogicalIdentityMatchesBinding(identity, c.binding) {
		return task.SupervisorRef{}, fmt.Errorf("%w: persisted private supervisor logical identity differs from the current config", ErrPrivateSupervisorMigrationRequired)
	}
	ready, readyErr := c.availableForBootstrap(ctx)
	if readyErr != nil || !ready {
		return task.SupervisorRef{}, fmt.Errorf("%w: private supervisor logical predecessor binding is unavailable: %w", ErrPrivateSupervisorMigrationRequired, errors.Join(readyErr, ErrBinding))
	}
	// The exact endpoint has answered a validated status request. The persisted
	// daemon identity also carries the prior logical generation, so a changed
	// config cannot be relabeled as the live predecessor.
	return privateSupervisorBindingForIdentity(c.binding, identity), nil
}

func (c *Client) reconcilePrivateSupervisor(ctx context.Context, stateRoot string, persisted task.SupervisorRef, identity, current, currentBinding privateDaemonIdentity) error {
	legacyBinding := privateSupervisorBindingForIdentity(persisted, identity)
	if privateMigrationIdentityMatchesCurrent(identity, current, currentBinding) && task.SameSupervisorIdentity(persisted, c.binding) {
		if startErr := c.StartPrivate(ctx, stateRoot); startErr != nil {
			return fmt.Errorf("%w: verify successor before clearing private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, startErr)
		}
		if clearErr := retirePrivateRecoveryBinding(ctx, stateRoot, c.Binding(), func(authorizeContext context.Context) error {
			return c.authorizePrivateMigrationQueue(authorizeContext, stateRoot, c)
		}); clearErr != nil {
			return fmt.Errorf("%w: clear completed private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, clearErr)
		}
		return nil
	}
	return c.migratePrivateSupervisor(ctx, stateRoot, legacyBinding, privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacyBinding),
		Successor:     currentBinding,
	})
}

func privateMigrationState(base string) (identity privateDaemonIdentity, found bool, pending *privateMigrationRecord, resultErr error) {
	identity, found, resultErr = loadPrivateDaemonIdentity(base)
	if resultErr != nil {
		return privateDaemonIdentity{}, false, nil, resultErr
	}
	record, pendingFound, err := loadPrivateMigrationRecord(base)
	if err != nil {
		return privateDaemonIdentity{}, false, nil, err
	}
	if pendingFound {
		pending = &record
	}
	return identity, found, pending, nil
}

func (c *Client) resumePrivateMigration(ctx context.Context, stateRoot, base string, legacyBinding task.SupervisorRef, bindingFound bool, identity privateDaemonIdentity, found bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) error {
	if pending.LegacyStopped {
		if privateMigrationIdentityMatchesCurrent(pending.Successor, current, currentBinding) {
			return c.completePendingPrivateMigration(ctx, stateRoot, base, pending)
		}
		// Once shutdown is durable, finish the recorded generation before
		// considering another installation. Never rewrite a launched successor.
		return c.finishRecordedPrivateSuccessor(ctx, stateRoot, base, legacyBinding, bindingFound, pending)
	}
	if pending.LegacyShutdownDispatched {
		return c.resumeDispatchedPrivateMigrationIfMatching(ctx, stateRoot, base, legacyBinding, bindingFound, identity, found, current, currentBinding, pending)
	}
	if pending.LegacyShutdownRequested {
		return c.resumeRequestedPrivateMigrationIfMatching(ctx, stateRoot, base, legacyBinding, bindingFound, identity, found, current, currentBinding, pending)
	}
	return c.resumePreparedPrivateMigration(ctx, stateRoot, base, legacyBinding, bindingFound, identity, found, current, currentBinding, pending)
}

func (c *Client) resumePreparedPrivateMigration(ctx context.Context, stateRoot, base string, legacyBinding task.SupervisorRef, bindingFound bool, identity privateDaemonIdentity, found bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) error {
	if found && bindingFound && privatePreparedMigrationSelectsLegacy(pending, legacyBinding, c.binding, identity) {
		// The explicit migration writer selected the still-published Legacy.
		// Withdraw only pre-stop intent; readers and post-stop recovery never
		// erase or retarget the fixed successor, and no edge was completed.
		if err := clearPrivateMigrationRecord(base, pending); err != nil {
			return fmt.Errorf("%w: withdraw prepared private migration journal: %w", ErrPrivateSupervisorMigrationRequired, err)
		}
		return c.reconcilePrivateSupervisor(ctx, stateRoot, legacyBinding, identity, current, currentBinding)
	}
	retargeted, err := retargetPrivateMigrationIfNeeded(base, legacyBinding, identity, found, current, currentBinding, pending)
	if err != nil {
		return err
	}
	pending = retargeted
	if err := validatePendingPrivateMigration(pending, legacyBinding, bindingFound, c.binding, current, currentBinding); err != nil {
		return err
	}
	if found && bindingFound && privateMigrationIdentityMatchesCurrent(identity, current, currentBinding) && task.SameSupervisorIdentity(legacyBinding, c.binding) {
		return c.completePendingPrivateMigration(ctx, stateRoot, base, pending)
	}
	if !found || !privateDaemonIdentityMatchesBinding(identity, privateSupervisorBindingForIdentity(legacyBinding, pending.Legacy)) {
		return fmt.Errorf("%w: private migration journal does not match the persisted daemon identity", ErrPrivateSupervisorMigrationRequired)
	}
	return c.migratePrivateSupervisor(ctx, stateRoot, privateSupervisorBindingForIdentity(legacyBinding, pending.Legacy), pending)
}

func privatePreparedMigrationSelectsLegacy(pending privateMigrationRecord, persisted, selected task.SupervisorRef, identity privateDaemonIdentity) bool {
	return privateMigrationMayRetarget(pending) &&
		privateDaemonIdentityMatchesBinding(pending.Legacy, selected) &&
		privateDaemonIdentityMatchesBinding(identity, selected) &&
		task.SameSupervisorIdentity(persisted, selected)
}

func retargetPrivateMigrationIfNeeded(base string, legacyBinding task.SupervisorRef, identity privateDaemonIdentity, found bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) (privateMigrationRecord, error) {
	if !found || !privateDaemonIdentityMatchesBinding(identity, legacyBinding) || privateMigrationIdentityMatchesCurrent(pending.Successor, current, currentBinding) {
		return pending, nil
	}
	if !privateMigrationMayRetarget(pending) {
		return pending, ErrPrivateSupervisorMigrationRequired
	}
	// A migration can remain blocked while the installed CLI is upgraded
	// again. The legacy endpoint is still the persisted identity, so the
	// pending successor may be retargeted under the bootstrap lock. This is a
	// deliberate journal replacement; the create-once publisher remains
	// strict for every unrelated record.
	retargeted := pending
	retargeted.Successor = currentBinding
	if err := replacePrivateMigrationRecord(base, pending, retargeted); err != nil {
		return pending, fmt.Errorf("%w: retarget private migration journal: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	return retargeted, nil
}

func (c *Client) resumeDispatchedPrivateMigrationIfMatching(ctx context.Context, stateRoot, base string, legacyBinding task.SupervisorRef, bindingFound bool, identity privateDaemonIdentity, found bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) error {
	if !found || !privateDaemonIdentityMatchesBinding(identity, privateSupervisorBindingForIdentity(legacyBinding, pending.Legacy)) {
		return fmt.Errorf("%w: dispatched private migration journal does not match the persisted daemon identity", ErrPrivateSupervisorMigrationRequired)
	}
	return c.resumeDispatchedPrivateMigration(ctx, stateRoot, base, legacyBinding, bindingFound, current, currentBinding, pending)
}

func (c *Client) resumeRequestedPrivateMigrationIfMatching(ctx context.Context, stateRoot, base string, legacyBinding task.SupervisorRef, bindingFound bool, identity privateDaemonIdentity, found bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) error {
	if !found || !privateDaemonIdentityMatchesBinding(identity, privateSupervisorBindingForIdentity(legacyBinding, pending.Legacy)) {
		return fmt.Errorf("%w: requested private migration journal does not match the persisted daemon identity", ErrPrivateSupervisorMigrationRequired)
	}
	legacy := privateMigrationPIDObserver(legacyBinding, pending.Legacy, c.options)
	updated, _, err := dispatchPrivateMigrationShutdown(ctx, base, legacy, pending)
	if err != nil {
		return err
	}
	if privateMigrationIdentityMatchesCurrent(updated.Successor, current, currentBinding) {
		return c.completePendingPrivateMigration(ctx, stateRoot, base, updated)
	}
	return c.finishRecordedPrivateSuccessor(ctx, stateRoot, base, legacyBinding, bindingFound, updated)
}

func (c *Client) resumeDispatchedPrivateMigration(ctx context.Context, stateRoot, base string, legacyBinding task.SupervisorRef, bindingFound bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) error {
	stopped, err := observeDispatchedPrivateMigrationShutdown(ctx, base, legacyBinding, pending, c.options)
	if err != nil {
		return err
	}
	if privateMigrationIdentityMatchesCurrent(stopped.Successor, current, currentBinding) {
		return c.completePendingPrivateMigration(ctx, stateRoot, base, stopped)
	}
	return c.finishRecordedPrivateSuccessor(ctx, stateRoot, base, legacyBinding, bindingFound, stopped)
}

func observeDispatchedPrivateMigrationShutdown(ctx context.Context, base string, legacyBinding task.SupervisorRef, pending privateMigrationRecord, options Options) (privateMigrationRecord, error) {
	if !pending.LegacyShutdownDispatched {
		return pending, fmt.Errorf("%w: private migration shutdown phase is not dispatched", ErrPrivateSupervisorMigrationRequired)
	}
	if pending.LegacyPID <= 0 {
		return pending, fmt.Errorf("%w: dispatched private migration journal lacks a predecessor PID", ErrPrivateSupervisorMigrationRequired)
	}
	if pending.LegacyStopped {
		return pending, nil
	}
	observer := privateMigrationPIDObserver(legacyBinding, pending.Legacy, options)
	process, err := privateMigrationProcess(pending)
	if err != nil {
		return pending, err
	}
	processUnavailable, err := observer.privateProcessUnavailable(process)
	if err != nil {
		return pending, fmt.Errorf("%w: inspect dispatched predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !processUnavailable {
		reachable, reachErr := observer.Reachable(ctx)
		if reachErr != nil {
			return pending, fmt.Errorf("%w: inspect dispatched predecessor endpoint: %w", ErrPrivateSupervisorMigrationRequired, reachErr)
		}
		if reachable {
			// Bootstrap serialization outlives every pending command owner. A
			// reachable exact predecessor here proves the prior shutdown command
			// completed without stopping it, so retry the idempotent request.
			retryable := pending
			retryable.LegacyShutdownDispatched = false
			if err := replacePrivateMigrationRecord(base, pending, retryable); err != nil {
				return pending, fmt.Errorf("%w: restore retryable predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
			}
			stopped, _, retryErr := dispatchPrivateMigrationShutdown(ctx, base, observer, retryable)
			return stopped, retryErr
		}
	}
	if err := observer.waitUnavailableWithProcess(ctx, process); err != nil {
		return pending, fmt.Errorf("%w: wait for dispatched predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	stopped := pending
	stopped.LegacyStopped = true
	if err := replacePrivateMigrationRecord(base, pending, stopped); err != nil {
		return pending, fmt.Errorf("%w: record dispatched predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	return stopped, nil
}

func privateMigrationPIDObserver(binding task.SupervisorRef, identity privateDaemonIdentity, options Options) *Client {
	runtime := privateSupervisorBindingForIdentity(binding, identity)
	return &Client{binding: runtime, runtimeBinding: runtime, options: options}
}

func (c *Client) finishRecordedPrivateSuccessor(ctx context.Context, stateRoot, base string, persisted task.SupervisorRef, bindingFound bool, pending privateMigrationRecord) error {
	if !privateLogicalIdentityPresent(pending.Successor) {
		return fmt.Errorf("%w: stopped migration lacks a logical successor binding", ErrPrivateSupervisorMigrationRequired)
	}
	successor := &Client{
		binding:        privateSupervisorBindingForIdentity(c.binding, pending.Successor),
		runtimeBinding: privateSupervisorBindingForIdentity(c.runtimeBinding, pending.Successor),
		options:        c.options,
	}
	identity := privateDaemonIdentity{Executable: pending.Successor.Executable, SHA256: pending.Successor.SHA256}
	if err := validatePendingPrivateMigration(pending, persisted, bindingFound, successor.binding, identity, pending.Successor); err != nil {
		return err
	}
	if err := successor.verifyBinding(); err != nil {
		return fmt.Errorf("%w: recorded successor configuration changed: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if err := successor.completePendingPrivateMigration(ctx, stateRoot, base, pending); err != nil {
		return err
	}
	return c.MigratePrivateSupervisorWithLock(ctx, stateRoot)
}

func validatePendingPrivateMigration(pending privateMigrationRecord, legacyBinding task.SupervisorRef, bindingFound bool, currentBinding task.SupervisorRef, current, currentRef privateDaemonIdentity) error {
	if !privateMigrationIdentityMatchesCurrent(pending.Successor, current, currentRef) {
		return fmt.Errorf("%w: pending migration targets a different daemon installation", ErrPrivateSupervisorMigrationRequired)
	}
	if !privateLogicalIdentityPresent(pending.Successor) && (!bindingFound || !task.SameSupervisorIdentity(legacyBinding, currentBinding)) {
		return fmt.Errorf("%w: legacy migration journal lacks a verified logical successor binding", ErrPrivateSupervisorMigrationRequired)
	}
	if privateLogicalIdentityPresent(pending.Legacy) && !privateDaemonIdentityMatchesBinding(pending.Legacy, legacyBinding) &&
		(!pending.LegacyStopped || !privateDaemonIdentityMatchesBinding(pending.Successor, legacyBinding)) {
		return fmt.Errorf("%w: pending migration has a different logical predecessor binding", ErrPrivateSupervisorMigrationRequired)
	}
	return nil
}

func (c *Client) completePendingPrivateMigration(ctx context.Context, stateRoot, base string, pending privateMigrationRecord) error {
	ready, err := c.availableForBootstrap(ctx)
	if err != nil {
		return fmt.Errorf("%w: verify successor before clearing private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !ready {
		if startErr := c.startPrivate(ctx); startErr != nil {
			return fmt.Errorf("%w: restart successor before clearing private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, startErr)
		}
	}
	if err := publishPrivateDaemonIdentity(base, c.Binding()); err != nil {
		return fmt.Errorf("%w: publish successor before clearing private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if err := appendPrivateMigrationHistory(base, pending); err != nil {
		return fmt.Errorf("%w: record completed private supervisor migration: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if clearErr := clearPrivateMigrationRecord(base, pending); clearErr != nil {
		return fmt.Errorf("%w: clear completed private migration journal: %w", ErrPrivateSupervisorMigrationRequired, clearErr)
	}
	if clearErr := retirePrivateRecoveryBinding(ctx, stateRoot, c.Binding(), nil); clearErr != nil {
		return fmt.Errorf("%w: clear completed private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, clearErr)
	}
	return nil
}

func (c *Client) migratePrivateSupervisor(ctx context.Context, stateRoot string, legacy task.SupervisorRef, migration privateMigrationRecord) (resultErr error) {
	base := filepath.Dir(c.binding.ConfigPath)
	// Prepared intent selects the durable target; it grants no shutdown
	// authority. Queue authorization still precedes PID capture and every
	// shutdown flag, while readers may observe the exact live Legacy.
	if err := publishPrivateMigrationState(base, migration); err != nil {
		return fmt.Errorf("%w: prepare private migration journal: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	oldSupervisor, recoveryOwned, err := RecoverSavedWithPair(ctx, legacy, c.runtimeBinding.ClientExecutable, c.runtimeBinding.DaemonExecutable, c.options)
	if err != nil {
		return fmt.Errorf("%w: recover previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	preserveLegacy := true
	shutdownOwned := false
	shutdownComplete := false
	queueSafe := false
	defer func() {
		if preserveLegacy {
			resultErr = errors.Join(resultErr, oldSupervisor.PersistPrivateRecoveryBinding(stateRoot))
		}
		if recoveryOwned && queueSafe && !shutdownComplete && !shutdownOwned && !errors.Is(resultErr, ErrPrivateSupervisorMigrationBusy) {
			resultErr = errors.Join(resultErr, oldSupervisor.StopRecovered(ctx, c.options.ObservationTimeout, resultErr))
		}
		resultErr = errors.Join(resultErr, oldSupervisor.CloseAfterPending(resultErr))
	}()
	shutdownOwned, shutdownComplete, queueSafe, err = c.runPrivateMigration(ctx, stateRoot, base, oldSupervisor, &migration)
	if err != nil {
		return err
	}
	preserveLegacy = false
	return nil
}

func (c *Client) runPrivateMigration(ctx context.Context, stateRoot, base string, oldSupervisor *Client, migration *privateMigrationRecord) (shutdownOwned, shutdownComplete, queueSafe bool, resultErr error) {
	if migration == nil {
		return false, false, false, ErrConfiguration
	}
	if err := c.authorizePrivateMigrationQueue(ctx, stateRoot, oldSupervisor); err != nil {
		return false, false, false, err
	}
	queueSafe = true
	if err := oldSupervisor.PersistPrivateRecoveryBinding(stateRoot); err != nil {
		return false, false, queueSafe, fmt.Errorf("%w: prepare private supervisor migration: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	oldProcess, processPresent, err := oldSupervisor.privateDaemonProcessIdentity()
	if err != nil {
		return false, false, queueSafe, fmt.Errorf("%w: capture previous private supervisor process identity: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !processPresent {
		return false, false, queueSafe, fmt.Errorf("%w: previous private supervisor process identity is unavailable", ErrPrivateSupervisorMigrationRequired)
	}
	initial := *migration
	initial.LegacyPID = oldProcess.PID
	initial.LegacyProcessStart = oldProcess.StartToken
	initial.LegacyShutdownRequested = true
	if err = replacePrivateMigrationRecord(base, *migration, initial); err != nil {
		return false, false, queueSafe, fmt.Errorf("%w: record previous private supervisor shutdown request: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	*migration = initial
	// The journal now owns the shutdown request. A crash before Start may safely
	// retry the same idempotent control request against this exact process
	// incarnation; a successful Start is recorded separately before observation.
	shutdownOwned = true
	stopped, _, err := dispatchPrivateMigrationShutdown(ctx, base, oldSupervisor, *migration)
	*migration = stopped
	if err != nil {
		return shutdownOwned, false, queueSafe, err
	}
	shutdownComplete = true
	if err := c.completePendingPrivateMigration(ctx, stateRoot, base, *migration); err != nil {
		return shutdownOwned, shutdownComplete, queueSafe, err
	}
	return shutdownOwned, shutdownComplete, queueSafe, nil
}

func dispatchPrivateMigrationShutdown(ctx context.Context, base string, legacy *Client, pending privateMigrationRecord) (privateMigrationRecord, bool, error) {
	if legacy == nil || !pending.LegacyShutdownRequested || pending.LegacyShutdownDispatched || pending.LegacyStopped {
		return pending, false, fmt.Errorf("%w: private migration shutdown request is invalid", ErrPrivateSupervisorMigrationRequired)
	}
	process, err := privateMigrationProcess(pending)
	if err != nil {
		return pending, false, err
	}
	processUnavailable, err := legacy.privateProcessUnavailable(process)
	if err != nil {
		return pending, false, fmt.Errorf("%w: inspect requested predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !processUnavailable {
		var dispatched bool
		pending, dispatched, err = requestPrivateMigrationShutdown(ctx, base, legacy, pending)
		if err != nil {
			return pending, dispatched, err
		}
	}
	if err := legacy.waitUnavailableWithProcess(ctx, process); err != nil {
		return pending, pending.LegacyShutdownDispatched, fmt.Errorf("%w: wait for previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	stopped := pending
	stopped.LegacyStopped = true
	if err := replacePrivateMigrationRecord(base, pending, stopped); err != nil {
		return pending, pending.LegacyShutdownDispatched, fmt.Errorf("%w: record previous private supervisor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	return stopped, stopped.LegacyShutdownDispatched, nil
}

func requestPrivateMigrationShutdown(ctx context.Context, base string, legacy *Client, pending privateMigrationRecord) (privateMigrationRecord, bool, error) {
	dispatched, shutdownErr := legacy.ShutdownDispatched(ctx)
	if dispatched {
		updated, err := recordPrivateMigrationShutdownDispatched(base, pending)
		if err != nil {
			return pending, true, err
		}
		pending = updated
	}
	if shutdownErr != nil {
		return restoreRetryablePrivateMigrationShutdown(base, pending, dispatched, shutdownErr)
	}
	if !dispatched {
		return pending, false, fmt.Errorf("%w: previous private supervisor shutdown was not dispatched", ErrPrivateSupervisorMigrationRequired)
	}
	return pending, true, nil
}

func recordPrivateMigrationShutdownDispatched(base string, pending privateMigrationRecord) (privateMigrationRecord, error) {
	updated := pending
	updated.LegacyShutdownDispatched = true
	if err := replacePrivateMigrationRecord(base, pending, updated); err != nil {
		return pending, fmt.Errorf("%w: record dispatched predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	return updated, nil
}

func restoreRetryablePrivateMigrationShutdown(base string, pending privateMigrationRecord, dispatched bool, shutdownErr error) (privateMigrationRecord, bool, error) {
	if !dispatched || pendingFrom(shutdownErr) != nil {
		return pending, dispatched, fmt.Errorf("%w: stop previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, shutdownErr)
	}
	retryable := pending
	retryable.LegacyShutdownDispatched = false
	if err := replacePrivateMigrationRecord(base, pending, retryable); err != nil {
		return pending, true, errors.Join(
			fmt.Errorf("%w: restore retryable predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err),
			shutdownErr,
		)
	}
	return retryable, true, fmt.Errorf("%w: stop previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, shutdownErr)
}

func (c *Client) authorizePrivateMigrationQueue(ctx context.Context, stateRoot string, oldSupervisor *Client) error {
	if c == nil || oldSupervisor == nil {
		return ErrConfiguration
	}
	if c.options.PrivateMigrationAuthorizer == nil {
		return fmt.Errorf("%w: private migration policy is not configured", ErrMigrationAuthorization)
	}
	snapshot, err := oldSupervisor.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("%w: inspect previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	// The caller already owns the exclusive bootstrap transition for this
	// predecessor. Preparation must not rebind or acquire another generation.
	if c.options.PrivateTerminalEvidencePreparer != nil {
		if err = c.options.PrivateTerminalEvidencePreparer(ctx, stateRoot, oldSupervisor.Binding(), append([]Job(nil), snapshot.Jobs...)); err != nil {
			return err
		}
	}
	if err = c.options.PrivateMigrationAuthorizer(ctx, stateRoot, oldSupervisor.Binding(), append([]Job(nil), snapshot.Jobs...)); err != nil {
		if errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
			if preserveErr := oldSupervisor.PersistPrivateRecoveryBinding(stateRoot); preserveErr != nil {
				return errors.Join(ErrPrivateSupervisorMigrationBusy,
					fmt.Errorf("%w: preserve previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, preserveErr))
			}
		}
		return err
	}
	return nil
}
