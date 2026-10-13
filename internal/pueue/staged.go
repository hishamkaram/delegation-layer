package pueue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// SubmissionCommit is application-owned durable acceptance for one exact
// staged ordinary submission. Pueue invokes it while the verified supervisor
// generation lease is held and before the row is made schedulable.
type SubmissionCommit func(Observation) error

// StagedSubmissionCommit is the explicit name used by staged callers.
type StagedSubmissionCommit = SubmissionCommit

// InspectionSubmissionCommit is application-owned durable acceptance for one
// exact staged inspection submission. The alias keeps the existing inspection
// observation commit contract usable by both status and staged paths.
type InspectionSubmissionCommit = InspectionObservationCommit

// StagedInspectionCommit is the explicit name used by staged inspection
// callers.
type StagedInspectionCommit = InspectionSubmissionCommit

// StagedLaunchCapability records the behavior required by the staged API.
// ObservedVersion is intentionally absent: staged admission depends on the
// command surface, not a release string.
type StagedLaunchCapability struct {
	AddStashed  bool
	EnqueueByID bool
}

// StagedLaunchCapability observes the add --stashed and enqueue-by-ID help
// surfaces under the caller's verified supervisor-generation lease.
func (c *Client) StagedLaunchCapability(ctx context.Context) (StagedLaunchCapability, error) {
	if c == nil {
		return StagedLaunchCapability{}, ErrConfiguration
	}
	controlContext, cancel, controlErr := c.privateControlContext(ctx)
	if controlErr != nil {
		return StagedLaunchCapability{}, controlErr
	}
	defer cancel()
	var capability StagedLaunchCapability
	var capabilityErr error
	leaseErr := c.withPrivateSupervisorLease(controlContext, func(lockedContext context.Context) error {
		verifyErr := c.verify(lockedContext)
		if verifyErr != nil {
			return verifyErr
		}
		capability, capabilityErr = c.stagedLaunchCapabilityLocked(lockedContext)
		return capabilityErr
	})
	return capability, leaseErr
}

// SupportsStagedLaunch is a boolean projection of StagedLaunchCapability for
// callers that only need an admission gate.
func (c *Client) SupportsStagedLaunch(ctx context.Context) (bool, error) {
	capability, err := c.StagedLaunchCapability(ctx)
	return err == nil && capability.AddStashed && capability.EnqueueByID, err
}

func (c *Client) stagedLaunchCapabilityLocked(ctx context.Context) (StagedLaunchCapability, error) {
	addHelp, err := c.stagedCapabilityProbe(ctx, "add", "--help")
	if err != nil {
		return StagedLaunchCapability{}, err
	}
	capability := StagedLaunchCapability{AddStashed: stagedAddHelp(addHelp)}
	if !capability.AddStashed {
		return StagedLaunchCapability{}, ErrStagedCapability
	}
	enqueueHelp, err := c.stagedCapabilityProbe(ctx, "enqueue", "--help")
	if err != nil {
		return StagedLaunchCapability{}, err
	}
	capability.EnqueueByID = stagedEnqueueHelp(enqueueHelp)
	if !capability.EnqueueByID {
		return StagedLaunchCapability{}, ErrStagedCapability
	}
	return capability, nil
}

