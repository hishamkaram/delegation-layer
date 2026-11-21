package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The retained drain alone owns this comparable acceptance/birth receipt. No
// ready bit is stored: every advance still needs a fresh live queue check.
type privateUpgradePredecessorStartRecord struct {
	Version      int    `json:"version"`
	BootID       string `json:"boot_id"`
	PID          int    `json:"pid,omitempty"`
	ProcessStart string `json:"process_start,omitempty"`
}

func (record privateUpgradePredecessorStartRecord) accepted() bool {
	return record != (privateUpgradePredecessorStartRecord{})
}

func (record privateUpgradePredecessorStartRecord) validate() error {
	if !record.accepted() {
		return nil
	}
	if record.Version != 1 || lifetime.CompareBootIDs(record.BootID, record.BootID) != lifetime.BootSame {
		return ErrBinding
	}
	if record.PID == 0 && record.ProcessStart == "" {
		return nil
	}
	_, err := lifetime.NewProcessRef(record.PID, record.ProcessStart)
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

func validatePrivateUpgradePredecessorProgress(before, after privateUpgradeDrain) error {
	old, next := before.PredecessorStart, after.PredecessorStart
	if old == next {
		return nil
	}
	if before.Phase != UpgradeDraining || after.Phase != UpgradeDraining {
		return ErrBinding
	}
	if !old.accepted() {
		if next.Version != 1 || next.PID != 0 || next.ProcessStart != "" {
			return ErrBinding
		}
		return next.validate()
	}
	if old.PID != 0 || old.Version != next.Version || old.BootID != next.BootID || next.PID <= 0 {
		return ErrBinding
	}
	return next.validate()
}

func validatePrivateUpgradeRecoveredPredecessor(birth privateUpgradePredecessorStartRecord, migration privateMigrationRecord) error {
	if !birth.accepted() || migration.LegacyPID == 0 {
		return nil
	}
	if lifetime.CompareBootIDs(birth.BootID, migration.UpgradeGeneration.LegacyBootID) != lifetime.BootSame {
		return ErrBinding
	}
	expected, err := lifetime.NewProcessRef(birth.PID, birth.ProcessStart)
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	observed, err := lifetime.NewProcessRef(migration.LegacyPID, migration.LegacyProcessStart)
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	same, err := lifetime.SameProcess(observed, expected)
	if err != nil || !same {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

// Only drain advancement may mint cold recovery authority. Later selection and
// migration preparation keep preparePrivateUpgradePredecessor observational.
func preparePrivateUpgradeDrainPredecessor(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, options Options) (_ *Client, current privateUpgradeDrain, resultErr error) {
	current = drain
	if drain.PredecessorStart.accepted() && drain.PredecessorStart.PID == 0 {
		return nil, current, upgradePending("predecessor_birth_unknown", ErrUnknown)
	}
	client, err := bindRecordedPredecessorTransport(ctx, root.Root(), drain.Predecessor, options)
	if err != nil {
		return nil, current, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr))
		}
	}()
	if drain.PredecessorStart.accepted() {
		return client, current, client.verifyPrivateUpgradePredecessor(ctx, root, drain.PredecessorStart)
	}
	observation, err := client.ObservePrivateEndpoint(ctx)
	if err != nil {
		return nil, current, err
	}
	if observation.ready() {
		return client, current, nil
	}
	if err = requirePrivateUpgradeColdPair(drain); err != nil {
		return nil, current, err
	}
	// Cold launch uses the exact saved pair; an installed observation transport
	// is not permission to label another daemon as this predecessor.
	if err = client.Close(); err != nil {
		return nil, current, err
	}
	options.RuntimeClientExecutable = drain.Predecessor.ClientExecutable
	client, err = bindRecordedPredecessorTransport(ctx, root.Root(), drain.Predecessor, options)
	if err != nil {
		return nil, current, err
	}
	current, err = client.startPrivateUpgradePredecessor(ctx, root, drain, observation)
	if err != nil {
		return nil, current, err
	}
	return client, current, client.verifyPrivateUpgradePredecessor(ctx, root, current.PredecessorStart)
}

