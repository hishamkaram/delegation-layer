package pueue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const inspectionGroupPrefix = "delegation-inspection-"

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

// InspectionIdentity binds one inspection worker to one root-scoped group.
// NumericTaskID is optional while preparing a submission and required to stop
// an already admitted worker.
type InspectionIdentity struct {
	RootID        string
	TaskID        string
	NumericTaskID *int64
}

// Group returns the exact supervisor group derived from the complete root ID.
func (i InspectionIdentity) Group() string { return inspectionGroup(i.RootID) }

// Label returns the exact supervisor label derived from the task ID.
func (i InspectionIdentity) Label() string { return i.Group() + "-" + i.TaskID }

// InspectionObservation is positive only when one exact labeled job belongs
// to the expected root group and that group is running with one worker slot.
type InspectionObservation struct {
	Binding  task.SupervisorRef
	Identity InspectionIdentity
	Job      Job
	Matched  bool
	Pending  *Pending
}

func inspectionGroup(rootID string) string { return inspectionGroupPrefix + rootID }

// Snapshot verifies the pinned binding, then obtains and parses one complete
// status JSON response. The command remains owned by the process observer if
// the caller's finite observation expires.
func (c *Client) Snapshot(ctx context.Context) (QueueSnapshot, error) {
	if err := c.verify(ctx); err != nil {
		return QueueSnapshot{}, err
	}
	return c.snapshotAfterVerify(ctx)
}

func (c *Client) snapshotAfterVerify(ctx context.Context) (QueueSnapshot, error) {
	result, err := c.command(ctx, nil, "status", "--json")
	if err != nil {
		return QueueSnapshot{}, errors.Join(ErrUnknown, err)
	}
	return ParseQueueSnapshot(result.Stdout, c.binding.ObservedVersion)
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
// root-scoped running single-slot group. Duplicate, stale, malformed or
// unknown rows remain unknown.
func (c *Client) ReconcileInspection(ctx context.Context, identity InspectionIdentity) (InspectionObservation, error) {
	identity = snapshotInspectionIdentity(identity)
	unknown := InspectionObservation{Binding: c.binding, Identity: identity}
	if err := validateInspectionIdentity(identity); err != nil {
		return unknown, err
	}
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	return matchingInspectionObservation(snapshot, identity, c.binding)
}

func matchingInspectionObservation(snapshot QueueSnapshot, identity InspectionIdentity, binding task.SupervisorRef) (InspectionObservation, error) {
	identity = snapshotInspectionIdentity(identity)
	unknown := InspectionObservation{Binding: binding, Identity: identity}
	if err := validateInspectionIdentity(identity); err != nil {
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
	if !ok || group.Status != "Running" || group.ParallelTasks != 1 {
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
// worker are validated, the binding is fresh and the derived group is proven
// running with one slot. Admission is create-once and is reconciled without a
// retry after the supervisor response.
func (c *Client) SubmitInspection(ctx context.Context, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch) (InspectionObservation, error) {
	identity = snapshotInspectionIdentity(identity)
	unknown := InspectionObservation{Binding: c.binding, Identity: identity}
	if err := validateInspectionIdentity(identity); err != nil {
		return unknown, err
	}
	if permit == nil {
		return unknown, task.ErrInvalidPermit
	}
	if err := validateInspectionSubmissionPermit(permit, identity, c.binding); err != nil {
		return unknown, err
	}
	if err := validateLaunch(launch, identity.RootID); err != nil {
		return unknown, err
	}
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
	result, err := c.command(ctx, permit.ConsumeInspectionSubmission, "add", "--escape", "--group", identity.Group(), "--label", identity.Label(), "--print-task-id", "--", launch.RunnerExecutable, "--inspection", "--root", launch.RootPath, identity.TaskID)
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, errors.Join(ErrUnknown, err)
	}
	id, err := parseID(strings.TrimSpace(string(result.Stdout)))
	if err != nil {
		return unknown, err
	}
	identity.NumericTaskID = &id
	return c.ReconcileInspection(ctx, identity)
}

// StopInspection routes one saved inspection target to remove or kill after a
// fresh exact reconciliation. Ended workers are a no-op; uncertain routing
// consumes no new authority and never broadens to a group action.
func (c *Client) StopInspection(ctx context.Context, permit InspectionStopPermit, identity InspectionIdentity) (StopResult, error) {
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
	observation, err := c.ReconcileInspection(ctx, identity)
	result.Pending = observation.Pending
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
	if err != nil || current != c.binding {
		return result, errors.Join(ErrBinding, err)
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
	if candidate != expected {
		return ErrBinding
	}
	return nil
}

func validateInspectionSubmissionPermit(permit InspectionSubmissionPermit, identity InspectionIdentity, binding task.SupervisorRef) error {
	if identity.NumericTaskID != nil || permit.InspectionSubmissionRootID() != identity.RootID || permit.InspectionSubmissionTaskID() != identity.TaskID {
		return task.ErrIdentityMismatch
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
	return validateInspectionPermitSupervisor(permit.InspectionStopSupervisor(), binding)
}

func inspectionStopAction(state State) (string, error) {
	switch state {
	case StateQueued:
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
