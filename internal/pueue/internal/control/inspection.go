package control

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// InspectionGroupPermit is the narrow authority for creating one derived
// inspection group. The action-specific consume method prevents a submission
// or stop permit from being accepted at this boundary. Identity and binding
// come from the durable journal permit, never from caller-selected values.
type InspectionGroupPermit interface {
	ConsumeInspectionGroup() error
	InspectionGroupRootID() string
	InspectionGroupSupervisor() task.SupervisorRef
}

// InspectionSubmissionPermit is the narrow authority for admitting one
// inspection worker. Its immutable root/task identity and supervisor binding
// are checked before any fresh supervisor observation.
type InspectionSubmissionPermit interface {
	ConsumeInspectionSubmission() error
	InspectionSubmissionRootID() string
	InspectionSubmissionTaskID() string
	InspectionSubmissionSupervisor() task.SupervisorRef
}

// InspectionStopPermit is the narrow authority for routing one already-bound
// inspection target. The numeric target accessor must return an owned copy so
// callers cannot mutate the permit between validation and command execution.
type InspectionStopPermit interface {
	ConsumeInspectionStop() error
	InspectionStopRootID() string
	InspectionStopTaskID() string
	InspectionStopNumericTaskID() *int64
	InspectionStopSupervisor() task.SupervisorRef
}

// InspectionObservation is positive only when one exact labeled job belongs
// to the saved group with its expected scheduling policy.
type InspectionObservation struct {
	Binding                        task.SupervisorRef
	Identity                       InspectionIdentity
	Job                            Job
	Matched                        bool
	Pending                        *Pending
	TerminalEventRegistrationKnown bool
	TerminalEventRegistered        bool
}

// InspectionObservationCommit is application-owned acceptance logic. Pueue
// only guarantees that the callback runs while the inspected supervisor
// generation is held under its verified shared lease.
type InspectionObservationCommit func(InspectionObservation) error

// Snapshot verifies the pinned binding, then obtains and parses one complete
// status JSON response. The command remains owned by the process observer if
// the caller's finite observation expires.
func (c *Client) Snapshot(ctx context.Context) (QueueSnapshot, error) {
	if err := c.verify(ctx); err != nil {
		return QueueSnapshot{}, err
	}
	return c.snapshotAfterVerify(ctx)
}

func (c *Client) snapshotAfterVerify(ctx context.Context, query ...string) (QueueSnapshot, error) {
	return c.snapshotAfterVerifyCompletion(ctx, false, query...)
}

// snapshotAfterVerifySettled is reserved for asynchronous maintenance that
// already owns an admitted command through natural completion. Interactive
// observations keep snapshotAfterVerify's finite-return behavior.
func (c *Client) snapshotAfterVerifySettled(ctx context.Context, query ...string) (QueueSnapshot, error) {
	return c.snapshotAfterVerifyCompletion(ctx, true, query...)
}

func (c *Client) snapshotAfterVerifyCompletion(ctx context.Context, settle bool, query ...string) (QueueSnapshot, error) {
	args := append([]string{"status", "--json"}, query...)
	result, err := c.command(ctx, nil, args...)
	if settle {
		if pending := pendingFrom(err); pending != nil {
			result, err = awaitPending(ctx, pending)
		}
	}
	if err != nil {
		return QueueSnapshot{}, errors.Join(ErrUnknown, err)
	}
	return snapshotFromResult(result, c.runtimeBinding.ObservedVersion)
}

// CreateInspectionGroup creates exactly one derived group after static root
// validation and fresh binding verification. Existing groups are never edited;
// the caller separately reconciles the resulting snapshot.
func (c *Client) CreateInspectionGroup(ctx context.Context, permit InspectionGroupPermit, rootID string) (CommandResult, error) {
	if permit == nil {
		return CommandResult{}, task.ErrInvalidPermit
	}
	if err := task.ValidateRootID(rootID); err != nil {
		return CommandResult{}, err
	}
	if permit.InspectionGroupRootID() != rootID {
		return CommandResult{}, task.ErrIdentityMismatch
	}
	if err := validateInspectionPermitSupervisor(permit.InspectionGroupSupervisor(), c.binding); err != nil {
		return CommandResult{}, err
	}
	if err := c.verify(ctx); err != nil {
		return CommandResult{}, err
	}
	return c.command(ctx, permit.ConsumeInspectionGroup, "group", "add", "--parallel", "1", inspectionGroup(rootID))
}

