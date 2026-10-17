package control

import (
	"context"
	"errors"
	"log"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
	authorization, gateErr := c.checkSubmissionGate(ctx, gate)
	if gateErr != nil {
		return unknown, gateErr
	}
	if capabilityErr := c.requireStagedLaunchCapability(ctx, authorization); capabilityErr != nil {
		unknown.Pending = pendingFrom(capabilityErr)
		return unknown, capabilityErr
	}
	runnerArguments := inspectionWorkerArguments(launch, identity)
	arguments := []string{"add", "--escape", "--stashed", "--group", identity.Group(), "--label", identity.Label(), "--print-task-id", "--"}
	var id int64
	var pending *Pending
	var observation InspectionObservation
	err := c.withPrivateQueueIdentityLease(ctx, func(lockedContext context.Context) error {
		var addErr error
		id, pending, addErr = c.stagedAdd(lockedContext, permit.ConsumeInspectionSubmission, append(arguments, runnerArguments...)...)
		if addErr != nil {
			return addErr
		}
		identity.NumericTaskID = &id
		var observationErr error
		observation, observationErr = stagedInspectionObservation(identity, c.binding, id)
		if observationErr != nil {
			return errors.Join(ErrSubmissionUncertain, observationErr)
		}
		observation.TerminalEventRegistrationKnown, observation.TerminalEventRegistered = c.registerStagedInspectionTerminalEvent(lockedContext, id)
		if commitErr := commit(observation); commitErr != nil {
			return commitErr
		}
		return c.enqueueStaged(lockedContext, id)
	})
	if err != nil {
		if !observation.Matched {
			unknown.Pending = pending
			return unknown, err
		}
		observation.Pending = pendingFrom(err)
		return observation, err
	}
	return observation, nil
}

func stagedInspectionCommand(launch Launch, identity InspectionIdentity) string {
	return escapedCommand(inspectionWorkerArguments(launch, identity)...)
}

func (c *Client) requireStagedLaunchCapability(ctx context.Context, authorization SubmissionAuthorization) error {
	if authorization.StagedLaunchCapability == nil {
		_, err := c.stagedLaunchCapabilityLocked(ctx)
		return err
	}
	capability := *authorization.StagedLaunchCapability
	if !capability.AddStashed || !capability.EnqueueByID {
		return ErrStagedCapability
	}
	return nil
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
	return c.commitInspectionStagedObservation(ctx, staged, nil, commit)
}

// CommitInspectionStagedWithGate is CommitInspectionStaged with application
// generation and staged-capability authorization under the same lease.
func (c *Client) CommitInspectionStagedWithGate(ctx context.Context, staged InspectionObservation, gate SubmissionGate, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.commitInspectionStagedObservation(ctx, staged, gate, commit)
}

// RecoverInspectionStaged revalidates an exact inspection identity and numeric
// row as StateStashed before rerunning the application commit and enqueueing it.
func (c *Client) RecoverInspectionStaged(ctx context.Context, identity InspectionIdentity, numericTaskID int64, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.RecoverInspectionStagedWithGate(ctx, identity, numericTaskID, nil, commit)
}

// RecoverInspectionStagedWithGate is RecoverInspectionStaged with application
// generation and staged-capability authorization under the same lease.
func (c *Client) RecoverInspectionStagedWithGate(ctx context.Context, identity InspectionIdentity, numericTaskID int64, gate SubmissionGate, commit InspectionSubmissionCommit) (InspectionObservation, error) {
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
	return c.commitInspectionStagedObservation(ctx, staged, gate, commit)
}

// RecoverInspectionStagedObservation is the observation form of
// CommitInspectionStaged for callers that already retained the exact staged
// receipt.
func (c *Client) RecoverInspectionStagedObservation(ctx context.Context, staged InspectionObservation, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	return c.commitInspectionStagedObservation(ctx, staged, nil, commit)
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

func (c *Client) commitInspectionStagedObservation(ctx context.Context, staged InspectionObservation, gate SubmissionGate, commit InspectionSubmissionCommit) (InspectionObservation, error) {
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
		var commitErr error
		result, commitErr = c.commitInspectionStagedObservationLocked(lockedContext, staged, gate, commit)
		return commitErr
	})
	return result, leaseErr
}

func (c *Client) commitInspectionStagedObservationLocked(ctx context.Context, staged InspectionObservation, gate SubmissionGate, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	result := staged
	if verifyErr := c.verify(ctx); verifyErr != nil {
		result.Pending = pendingFrom(verifyErr)
		return result, verifyErr
	}
	authorization, gateErr := c.checkSubmissionGate(ctx, gate)
	if gateErr != nil {
		return result, gateErr
	}
	if capabilityErr := c.requireStagedLaunchCapability(ctx, authorization); capabilityErr != nil {
		result.Pending = pendingFrom(capabilityErr)
		return result, capabilityErr
	}
	return c.commitInspectionStagedObservationWithQueueIdentityMutation(ctx, staged, commit)
}

func (c *Client) commitInspectionStagedObservationWithQueueIdentityMutation(ctx context.Context, staged InspectionObservation, commit InspectionSubmissionCommit) (InspectionObservation, error) {
	result := staged
	leaseErr := c.withPrivateQueueIdentityMutation(ctx, func(identityContext context.Context) error {
		current, reconcileErr := c.reconcileInspectionLocked(identityContext, staged.Identity)
		result = current
		if reconcileErr != nil {
			return reconcileErr
		}
		if current.Job.State != StateStashed {
			return ErrUnknown
		}
		current.TerminalEventRegistrationKnown, current.TerminalEventRegistered = c.registerStagedInspectionTerminalEvent(identityContext, current.Job.ID)
		result = current
		if commitErr := commit(current); commitErr != nil {
			return commitErr
		}
		if enqueueErr := c.enqueueStaged(identityContext, current.Job.ID); enqueueErr != nil {
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