func requirePrivateUpgradeColdPair(drain privateUpgradeDrain) error {
	pair := privateUpgradePair{ClientExecutable: drain.Predecessor.ClientExecutable, DaemonExecutable: drain.Predecessor.DaemonExecutable}
	missing, err := privateUpgradePairMissing(pair)
	if err != nil || missing {
		return upgradePending("predecessor_pair_missing", errors.Join(ErrBinding, err))
	}
	return validatePrivateUpgradePair(pair)
}

func (c *Client) preparePrivateUpgradeColdStart(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, observation PrivateEndpointObservation) (PrivateRestoreSource, error) {
	if err := requirePrivateUpgradeColdState(ctx, root, drain, c.options); err != nil {
		return PrivateRestoreSource{}, err
	}
	if err := c.requireStoppedPrivateCallback(observation); err != nil {
		return PrivateRestoreSource{}, err
	}
	owner, ok := c.options.DaemonWaitOwner.(DaemonWaitQuiescenceOwner)
	if !ok {
		return PrivateRestoreSource{}, ErrInfrastructureOwnerRequired
	}
	if err := owner.RequireRootDaemonWaitQuiescence(ctx, root.Root()); err != nil {
		// The infrastructure owner authenticates its own pending provenance.
		// Adding a generic pending sibling would hide or invalidate that scope.
		return PrivateRestoreSource{}, &PrivateUpgradeError{ReasonCode: "predecessor_custody_unavailable", Err: err}
	}
	if err := c.checkStoppedPrivateCallbackRestore(ctx, root.Root(), drain.Predecessor, c); err != nil {
		return PrivateRestoreSource{}, err
	}
	resolution, err := c.resolution()
	if err != nil {
		return PrivateRestoreSource{}, err
	}
	restore, err := ObservePrivateRestore(ctx, root, drain.Predecessor, resolution)
	if err != nil || restore.Presence() != PrivateRestorePresent {
		return PrivateRestoreSource{}, errors.Join(ErrPrivateRestoreLayout, err)
	}
	if err = c.closeRefusedPrivateEndpoint(ctx, observation); err != nil {
		return PrivateRestoreSource{}, err
	}
	if err = c.preparePrivateDaemonStart(ctx, root.Root()); err != nil {
		return PrivateRestoreSource{}, err
	}
	return restore.Source(), nil
}

func requirePrivateUpgradeColdState(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, options Options) error {
	if err := requirePrivateUpgradeAuthority(ctx, root, options); err != nil {
		return err
	}
	if drain.Phase != UpgradeDraining {
		return ErrBinding
	}
	facts, err := loadPrivateUpgradeReselectionFacts(root.Root(), drain)
	if err != nil {
		return err
	}
	if facts.aliasPresent || facts.aliasPublishing || facts.runtimeRestartPending {
		return ErrBinding
	}
	return requirePrivateUpgradeColdPair(drain)
}

func (c *Client) startPrivateUpgradePredecessor(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, observation PrivateEndpointObservation) (privateUpgradeDrain, error) {
	if drain.PredecessorStart.accepted() || c.options.DaemonStartGuard == nil {
		return drain, ErrInfrastructureOwnerRequired
	}
	if _, ok := c.options.DaemonWaitOwner.(DaemonWaitBirthOwner); !ok {
		return drain, ErrInfrastructureOwnerRequired
	}
	if _, ok := c.options.DaemonWaitOwner.(DaemonWaitSettlementOwner); !ok {
		return drain, ErrInfrastructureOwnerRequired
	}
	source, err := c.preparePrivateUpgradeColdStart(ctx, root, drain, observation)
	if err != nil {
		return drain, err
	}
	boot, err := lifetime.BootID()
	if err != nil || lifetime.CompareBootIDs(boot, boot) != lifetime.BootSame {
		return drain, upgradePending("predecessor_boot_unknown", errors.Join(ErrUnknown, err))
	}
	accepted := drain
	accepted.PredecessorStart = privateUpgradePredecessorStartRecord{Version: 1, BootID: boot}
	if err = replacePrivateUpgrade(ctx, root, drain, accepted, c.options); err != nil {
		return drain, c.settlePrivateUpgradePredecessorAcceptance(context.WithoutCancel(ctx), root, drain, accepted, err)
	}
	scope := privateUpgradePredecessorStart{root: root, record: accepted, client: c, source: source}
	options := c.options
	options.DaemonStartGuard = privateUpgradePredecessorGuard{scope: scope, guard: options.DaemonStartGuard}
	started, startErr := startDaemonWithAuthorization(ctx, root.Root(), drain.Predecessor.DaemonExecutable, drain.Predecessor.ConfigPath, filepath.Dir(drain.Predecessor.ConfigPath), options, &scope)
	if started.done != nil {
		updated, birthErr := c.recordPrivateUpgradePredecessorBirth(context.WithoutCancel(ctx), root, accepted, started)
		return updated, errors.Join(startErr, birthErr)
	}
	if !started.attempted || started.notStarted {
		rollbackErr := c.releasePrivateUpgradePredecessorUnstarted(context.WithoutCancel(ctx), root, drain, accepted, started)
		return drain, errors.Join(startErr, rollbackErr)
	}
	return accepted, errors.Join(upgradePending("predecessor_birth_unknown", ErrUnknown), startErr)
}