// ReconcileInspection positively identifies exactly one worker in the
// recorded running group. Duplicate, stale, malformed or
// unknown rows remain unknown.
func (c *Client) ReconcileInspection(ctx context.Context, identity InspectionIdentity) (InspectionObservation, error) {
	return c.ReconcileInspectionWithLease(ctx, identity, nil)
}

// ReconcileInspectionWithLease reconciles one inspection and, when supplied,
// runs the application commit callback before releasing the verified
// supervisor-generation lease.
func (c *Client) ReconcileInspectionWithLease(ctx context.Context, identity InspectionIdentity, commit InspectionObservationCommit) (InspectionObservation, error) {
	identity = snapshotInspectionIdentity(identity)
	unknown := InspectionObservation{Binding: c.binding, Identity: identity}
	if validationErr := validateInspectionIdentity(identity); validationErr != nil {
		return unknown, validationErr
	}
	var observation InspectionObservation
	err := c.withPrivateSupervisorLease(ctx, func(lockedContext context.Context) error {
		var reconcileErr error
		observation, reconcileErr = c.reconcileInspectionLocked(lockedContext, identity)
		if reconcileErr != nil || commit == nil {
			return reconcileErr
		}
		return commit(observation)
	})
	if observation.Binding.ConfigPath == "" {
		observation = unknown
	}
	if observation.Pending == nil {
		observation.Pending = pendingFrom(err)
	}
	return observation, err
}

// AwaitInspectionObservation consumes the result of an exact status command
// retained by ReconcileInspection. It never launches another control command.
func (c *Client) AwaitInspectionObservation(ctx context.Context, identity InspectionIdentity, pending *Pending) (InspectionObservation, error) {
	return c.AwaitInspectionObservationWithLease(ctx, identity, pending, nil)
}

// AwaitInspectionObservationWithLease consumes a retained exact status result
// and, when supplied, runs the application commit callback before releasing
// the verified supervisor-generation lease.
func (c *Client) AwaitInspectionObservationWithLease(ctx context.Context, identity InspectionIdentity, pending *Pending, commit InspectionObservationCommit) (InspectionObservation, error) {
	identity = snapshotInspectionIdentity(identity)
	unknown := InspectionObservation{Identity: identity}
	if c == nil {
		return unknown, ErrConfiguration
	}
	unknown.Binding = c.binding
	if validationErr := validateInspectionIdentity(identity); validationErr != nil {
		return unknown, validationErr
	}
	if !c.matchesInspectionPending(identity, pending) {
		return unknown, fmt.Errorf("%w: pending status observation does not match inspection generation", task.ErrIdentityMismatch)
	}
	result, err := awaitPending(ctx, pending)
	err = statusObservationError(err)
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, errors.Join(ErrUnknown, err)
	}
	var observation InspectionObservation
	err = c.withPrivateSupervisorLease(ctx, func(lockedContext context.Context) error {
		if verifyErr := c.verify(lockedContext); verifyErr != nil {
			return verifyErr
		}
		_, version, _ := pending.StatusIdentity()
		snapshot, snapshotErr := snapshotFromResult(result, version)
		if snapshotErr != nil {
			return errors.Join(ErrUnknown, ErrStatusOperational, snapshotErr)
		}
		observation, snapshotErr = matchingInspectionObservation(snapshot, identity, c.binding)
		if snapshotErr != nil || commit == nil {
			return snapshotErr
		}
		return commit(observation)
	})
	if observation.Binding.ConfigPath == "" {
		observation = unknown
	}
	return observation, err
}

// CommitInspectionObservation runs application acceptance for a previously
// matched observation while holding a freshly verified generation lease. It
// does not reread the queue; the cached observation remains authoritative only
// while its recorded binding is still the active generation.
func (c *Client) CommitInspectionObservation(ctx context.Context, observation InspectionObservation, commit InspectionObservationCommit) error {
	if c == nil || commit == nil {
		return ErrConfiguration
	}
	if !observation.Matched || observation.Binding != c.binding {
		return task.ErrIdentityMismatch
	}
	return c.withPrivateSupervisorLease(ctx, func(lockedContext context.Context) error {
		if err := c.verify(lockedContext); err != nil {
			return err
		}
		if observation.Binding != c.binding {
			return task.ErrIdentityMismatch
		}
		return commit(observation)
	})
}