func (c *Client) stagedCapabilityProbe(ctx context.Context, args ...string) (string, error) {
	result, err := c.command(ctx, nil, args...)
	if pending := pendingFrom(err); pending != nil {
		settled, waitErr := awaitPending(ctx, pending)
		if pendingFrom(waitErr) != nil {
			return "", errors.Join(ErrStagedCapability, err, waitErr)
		}
		result = settled
		err = waitErr
	}
	if err != nil {
		return "", errors.Join(ErrStagedCapability, err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("%w: capability probe exited with %d", ErrStagedCapability, result.ExitCode)
	}
	return string(append(append([]byte{}, result.Stdout...), result.Stderr...)), nil
}

func stagedAddHelp(output string) bool {
	return strings.Contains(output, "--stashed") && strings.Contains(output, "--print-task-id")
}

func stagedEnqueueHelp(output string) bool {
	return strings.Contains(strings.ToUpper(output), "TASK_IDS")
}

// SubmitStaged performs one managed ordinary staged submission. The add is
// issued once, the exact Stashed receipt is committed, and only then is that
// numeric row enqueued.
func (c *Client) SubmitStaged(ctx context.Context, permit *taskdir.SubmissionPermit, launch Launch, commit SubmissionCommit) (Observation, error) {
	return c.SubmitStagedWithGate(ctx, permit, launch, nil, commit)
}

// SubmitStagedWithCommit is the explicit committed form of SubmitStaged.
func (c *Client) SubmitStagedWithCommit(ctx context.Context, permit *taskdir.SubmissionPermit, launch Launch, commit SubmissionCommit) (Observation, error) {
	return c.SubmitStaged(ctx, permit, launch, commit)
}

// SubmitStagedWithGate runs the application generation gate and staged commit
// under one verified supervisor-generation lease.
func (c *Client) SubmitStagedWithGate(ctx context.Context, permit *taskdir.SubmissionPermit, launch Launch, gate SubmissionGate, commit SubmissionCommit) (Observation, error) {
	if c == nil {
		return Observation{}, ErrConfiguration
	}
	unknown := Observation{State: StateUnknown, Binding: c.binding, Group: "default"}
	if commit == nil {
		return unknown, ErrConfiguration
	}
	controlContext, cancel, controlErr := c.privateControlContext(ctx)
	if controlErr != nil {
		return unknown, controlErr
	}
	defer cancel()
	var observation Observation
	var submissionErr error
	leaseErr := c.withPrivateSupervisorLease(controlContext, func(lockedContext context.Context) error {
		observation, submissionErr = c.submitStagedLocked(lockedContext, permit, launch, gate, commit)
		return submissionErr
	})
	if observation.Binding.ConfigPath == "" {
		observation = unknown
	}
	return observation, leaseErr
}

func (c *Client) submitStagedLocked(ctx context.Context, permit *taskdir.SubmissionPermit, launch Launch, gate SubmissionGate, commit SubmissionCommit) (Observation, error) {
	unknown := Observation{State: StateUnknown, Binding: c.binding, Group: "default"}
	if permit == nil {
		return unknown, task.ErrInvalidPermit
	}
	record, recordErr := permit.Record()
	if recordErr != nil {
		return unknown, recordErr
	}
	identity := Identity{RootID: record.RootID, TaskID: record.TaskID, SpecSHA256: record.SpecSHA256, MetaSHA256: record.MetaSHA256}
	unknown.Identity = identity
	if record.Supervisor != c.binding || record.Label != identity.Label() {
		return unknown, ErrBinding
	}
	launchErr := validateLaunch(launch, record.RootID)
	if launchErr != nil {
		return unknown, launchErr
	}
	verifyErr := c.verify(ctx)
	if verifyErr != nil {
		unknown.Pending = pendingFrom(verifyErr)
		return unknown, verifyErr
	}
	_, capabilityErr := c.stagedLaunchCapabilityLocked(ctx)
	if capabilityErr != nil {
		unknown.Pending = pendingFrom(capabilityErr)
		return unknown, capabilityErr
	}
	if gateErr := c.checkSubmissionGate(ctx, gate); gateErr != nil {
		return unknown, gateErr
	}
	runnerArguments := []string{launch.RunnerExecutable}
	if launch.Supervised {
		runnerArguments = append(runnerArguments, "--guardian", "--supervised")
	}
	runnerArguments = append(runnerArguments, "--root", launch.RootPath, record.TaskID)
	arguments := []string{"add", "--escape", "--stashed", "--label", record.Label, "--print-task-id", "--"}
	id, pending, addErr := c.stagedAdd(ctx, permit.Consume, append(arguments, runnerArguments...)...)
	if addErr != nil {
		unknown.Pending = pending
		return unknown, addErr
	}
	identity.NumericTaskID = &id
	observation, observationErr := stagedOrdinaryObservation(identity, c.binding, id)
	if observationErr != nil {
		return unknown, errors.Join(ErrSubmissionUncertain, observationErr)
	}
	commitErr := commit(observation)
	if commitErr != nil {
		return observation, commitErr
	}
	enqueueErr := c.enqueueStaged(ctx, id)
	if enqueueErr != nil {
		observation.Pending = pendingFrom(enqueueErr)
		return observation, enqueueErr
	}
	return observation, nil
}

func (c *Client) stagedAdd(ctx context.Context, consume func() error, args ...string) (int64, *Pending, error) {
	result, err := c.command(ctx, consume, args...)
	pending := pendingFrom(err)
	if pending != nil {
		settled, waitErr := awaitPending(ctx, pending)
		if pendingFrom(waitErr) != nil {
			return 0, pending, errors.Join(ErrSubmissionUncertain, err, waitErr)
		}
		result = settled
		err = waitErr
	}
	if err != nil {
		if result.Started {
			return 0, nil, errors.Join(ErrSubmissionUncertain, err)
		}
		return 0, nil, err
	}
	id, err := parseID(strings.TrimSpace(string(result.Stdout)))
	if err != nil {
		return 0, nil, errors.Join(ErrSubmissionUncertain, err)
	}
	return id, nil, nil
}

func stagedOrdinaryObservation(identity Identity, binding task.SupervisorRef, id int64) (Observation, error) {
	identity = snapshotIdentity(identity)
	if err := validateIdentity(identity); err != nil {
		return Observation{}, err
	}
	if identity.NumericTaskID == nil || *identity.NumericTaskID != id || id < 0 {
		return Observation{}, ErrUnknown
	}
	observation := Observation{
		Matched:       true,
		State:         StateStashed,
		Group:         "default",
		Identity:      identity,
		Binding:       binding,
		NumericTaskID: id,
	}
	if err := validateStagedOrdinaryObservation(observation, binding); err != nil {
		return Observation{}, err
	}
	return observation, nil
}

func validateStagedOrdinaryObservation(observation Observation, binding task.SupervisorRef) error {
	if !observation.Matched || observation.State != StateStashed || observation.Group != "default" || observation.NumericTaskID < 0 || observation.Binding != binding {
		return ErrUnknown
	}
	if observation.Identity.NumericTaskID == nil || *observation.Identity.NumericTaskID != observation.NumericTaskID {
		return ErrUnknown
	}
	if err := validateIdentity(observation.Identity); err != nil {
		return err
	}
	return nil
}

func (c *Client) enqueueStaged(ctx context.Context, id int64) error {
	if id < 0 {
		return errors.Join(ErrEnqueueUncertain, ErrUnknown)
	}
	result, err := c.command(ctx, nil, "enqueue", strconv.FormatInt(id, 10))
	if err != nil {
		return errors.Join(ErrEnqueueUncertain, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%w: enqueue exited with %d", ErrEnqueueUncertain, result.ExitCode)
	}
	return nil
}

// CommitStaged revalidates one exact Stashed ordinary row while holding the
// generation lease, reruns the idempotent application commit, and enqueues
// only that numeric ID. Already-scheduled rows remain unknown to this API;
// only the application can accept them from an independently durable receipt.
func (c *Client) CommitStaged(ctx context.Context, staged Observation, commit SubmissionCommit) (Observation, error) {
	return c.commitStagedObservation(ctx, staged, commit)
}

// RecoverStaged revalidates an exact ordinary identity and numeric row as
// StateStashed before rerunning the application commit and enqueueing it.
func (c *Client) RecoverStaged(ctx context.Context, identity Identity, numericTaskID int64, commit SubmissionCommit) (Observation, error) {
	if c == nil {
		return Observation{Identity: identity}, ErrConfiguration
	}
	identity, err := stagedIdentityWithID(identity, numericTaskID)
	if err != nil {
		return Observation{Identity: identity}, err
	}
	staged, err := stagedOrdinaryObservation(identity, c.binding, numericTaskID)
	if err != nil {
		return staged, err
	}
	return c.commitStagedObservation(ctx, staged, commit)
}

// RecoverStagedObservation is the observation form of CommitStaged for callers
// that already retained the exact staged receipt.
func (c *Client) RecoverStagedObservation(ctx context.Context, staged Observation, commit SubmissionCommit) (Observation, error) {
	return c.commitStagedObservation(ctx, staged, commit)
}

// RecoverStagedIdentity retains compatibility with callers whose identity
// already carries its numeric target. New callers should use RecoverStaged so
// the numeric target is explicit at the recovery boundary.
func (c *Client) RecoverStagedIdentity(ctx context.Context, identity Identity, commit SubmissionCommit) (Observation, error) {
	if c == nil {
		return Observation{Identity: identity}, ErrConfiguration
	}
	identity = snapshotIdentity(identity)
	if identity.NumericTaskID == nil {
		return Observation{Identity: identity, Binding: c.binding}, ErrUnknown
	}
	return c.RecoverStaged(ctx, identity, *identity.NumericTaskID, commit)
}

func stagedIdentityWithID(identity Identity, id int64) (Identity, error) {
	identity = snapshotIdentity(identity)
	if id < 0 {
		return identity, ErrUnknown
	}
	if identity.NumericTaskID != nil && *identity.NumericTaskID != id {
		return identity, task.ErrIdentityMismatch
	}
	identity.NumericTaskID = &id
	return identity, nil
}

func (c *Client) commitStagedObservation(ctx context.Context, staged Observation, commit SubmissionCommit) (Observation, error) {
	if c == nil {
		return staged, ErrConfiguration
	}
	if commit == nil {
		return staged, ErrConfiguration
	}
	validationErr := validateStagedOrdinaryObservation(staged, c.binding)
	if validationErr != nil {
		return staged, validationErr
	}
	controlContext, cancel, controlErr := c.privateControlContext(ctx)
	if controlErr != nil {
		return staged, controlErr
	}
	defer cancel()
	result := staged
	leaseErr := c.withPrivateSupervisorLease(controlContext, func(lockedContext context.Context) error {
		verifyErr := c.verify(lockedContext)
		if verifyErr != nil {
			result.Pending = pendingFrom(verifyErr)
			return verifyErr
		}
		_, capabilityErr := c.stagedLaunchCapabilityLocked(lockedContext)
		if capabilityErr != nil {
			result.Pending = pendingFrom(capabilityErr)
			return capabilityErr
		}
		current, reconcileErr := c.reconcileStagedOrdinaryLocked(lockedContext, staged.Identity)
		result = current
		if reconcileErr != nil {
			return reconcileErr
		}
		if current.State != StateStashed {
			return ErrUnknown
		}
		commitErr := commit(current)
		if commitErr != nil {
			return commitErr
		}
		enqueueErr := c.enqueueStaged(lockedContext, current.NumericTaskID)
		if enqueueErr != nil {
			result.Pending = pendingFrom(enqueueErr)
			return enqueueErr
		}
		return nil
	})
	return result, leaseErr
}

func (c *Client) reconcileStagedOrdinaryLocked(ctx context.Context, identity Identity) (Observation, error) {
	identity = snapshotIdentity(identity)
	unknown := Observation{State: StateUnknown, Group: "default", Identity: identity, Binding: c.binding}
	if err := validateIdentity(identity); err != nil {
		return unknown, err
	}
	if identity.NumericTaskID == nil {
		return unknown, ErrUnknown
	}
	snapshot, err := c.snapshotAfterVerify(ctx, "label="+identity.Label())
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	observation, err := matchingObservation(snapshot.Jobs, identity, c.binding)
	if err != nil {
		return observation, err
	}
	if (observation.State != StateStashed && observation.State != StateQueued) || observation.Group != "default" {
		return observation, ErrUnknown
	}
	return observation, nil
}

// SubmitInspectionStaged performs one managed staged inspection submission.
func (c *Client) SubmitInspectionStaged(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.SubmitInspectionStagedWithGate(ctx, permit, identity, launch, nil, commit)
}

// SubmitInspectionStagedWithCommit is the explicit committed form of
// SubmitInspectionStaged.
func (c *Client) SubmitInspectionStagedWithCommit(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.SubmitInspectionStaged(ctx, permit, identity, launch, commit)
}

// SubmitInspectionStagedWithGate runs the application generation gate and
// staged inspection commit under one verified supervisor-generation lease.
func (c *Client) SubmitInspectionStagedWithGate(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, gate SubmissionGate, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.submitInspectionStaged(ctx, permit, identity, launch, false, gate, commit)
}

// SubmitConcurrentInspectionStaged performs the same staged protocol in the
// private supervisor's already-configured default group.
func (c *Client) SubmitConcurrentInspectionStaged(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.SubmitConcurrentInspectionStagedWithGate(ctx, permit, identity, launch, nil, commit)
}

// SubmitConcurrentInspectionStagedWithCommit is the explicit committed form
// of SubmitConcurrentInspectionStaged.
func (c *Client) SubmitConcurrentInspectionStagedWithCommit(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.SubmitConcurrentInspectionStaged(ctx, permit, identity, launch, commit)
}

// SubmitConcurrentInspectionStagedWithGate runs the application generation
// gate and staged commit for a concurrent inspection.
func (c *Client) SubmitConcurrentInspectionStagedWithGate(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, gate SubmissionGate, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.submitInspectionStaged(ctx, permit, identity, launch, true, gate, commit)
}

func (c *Client) submitInspectionStaged(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, concurrent bool, gate SubmissionGate, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	identity = snapshotInspectionIdentity(identity)
	if c == nil {
		return InspectionObservation{Identity: identity}, ErrConfiguration
	}
	unknown := InspectionObservation{Binding: c.binding, Identity: identity}
	if commit == nil {
		return unknown, ErrConfiguration
	}
	controlContext, cancel, controlErr := c.privateControlContext(ctx)
	if controlErr != nil {
		return unknown, controlErr
	}
	defer cancel()
	identityErr := validateInspectionIdentity(identity)
	if identityErr != nil {
		return unknown, identityErr
	}
	if permit == nil {
		return unknown, task.ErrInvalidPermit
	}
	permitErr := validateInspectionSubmissionPermit(permit, identity, c.binding)
	if permitErr != nil {
		return unknown, permitErr
	}
	launchErr := validateLaunch(launch, identity.RootID)
	if launchErr != nil {
		return unknown, launchErr
	}
	if concurrent {
		concurrentErr := validateConcurrentInspectionSubmission(c, identity, launch)
		if concurrentErr != nil {
			return unknown, concurrentErr
		}
	}
	var observation InspectionObservation
	var submissionErr error
	leaseErr := c.withPrivateSupervisorLease(controlContext, func(lockedContext context.Context) error {
		observation, submissionErr = c.submitInspectionStagedLocked(lockedContext, permit, identity, launch, concurrent, gate, commit)
		return submissionErr
	})
	if observation.Binding.ConfigPath == "" {
		observation = unknown
	}
	return observation, leaseErr
}

func (c *Client) submitInspectionStagedLocked(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, concurrent bool, gate SubmissionGate, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	unknown := InspectionObservation{Binding: c.binding, Identity: identity}
	if err := c.verify(ctx); err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	if !concurrent {
		snapshot, err := c.snapshotAfterVerify(ctx)
		if err != nil {
			unknown.Pending = pendingFrom(err)
			return unknown, err
		}
		if err := validInspectionGroup(snapshot, identity); err != nil {
			return unknown, err
		}
	}
	if _, err := c.stagedLaunchCapabilityLocked(ctx); err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	if err := c.checkSubmissionGate(ctx, gate); err != nil {
		return unknown, err
	}
	runnerArguments := []string{launch.RunnerExecutable, "--inspection"}
	if launch.Supervised {
		runnerArguments = append(runnerArguments, "--guardian", "--supervised")
	}
	runnerArguments = append(runnerArguments, "--root", launch.RootPath, identity.TaskID)
	arguments := []string{"add", "--escape", "--stashed", "--group", identity.Group(), "--label", identity.Label(), "--print-task-id", "--"}
	id, pending, err := c.stagedAdd(ctx, permit.ConsumeInspectionSubmission, append(arguments, runnerArguments...)...)
	if err != nil {
		unknown.Pending = pending
		return unknown, err
	}
	identity.NumericTaskID = &id
	observation, err := stagedInspectionObservation(identity, c.binding, id)
	if err != nil {
		return unknown, errors.Join(ErrSubmissionUncertain, err)
	}
	observation.TerminalEventRegistrationKnown, observation.TerminalEventRegistered = c.registerStagedInspectionTerminalEvent(ctx, id)
	if err := commit(observation); err != nil {
		return observation, err
	}
	if err := c.enqueueStaged(ctx, id); err != nil {
		observation.Pending = pendingFrom(err)
		return observation, err
	}
	return observation, nil
}

func stagedInspectionObservation(identity InspectionIdentity, binding task.SupervisorRef, id int64) (InspectionObservation, error) {
	identity = snapshotInspectionIdentity(identity)
	if err := validateInspectionIdentity(identity); err != nil {
		return InspectionObservation{}, err
	}
	if identity.NumericTaskID == nil || *identity.NumericTaskID != id || id < 0 {
		return InspectionObservation{}, ErrUnknown
	}
	label := identity.Label()
	observation := InspectionObservation{
		Binding:  binding,
		Identity: identity,
		Job: Job{
			ID:    id,
			Label: &label,
			State: StateStashed,
			Group: identity.Group(),
		},
		Matched: true,
	}
	if err := validateStagedInspectionObservation(observation, binding); err != nil {
		return InspectionObservation{}, err
	}
	return observation, nil
}

func validateStagedInspectionObservation(observation InspectionObservation, binding task.SupervisorRef) error {
	if !observation.Matched || observation.Binding != binding || observation.Job.State != StateStashed || observation.Job.ID < 0 || observation.Job.Group != observation.Identity.Group() || observation.Job.Label == nil || *observation.Job.Label != observation.Identity.Label() {
		return ErrUnknown
	}
	if observation.Identity.NumericTaskID == nil || *observation.Identity.NumericTaskID != observation.Job.ID {
		return ErrUnknown
	}
	return validateInspectionIdentity(observation.Identity)
}

// CommitInspectionStaged revalidates one exact Stashed inspection row while
// holding the generation lease, reruns the idempotent application commit, and
// enqueues only that numeric ID. Already-scheduled rows remain unknown to this
// API; only the application can accept them from a durable exact receipt.
func (c *Client) CommitInspectionStaged(ctx context.Context, staged InspectionObservation, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.commitInspectionStagedObservation(ctx, staged, commit)
}

// RecoverInspectionStaged revalidates an exact inspection identity and numeric
// row as StateStashed before rerunning the application commit and enqueueing it.
func (c *Client) RecoverInspectionStaged(ctx context.Context, identity InspectionIdentity, numericTaskID int64, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	if c == nil {
		return InspectionObservation{Identity: identity}, ErrConfiguration
	}
	identity, err := stagedInspectionIdentityWithID(identity, numericTaskID)
	if err != nil {
		return InspectionObservation{Identity: identity, Binding: c.binding}, err
	}
	staged, err := stagedInspectionObservation(identity, c.binding, numericTaskID)
	if err != nil {
		return staged, err
	}
	return c.commitInspectionStagedObservation(ctx, staged, commit)
}

// RecoverInspectionStagedObservation is the observation form of
// CommitInspectionStaged for callers that already retained the exact staged
// receipt.
func (c *Client) RecoverInspectionStagedObservation(ctx context.Context, staged InspectionObservation, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.commitInspectionStagedObservation(ctx, staged, commit)
}

// RecoverInspectionStagedIdentity retains compatibility with callers whose
// identity already carries its numeric target. New callers should use
// RecoverInspectionStaged so the numeric target is explicit.
func (c *Client) RecoverInspectionStagedIdentity(ctx context.Context, identity InspectionIdentity, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	if c == nil {
		return InspectionObservation{Identity: identity}, ErrConfiguration
	}
	identity = snapshotInspectionIdentity(identity)
	if identity.NumericTaskID == nil {
		return InspectionObservation{Binding: c.binding, Identity: identity}, ErrUnknown
	}
	return c.RecoverInspectionStaged(ctx, identity, *identity.NumericTaskID, commit)
}

func stagedInspectionIdentityWithID(identity InspectionIdentity, id int64) (InspectionIdentity, error) {
	identity = snapshotInspectionIdentity(identity)
	if id < 0 {
		return identity, ErrUnknown
	}
	if identity.NumericTaskID != nil && *identity.NumericTaskID != id {
		return identity, task.ErrIdentityMismatch
	}
	identity.NumericTaskID = &id
	return identity, nil
}

func (c *Client) commitInspectionStagedObservation(ctx context.Context, staged InspectionObservation, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	if c == nil {
		return staged, ErrConfiguration
	}
	if commit == nil {
		return staged, ErrConfiguration
	}
	validationErr := validateStagedInspectionObservation(staged, c.binding)
	if validationErr != nil {
		return staged, validationErr
	}
	controlContext, cancel, controlErr := c.privateControlContext(ctx)
	if controlErr != nil {
		return staged, controlErr
	}
	defer cancel()
	result := staged
	leaseErr := c.withPrivateSupervisorLease(controlContext, func(lockedContext context.Context) error {
		verifyErr := c.verify(lockedContext)
		if verifyErr != nil {
			result.Pending = pendingFrom(verifyErr)
			return verifyErr
		}
		_, capabilityErr := c.stagedLaunchCapabilityLocked(lockedContext)
		if capabilityErr != nil {
			result.Pending = pendingFrom(capabilityErr)
			return capabilityErr
		}
		current, reconcileErr := c.reconcileInspectionLocked(lockedContext, staged.Identity)
		result = current
		if reconcileErr != nil {
			return reconcileErr
		}
		if current.Job.State != StateStashed {
			return ErrUnknown
		}
		current.TerminalEventRegistrationKnown, current.TerminalEventRegistered = c.registerStagedInspectionTerminalEvent(controlContext, current.Job.ID)
		result = current
		commitErr := commit(current)
		if commitErr != nil {
			return commitErr
		}
		enqueueErr := c.enqueueStaged(controlContext, current.Job.ID)
		if enqueueErr != nil {
			result.Pending = pendingFrom(enqueueErr)
			return enqueueErr
		}
		return nil
	})
	return result, leaseErr
}

// registerStagedInspectionTerminalEvent runs after the exact Stashed row is
// known and before the application callback. Once the row is exact, known is
// true even when registration is disabled or fails; registered is then false
// and polling is the safe fallback. This matches the existing inspection
// submission contract.
func (c *Client) registerStagedInspectionTerminalEvent(ctx context.Context, id int64) (known, registered bool) {
	known = true
	enabled, err := c.PrivateTerminalEventsEnabled(ctx)
	if err != nil {
		log.Printf("delegate: staged terminal event registration deferred: %v", err)
		return known, false
	}
	if !enabled {
		return known, false
	}
	if err := c.RegisterPrivateTerminalEvent(ctx, id); err != nil {
		log.Printf("delegate: staged terminal event registration deferred: %v", err)
		return known, false
	}
	return known, true
}