// Only this live invocation can prove canonical Start was never entered. An
// ambiguous write is read back but never authorizes a launch on failed durability.
func (c *Client) settlePrivateUpgradePredecessorAcceptance(ctx context.Context, root taskdir.RootPin, before, accepted privateUpgradeDrain, cause error) error {
	winner, found, err := readPrivateUpgrade(root, accepted.OperationID)
	if err != nil || !found || winner != before && winner != accepted {
		return errors.Join(cause, err, ErrBinding)
	}
	if winner == before {
		return cause
	}
	return errors.Join(cause, c.releasePrivateUpgradePredecessorUnstarted(ctx, root, before, accepted, privateDaemonStart{}))
}

func (c *Client) releasePrivateUpgradePredecessorUnstarted(ctx context.Context, root taskdir.RootPin, before, accepted privateUpgradeDrain, started privateDaemonStart) error {
	if started.done != nil || started.attempted && !started.notStarted || before.PredecessorStart.accepted() {
		return ErrBinding
	}
	candidate := before
	candidate.PredecessorStart = accepted.PredecessorStart
	if candidate != accepted || accepted.PredecessorStart.PID != 0 {
		return ErrBinding
	}
	return guardPrivateGeneration(ctx, root.Root(), c.options.PrivateGenerationGuard, func() error {
		if err := requirePrivateUpgradeAuthority(ctx, root, c.options); err != nil {
			return err
		}
		winner, found, err := readPrivateUpgrade(root, accepted.OperationID)
		if err != nil || !found || winner != accepted {
			return errors.Join(ErrBinding, err)
		}
		return writePrivateUpgradeReplacement(root, before)
	})
}

func (c *Client) recordPrivateUpgradePredecessorBirth(ctx context.Context, root taskdir.RootPin, accepted privateUpgradeDrain, started privateDaemonStart) (privateUpgradeDrain, error) {
	if started.birthReady == nil || started.birth == nil {
		return accepted, upgradePending("predecessor_birth_unknown", ErrUnknown)
	}
	select {
	case <-started.birthReady:
	case <-ctx.Done():
		return accepted, upgradePending("predecessor_birth_unknown", ctx.Err())
	}
	birth, ready, err := started.birth()
	if err != nil || !ready {
		return accepted, upgradePending("predecessor_birth_unknown", errors.Join(ErrUnknown, err))
	}
	updated := accepted
	updated.PredecessorStart.PID, updated.PredecessorStart.ProcessStart = birth.PID(), birth.StartToken()
	if err = replacePrivateUpgrade(ctx, root, accepted, updated, c.options); err != nil {
		return accepted, err
	}
	return updated, nil
}

func (c *Client) verifyPrivateUpgradePredecessor(ctx context.Context, root taskdir.RootPin, receipt privateUpgradePredecessorStartRecord) error {
	birth, err := c.privateUpgradePredecessorBirth(ctx, root, receipt)
	if err != nil {
		return err
	}
	if err = waitPrivateReadyWithProbe(ctx, c, c.options.ObservationTimeout, false, c.runtimeRestartReadiness); err != nil {
		return err
	}
	pid, found, err := c.privateDaemonPID()
	if err != nil || !found || pid != birth.PID() {
		return upgradePending("predecessor_pid_mismatch", errors.Join(ErrBinding, err))
	}
	present, err := lifetime.ProcessPresent(birth)
	if err != nil || !present {
		return upgradePending("predecessor_not_ready", errors.Join(ErrUnknown, err))
	}
	return nil
}