func (c *Client) matchesInspectionPending(identity InspectionIdentity, pending *Pending) bool {
	if pending == nil {
		return false
	}
	want := []string{
		c.runtimeBinding.ClientExecutable,
		"-c", c.runtimeBinding.ConfigPath,
		"status", "--json", "label=" + identity.Label(),
	}
	binding, version, ok := pending.StatusIdentity()
	return ok && binding == c.runtimeBinding &&
		version == c.runtimeBinding.ObservedVersion && pending.MatchesArgs(want)
}

// reconcileInspectionLocked reconciles one inspection task while the caller
// holds the private supervisor lease or bootstrap lock. Keeping this separate
// lets the public method retain a lease for an in-flight status command without
// nesting a shared lease under an existing exclusive transition.
func (c *Client) reconcileInspectionLocked(ctx context.Context, identity InspectionIdentity) (InspectionObservation, error) {
	identity = snapshotInspectionIdentity(identity)
	unknown := InspectionObservation{Binding: c.binding, Identity: identity}
	if validationErr := validateInspectionIdentity(identity); validationErr != nil {
		return unknown, validationErr
	}
	if err := c.verify(ctx); err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	snapshot, err := c.snapshotAfterVerify(ctx, "label="+identity.Label())
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	return matchingInspectionObservation(snapshot, identity, c.binding)
}

func matchingInspectionObservation(snapshot QueueSnapshot, identity InspectionIdentity, binding task.SupervisorRef) (InspectionObservation, error) {
	identity = snapshotInspectionIdentity(identity)
	unknown := InspectionObservation{Binding: binding, Identity: identity}
	if validationErr := validateInspectionIdentity(identity); validationErr != nil {
		return unknown, validationErr
	}
	if err := validateInspectionQueueBinding(identity, binding); err != nil {
		return unknown, err
	}
	if groupErr := validInspectionGroup(snapshot, identity); groupErr != nil {
		return unknown, groupErr
	}

	matches := 0
	var job Job
	for _, candidate := range snapshot.Jobs {
		if candidate.Label == nil || *candidate.Label != identity.Label() {
			continue
		}
		matches++
		job = candidate
	}
	if matches != 1 || job.Group != identity.Group() || job.State == StateUnknown ||
		(identity.NumericTaskID != nil && *identity.NumericTaskID != job.ID) {
		return unknown, ErrUnknown
	}
	return InspectionObservation{Binding: binding, Identity: identity, Job: job, Matched: true}, nil
}

func validInspectionGroup(snapshot QueueSnapshot, identity InspectionIdentity) error {
	group, ok := snapshot.Groups[identity.Group()]
	parallel := uint64(1)
	if identity.Group() == "default" {
		parallel = 0
	}
	if !ok || group.Status != "Running" || group.ParallelTasks != parallel {
		return ErrUnknown
	}
	return nil
}

func validateInspectionIdentity(identity InspectionIdentity) error {
	if err := task.ValidateRootID(identity.RootID); err != nil {
		return err
	}
	if err := task.ValidateTaskID(identity.TaskID); err != nil {
		return err
	}
	if identity.Group() != inspectionGroup(identity.RootID) && identity.Group() != "default" {
		return task.ErrIdentityMismatch
	}
	if identity.NumericTaskID != nil && *identity.NumericTaskID < 0 {
		return ErrUnknown
	}
	return nil
}

func snapshotInspectionIdentity(identity InspectionIdentity) InspectionIdentity {
	if identity.NumericTaskID != nil {
		id := *identity.NumericTaskID
		identity.NumericTaskID = &id
	}
	return identity
}

// SubmitInspection admits one fixed inspection worker only after the root and
// worker are validated, the binding is fresh and the recorded group is proven
// ready. Admission is create-once and is reconciled without a
// retry after the supervisor response.
func (c *Client) SubmitInspection(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch) (InspectionObservation, error) {
	return c.SubmitInspectionWithGate(ctx, permit, identity, launch, nil)
}

// SubmitInspectionWithGate runs the application-owned generation gate under
// the same lease as the inspection add.
func (c *Client) SubmitInspectionWithGate(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, gate SubmissionGate) (InspectionObservation, error) {
	return c.submitInspection(ctx, permit, identity, launch, false, gate)
}

// SubmitConcurrentInspection admits a runtime inspection in the private
// supervisor's already-configured default group. Its completed add response
// is the task-specific submission receipt, so this path deliberately avoids a
// queue-wide status snapshot. It does not establish worker completion: the
// application still requires independently recorded terminal evidence.
func (c *Client) SubmitConcurrentInspection(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch) (InspectionObservation, error) {
	return c.SubmitConcurrentInspectionWithGate(ctx, permit, identity, launch, nil)
}

// SubmitConcurrentInspectionWithGate runs the application-owned generation
// gate under the same lease as the concurrent inspection add.
func (c *Client) SubmitConcurrentInspectionWithGate(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, gate SubmissionGate) (InspectionObservation, error) {
	return c.submitInspection(ctx, permit, identity, launch, true, gate)
}

func (c *Client) submitInspection(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, concurrent bool, gate SubmissionGate) (InspectionObservation, error) {
	identity = snapshotInspectionIdentity(identity)
	if c == nil {
		return InspectionObservation{Identity: identity}, ErrConfiguration
	}
	controlContext, cancel, err := c.privateControlContext(ctx)
	if err != nil {
		return InspectionObservation{Identity: identity}, err
	}
	defer cancel()
	unknown := InspectionObservation{Binding: c.binding, Identity: identity}
	if validationErr := validateInspectionIdentity(identity); validationErr != nil {
		return unknown, validationErr
	}
	if permit == nil {
		return unknown, task.ErrInvalidPermit
	}
	if validationErr := validateInspectionSubmissionPermit(permit, identity, c.binding); validationErr != nil {
		return unknown, validationErr
	}
	if validationErr := validateLaunch(launch, identity.RootID); validationErr != nil {
		return unknown, validationErr
	}
	if concurrent {
		if validationErr := validateConcurrentInspectionSubmission(c, identity, launch); validationErr != nil {
			return unknown, validationErr
		}
	}
	var observation InspectionObservation
	err = c.withPrivateSupervisorLease(controlContext, func(lockedContext context.Context) error {
		if concurrent {
			observation, err = c.submitConcurrentInspectionLocked(lockedContext, permit, identity, launch, gate)
		} else {
			observation, err = c.submitInspectionLocked(lockedContext, permit, identity, launch, gate)
		}
		return err
	})
	if observation.Binding.ConfigPath == "" {
		observation = unknown
	}
	return observation, err
}