func (c *Client) privateUpgradePredecessorBirth(ctx context.Context, root taskdir.RootPin, receipt privateUpgradePredecessorStartRecord) (lifetime.ProcessRef, error) {
	if receipt.PID == 0 {
		return lifetime.ProcessRef{}, upgradePending("predecessor_birth_unknown", ErrUnknown)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return lifetime.ProcessRef{}, err
	}
	switch lifetime.CompareBootIDs(receipt.BootID, boot) {
	case lifetime.BootDifferent:
		return lifetime.ProcessRef{}, upgradePending("predecessor_boot_changed", nil)
	case lifetime.BootUnknown:
		return lifetime.ProcessRef{}, upgradePending("predecessor_boot_unknown", ErrUnknown)
	case lifetime.BootSame:
	}
	birth, err := lifetime.NewProcessRef(receipt.PID, receipt.ProcessStart)
	if err != nil {
		return lifetime.ProcessRef{}, err
	}
	owner, ok := c.options.DaemonWaitOwner.(DaemonWaitSettlementOwner)
	if !ok {
		return lifetime.ProcessRef{}, ErrInfrastructureOwnerRequired
	}
	owned, closed, err := owner.ObserveDaemonWait(ctx, root.Root(), birth)
	if err != nil {
		return lifetime.ProcessRef{}, err
	}
	if !owned && closed {
		return lifetime.ProcessRef{}, ErrBinding
	}
	if closed {
		return lifetime.ProcessRef{}, upgradePending("predecessor_wait_closed", nil)
	}
	present, err := lifetime.ProcessPresent(birth)
	if err != nil || !present {
		return lifetime.ProcessRef{}, upgradePending("predecessor_not_ready", errors.Join(ErrUnknown, err))
	}
	return birth, nil
}

// The adapter adds only final local validation inside the actual resident Start
// guard. It calls the canonical callback only after validation succeeds, so a
// refusal leaves that callback unentered and definitely does not create a child.
type privateUpgradePredecessorGuard struct {
	scope privateUpgradePredecessorStart
	guard DaemonStartGuard
}

func (guard privateUpgradePredecessorGuard) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	return guard.guard.GuardDaemonStart(ctx, root, func() error {
		if err := guard.scope.validateSources(ctx, root); err != nil {
			return err
		}
		return start()
	})
}

type privateUpgradePredecessorStart struct {
	root   taskdir.RootPin
	record privateUpgradeDrain
	client *Client
	source PrivateRestoreSource
}

func (scope *privateUpgradePredecessorStart) authorizeStart(ctx context.Context, root string) error {
	if scope == nil || scope.client == nil || root != scope.root.Root() || !scope.record.PredecessorStart.accepted() || scope.record.PredecessorStart.PID != 0 {
		return ErrBinding
	}
	return requirePrivateUpgradeColdState(ctx, scope.root, scope.record, scope.client.options)
}

// No transport command, custody query, or nested resident guard may run here.
func (scope *privateUpgradePredecessorStart) validateSources(ctx context.Context, root string) error {
	if err := scope.authorizeStart(ctx, root); err != nil {
		return err
	}
	boot, err := lifetime.BootID()
	if err != nil || lifetime.CompareBootIDs(scope.record.PredecessorStart.BootID, boot) != lifetime.BootSame {
		return errors.Join(ErrBinding, err)
	}
	resolution, err := scope.client.resolution()
	if err != nil {
		return err
	}
	restore, err := ObservePrivateRestore(ctx, scope.root, scope.record.Predecessor, resolution)
	if err != nil || restore.Presence() != PrivateRestorePresent || restore.Source() != scope.source {
		return upgradePending("predecessor_restore_changed", errors.Join(ErrPrivateRestoreLayout, err))
	}
	if err = scope.client.requireStoppedPrivateCallback(PrivateEndpointObservation{state: PrivateEndpointAbsent}); err != nil {
		return err
	}
	path, err := privateEndpointSocketPath(scope.record.Predecessor.Endpoint)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrBinding, err)
	}
	return ctx.Err()
}