func validateConcurrentInspectionSubmission(c *Client, identity InspectionIdentity, launch Launch) error {
	if identity.Group() != "default" {
		return task.ErrIdentityMismatch
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return ErrConfiguration
	}
	if stateRoot != launch.RootPath {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (c *Client) submitInspectionLocked(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, gate SubmissionGate) (InspectionObservation, error) {
	unknown := InspectionObservation{Binding: c.binding, Identity: identity}
	if err := c.verify(ctx); err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	snapshot, err := c.snapshotAfterVerify(ctx)
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	if groupErr := validInspectionGroup(snapshot, identity); groupErr != nil {
		return unknown, groupErr
	}
	if _, gateErr := c.checkSubmissionGate(ctx, gate); gateErr != nil {
		return unknown, gateErr
	}
	id, terminalEventRegistered, terminalEventRegistrationKnown, err := c.addInspectionWorker(ctx, permit, identity, launch)
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	identity.NumericTaskID = &id
	observation, reconcileErr := c.reconcileInspectionLocked(ctx, identity)
	observation.TerminalEventRegistrationKnown = terminalEventRegistrationKnown
	observation.TerminalEventRegistered = terminalEventRegistered
	return observation, reconcileErr
}

func (c *Client) submitConcurrentInspectionLocked(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch, gate SubmissionGate) (InspectionObservation, error) {
	unknown := InspectionObservation{Binding: c.binding, Identity: identity}
	if err := c.verify(ctx); err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	if _, gateErr := c.checkSubmissionGate(ctx, gate); gateErr != nil {
		return unknown, gateErr
	}
	id, terminalEventRegistered, terminalEventRegistrationKnown, err := c.addInspectionWorker(ctx, permit, identity, launch)
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	identity.NumericTaskID = &id
	label := identity.Label()
	return InspectionObservation{
		Binding:                        c.binding,
		Identity:                       identity,
		Job:                            Job{ID: id, Label: &label, State: StateQueued, Group: identity.Group()},
		Matched:                        true,
		TerminalEventRegistrationKnown: terminalEventRegistrationKnown,
		TerminalEventRegistered:        terminalEventRegistered,
	}, nil
}

func (c *Client) addInspectionWorker(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch) (id int64, terminalEventRegistered, terminalEventRegistrationKnown bool, resultErr error) {
	resultErr = c.withPrivateQueueIdentityLease(ctx, func(lockedContext context.Context) error {
		id, terminalEventRegistered, terminalEventRegistrationKnown, resultErr = c.addInspectionWorkerWithIdentityLease(lockedContext, permit, identity, launch)
		return resultErr
	})
	return id, terminalEventRegistered, terminalEventRegistrationKnown, resultErr
}

func (c *Client) addInspectionWorkerWithIdentityLease(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch) (id int64, terminalEventRegistered, terminalEventRegistrationKnown bool, resultErr error) {
	runnerArguments := inspectionWorkerArguments(launch, identity)
	arguments := []string{"add", "--escape", "--group", identity.Group(), "--label", identity.Label(), "--print-task-id", "--"}
	result, err := c.command(ctx, permit.ConsumeInspectionSubmission, append(arguments, runnerArguments...)...)
	if err != nil {
		return 0, false, false, errors.Join(ErrUnknown, err)
	}
	id, err = parseID(strings.TrimSpace(string(result.Stdout)))
	if err != nil {
		return 0, false, false, err
	}
	terminalEventRegistrationKnown = true
	enabled, eventErr := c.PrivateTerminalEventsEnabled(ctx)
	if eventErr != nil {
		// A terminal-event ticket is an optimization, never a second
		// submission authority. Terminal evidence falls back to conservative
		// queue reconciliation when its durable write is unavailable.
		log.Printf("delegate: terminal event registration deferred: %v", eventErr)
		return id, false, terminalEventRegistrationKnown, nil
	}
	if !enabled {
		return id, false, terminalEventRegistrationKnown, nil
	}
	if eventErr = c.RegisterPrivateTerminalEvent(ctx, id); eventErr != nil {
		// A terminal-event ticket is an optimization, never a second
		// submission authority. Terminal evidence falls back to conservative
		// queue reconciliation when its durable write is unavailable.
		log.Printf("delegate: terminal event registration deferred: %v", eventErr)
		return id, false, terminalEventRegistrationKnown, nil
	}
	return id, true, terminalEventRegistrationKnown, nil
}

// inspectionWorkerArguments is shared by immediate and staged auxiliary
// submission, so their native guardian modes cannot drift apart.
func inspectionWorkerArguments(launch Launch, identity InspectionIdentity) []string {
	mode := "--inspection"
	if launch.WorkspaceOperation {
		mode = "--workspace-operation"
	}
	arguments := []string{launch.RunnerExecutable, mode}
	if launch.Supervised {
		arguments = append(arguments, "--guardian", "--supervised")
	}
	return append(arguments, "--root", launch.RootPath, identity.TaskID)
}

// StopInspection routes one saved inspection target to remove or kill after a
// fresh exact reconciliation. Ended workers are a no-op; uncertain routing
// consumes no new authority and never broadens to a group action.
func (c *Client) StopInspection(ctx context.Context, permit InspectionStopPermit, identity InspectionIdentity) (StopResult, error) {
	identity = snapshotInspectionIdentity(identity)
	return c.withPrivateStop(ctx, func(controlContext context.Context) (StopResult, error) {
		return c.stopInspectionLocked(controlContext, permit, identity)
	})
}

func (c *Client) stopInspectionLocked(ctx context.Context, permit InspectionStopPermit, identity InspectionIdentity) (StopResult, error) {
	result := StopResult{ObservedState: StateUnknown}
	if permit == nil {
		return result, task.ErrInvalidPermit
	}
	identity = snapshotInspectionIdentity(identity)
	if err := validateInspectionIdentity(identity); err != nil {
		return result, err
	}
	result.Requested = true
	if identity.NumericTaskID == nil {
		return result, fmt.Errorf("%w: inspection stop has no saved numeric target", ErrUnknown)
	}
	if err := validateInspectionStopPermit(permit, identity, c.binding); err != nil {
		return result, err
	}
	id := *identity.NumericTaskID
	result.NumericTaskID = &id
	observation, err := settlePendingControlObservation(c, ctx, func(controlContext context.Context) (InspectionObservation, error) {
		return c.reconcileInspectionLocked(controlContext, identity)
	})
	result.Pending = pendingFrom(err)
	result.InFlight = result.Pending != nil
	if err != nil {
		return result, err
	}
	result.Matched, result.ObservedState = observation.Matched, observation.Job.State
	if !observation.Matched {
		return result, ErrUnknown
	}
	if result.ObservedState == StateEnded {
		return result, nil
	}
	action, actionErr := inspectionStopAction(result.ObservedState)
	if actionErr != nil {
		return result, actionErr
	}
	result.Action = action
	return c.stopInspectionMatched(ctx, permit, result, id, action)
}

func (c *Client) stopInspectionMatched(ctx context.Context, permit InspectionStopPermit, result StopResult, id int64, action string) (StopResult, error) {
	current, err := c.readBinding()
	if err != nil || current != c.runtimeBinding {
		return result, errors.Join(ErrBinding, err)
	}
	if action == "remove" {
		// Removing a queued row can make its numeric ID immediately reusable and
		// produces no terminal callback. Retire the old ID-scoped ticket first;
		// the exclusive supervisor generation scope prevents a concurrent add
		// between this clear and the remove command.
		if err = c.ClearPrivateTerminalEvents(ctx, id); err != nil {
			return result, err
		}
	}
	consume := func() error {
		if consumeErr := permit.ConsumeInspectionStop(); consumeErr != nil {
			return consumeErr
		}
		result.Attempted = true
		return nil
	}
	command, err := c.command(ctx, consume, action, strconv.FormatInt(id, 10))
	if pending := pendingFrom(err); pending != nil {
		result.Pending, result.InFlight = pending, true
		return result, err
	}
	if !result.Attempted {
		return result, err
	}
	result.Acknowledged = commandAcknowledgment(command, err)
	if result.Acknowledged != nil && *result.Acknowledged {
		result.Message = "supervisor acknowledged request; termination remains separately observed"
	} else if result.Acknowledged != nil && !*result.Acknowledged {
		result.Message = "supervisor request failed or was refused; termination remains unknown"
	}
	return result, err
}

func validateInspectionPermitSupervisor(candidate, expected task.SupervisorRef) error {
	if err := task.ValidateFreshSupervisorRef(candidate); err != nil {
		return errors.Join(ErrBinding, err)
	}
	if !task.SameSupervisorIdentity(candidate, expected) {
		return ErrBinding
	}
	return nil
}

func validateInspectionSubmissionPermit(permit InspectionSubmissionPermit, identity InspectionIdentity, binding task.SupervisorRef) error {
	if identity.NumericTaskID != nil || permit.InspectionSubmissionRootID() != identity.RootID || permit.InspectionSubmissionTaskID() != identity.TaskID {
		return task.ErrIdentityMismatch
	}
	if err := validateInspectionQueuePermit(permit, identity, binding); err != nil {
		return err
	}
	return validateInspectionPermitSupervisor(permit.InspectionSubmissionSupervisor(), binding)
}

func validateInspectionStopPermit(permit InspectionStopPermit, identity InspectionIdentity, binding task.SupervisorRef) error {
	if permit.InspectionStopRootID() != identity.RootID || permit.InspectionStopTaskID() != identity.TaskID {
		return task.ErrIdentityMismatch
	}
	permitTarget := permit.InspectionStopNumericTaskID()
	if permitTarget == nil || *permitTarget != *identity.NumericTaskID {
		return task.ErrIdentityMismatch
	}
	if err := validateInspectionQueuePermit(permit, identity, binding); err != nil {
		return err
	}
	return validateInspectionPermitSupervisor(permit.InspectionStopSupervisor(), binding)
}

func validateInspectionQueuePermit(permit any, identity InspectionIdentity, binding task.SupervisorRef) error {
	group := inspectionGroup(identity.RootID)
	if scoped, ok := permit.(interface{ InspectionQueueGroup() string }); ok {
		group = scoped.InspectionQueueGroup()
	}
	if group != identity.Group() {
		return task.ErrIdentityMismatch
	}
	return validateInspectionQueueBinding(identity, binding)
}

func validateInspectionQueueBinding(identity InspectionIdentity, binding task.SupervisorRef) error {
	if identity.Group() == "default" {
		if _, private := privateStateRoot(binding.ConfigPath); !private {
			return ErrConfiguration
		}
	}
	return nil
}

func inspectionStopAction(state State) (string, error) {
	switch state {
	case StateStashed, StateQueued:
		return "remove", nil
	case StateRunning:
		return "kill", nil
	case StateEnded:
		return "", nil
	case StateUnknown:
		return "", ErrUnknown
	default:
		return "", ErrUnknown
	}
}
