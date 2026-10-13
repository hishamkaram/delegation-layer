package pueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// SubmissionGate lets the application validate its own generation evidence
// while the private supervisor's shared generation lease is still held.
type SubmissionGate func(context.Context, task.SupervisorRef) error

func (c *Client) checkSubmissionGate(ctx context.Context, gate SubmissionGate) error {
	if gate == nil {
		return nil
	}
	return gate(ctx, c.runtimeBinding)
}

// Receipt constructs a record only from a positive full-binding observation.
func (o Observation) Receipt() (*task.SupervisorReceipt, error) {
	if !o.Matched || o.NumericTaskID < 0 {
		return nil, ErrUnknown
	}
	if err := validateIdentity(o.Identity); err != nil {
		return nil, err
	}
	if err := task.ValidateFreshSupervisorRef(o.Binding); err != nil {
		return nil, err
	}
	return &task.SupervisorReceipt{
		SchemaVersion: task.SchemaVersion, RootID: o.Identity.RootID, TaskID: o.Identity.TaskID,
		SpecSHA256: o.Identity.SpecSHA256, MetaSHA256: o.Identity.MetaSHA256,
		NumericTaskID: o.NumericTaskID, Label: o.Identity.Label(),
		ConfigDigest: o.Binding.ConfigDigest, ConfigPath: o.Binding.ConfigPath,
		Endpoint: o.Binding.Endpoint, ObservedVersion: o.Binding.ObservedVersion,
		ClientExecutable: o.Binding.ClientExecutable, ClientSHA256: o.Binding.ClientSHA256,
		DaemonExecutable: o.Binding.DaemonExecutable, DaemonSHA256: o.Binding.DaemonSHA256,
		ResolutionOS: o.Binding.ResolutionOS, ResolutionHome: o.Binding.ResolutionHome,
		ResolutionDataLocal: o.Binding.ResolutionDataLocal, ResolutionConfig: o.Binding.ResolutionConfig,
		ResolutionRuntime: o.Binding.ResolutionRuntime, ResolutionUsername: o.Binding.ResolutionUsername,
		ResolutionCwd:        o.Binding.ResolutionCwd,
		ResolvedConfigSHA256: o.Binding.ResolvedConfigSHA256,
	}, nil
}

// Submit consumes only a real newly-created submission permit. Pueue publishes
// the assigned numeric ID only after add has accepted the task; that completed
// response is the admission receipt. A queue-wide status scan here would add
// no authority and would make concurrent submissions contend quadratically.
func (c *Client) Submit(ctx context.Context, permit *taskdir.SubmissionPermit, launch Launch) (Observation, error) {
	return c.SubmitWithGate(ctx, permit, launch, nil)
}

// SubmitWithGate runs the application-owned admission gate immediately before
// add while holding the same generation lease.
func (c *Client) SubmitWithGate(ctx context.Context, permit *taskdir.SubmissionPermit, launch Launch, gate SubmissionGate) (Observation, error) {
	if c == nil {
		return Observation{}, ErrConfiguration
	}
	controlContext, cancel, err := c.privateControlContext(ctx)
	if err != nil {
		return Observation{}, err
	}
	defer cancel()
	unknown := Observation{State: StateUnknown, Binding: c.binding}
	var observation Observation
	err = c.withPrivateSupervisorLease(controlContext, func(lockedContext context.Context) error {
		observation, err = c.submitLocked(lockedContext, permit, launch, gate)
		return err
	})
	if observation.Binding.ConfigPath == "" {
		observation = unknown
	}
	return observation, err
}

// RemoveCompletedTask removes one exact terminal task row from the private
// supervisor. Delegate task records remain the durable history; the Pueue row
// is only an operational handle and must not be retained indefinitely.
//
// The caller must have already published a terminal outcome and provide the
// complete task identity from that task's supervisor receipt. A failed or
// uncertain removal is returned to the caller so it can retain the task for a
// later bounded cleanup attempt.
func (c *Client) RemoveCompletedTask(ctx context.Context, identity Identity) error {
	identity = snapshotIdentity(identity)
	if c == nil {
		return ErrConfiguration
	}
	if !isPrivateClient(c) {
		return ErrConfiguration
	}
	if identity.NumericTaskID == nil || *identity.NumericTaskID < 0 {
		return ErrUnknown
	}
	controlContext, cancel, err := c.privateControlContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	// Pueue can reuse numeric IDs after removal. Exclude submission and other
	// removers from the identity observation through completion of removal.
	return c.withPrivateBootstrapLock(controlContext, func(lockedContext context.Context) error {
		if err := c.verify(lockedContext); err != nil {
			return err
		}
		observation, reconcileErr := c.Reconcile(lockedContext, identity)
		if reconcileErr != nil {
			return reconcileErr
		}
		if err := validateCompletedTaskRemoval(observation, *identity.NumericTaskID); err != nil {
			return err
		}
		numericTaskID := *identity.NumericTaskID
		result, commandErr := c.command(lockedContext, nil, "remove", strconv.FormatInt(numericTaskID, 10))
		if pending := pendingFrom(commandErr); pending != nil {
			settled, waitErr := awaitPending(lockedContext, pending)
			result = settled
			commandErr = errors.Join(commandErr, waitErr)
		}
		if commandErr != nil {
			return errors.Join(commandErr, result.Err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("%w: remove completed supervisor task %d exited with %d", ErrBinding, numericTaskID, result.ExitCode)
		}
		return nil
	})
}

func validateCompletedTaskRemoval(observation Observation, numericTaskID int64) error {
	if !observation.Matched || observation.NumericTaskID != numericTaskID {
		return ErrUnknown
	}
	// The runner writes Delegate's terminal outcome before it exits. The
	// authoritative outcome can therefore be available while Pueue still
	// reports this exact row as running. Do not remove a live row; later
	// private maintenance will retry after the row reaches its terminal state.
	if observation.State == StateRunning {
		return ErrCleanupDeferred
	}
	if observation.State != StateEnded {
		return ErrUnknown
	}
	return nil
}

func (c *Client) submitLocked(ctx context.Context, permit *taskdir.SubmissionPermit, launch Launch, gate SubmissionGate) (Observation, error) {
	unknown := Observation{State: StateUnknown, Binding: c.binding}
	if permit == nil {
		return unknown, task.ErrInvalidPermit
	}
	record, err := permit.Record()
	if err != nil {
		return unknown, err
	}
	identity := Identity{RootID: record.RootID, TaskID: record.TaskID, SpecSHA256: record.SpecSHA256, MetaSHA256: record.MetaSHA256}
	unknown.Identity = identity
	if record.Supervisor != c.binding {
		return unknown, ErrBinding
	}
	if launchErr := validateLaunch(launch, record.RootID); launchErr != nil {
		return unknown, launchErr
	}
	if verifyErr := c.verify(ctx); verifyErr != nil {
		unknown.Pending = pendingFrom(verifyErr)
		return unknown, verifyErr
	}
	if gateErr := c.checkSubmissionGate(ctx, gate); gateErr != nil {
		return unknown, gateErr
	}
	runnerArguments := []string{launch.RunnerExecutable}
	if launch.Supervised {
		runnerArguments = append(runnerArguments, "--guardian", "--supervised")
	}
	runnerArguments = append(runnerArguments, "--root", launch.RootPath, record.TaskID)
	arguments := []string{"add", "--escape", "--label", record.Label, "--print-task-id", "--"}
	result, err := c.command(ctx, permit.Consume, append(arguments, runnerArguments...)...)
	if err != nil {
		unknown.Pending = pendingFrom(err)
		if !result.Started && !errors.Is(err, ErrInFlight) {
			return unknown, err
		}
		return unknown, errors.Join(ErrUnknown, ErrSubmissionUncertain, err)
	}
	id, err := parseID(strings.TrimSpace(string(result.Stdout)))
	if err != nil {
		return unknown, errors.Join(ErrUnknown, ErrSubmissionUncertain, err)
	}
	identity.NumericTaskID = &id
	return Observation{
		Matched: true, State: StateQueued, Identity: identity,
		Binding: c.binding, NumericTaskID: id,
	}, nil
}

func validateLaunch(launch Launch, rootID string) error {
	resolved, err := canonicalExecutable(launch.RunnerExecutable)
	if err != nil || resolved != launch.RunnerExecutable {
		return errors.Join(ErrConfiguration, err)
	}
	if !filepath.IsAbs(launch.RootPath) || filepath.Clean(launch.RootPath) != launch.RootPath {
		return ErrConfiguration
	}
	if symlinkErr := rejectSymlinkComponents(launch.RootPath); symlinkErr != nil {
		return symlinkErr
	}
	info, err := os.Stat(launch.RootPath)
	if err != nil || !info.IsDir() {
		return errors.Join(ErrConfiguration, err)
	}
	data, err := readRegular(filepath.Join(launch.RootPath, "root.json"), MaxControlBytes)
	if err != nil {
		return err
	}
	var record task.RootRecord
	if err := task.DecodeStrict(data, &record); err != nil {
		return err
	}
	if err := task.ValidateRootRecord(&record); err != nil {
		return err
	}
	if record.RootID != rootID {
		return task.ErrIdentityMismatch
	}
	return nil
}

// Stop routes one immutable request to exactly one freshly revalidated job.
// A request without an already-bound numeric target remains unknown forever;
// discovering a later target requires a separately authorized new request.
func (c *Client) Stop(ctx context.Context, permit *taskdir.StopPermit) (StopResult, error) {
	return c.withPrivateStop(ctx, func(controlContext context.Context) (StopResult, error) {
		return c.stopLocked(controlContext, permit)
	})
}

// withPrivateStop keeps exact identity observation and a numeric mutation in
// one exclusive scope. Pueue reuses removed IDs; submissions and cleanup must
// not change the target between those two operations. Pending errors retain
// the same lock until the command settles.
func (c *Client) withPrivateStop(ctx context.Context, stop func(context.Context) (StopResult, error)) (StopResult, error) {
	result := StopResult{ObservedState: StateUnknown}
	controlContext, cancel, err := c.privateControlContext(ctx)
	if err != nil {
		return result, err
	}
	defer cancel()
	err = c.withPrivateBootstrapLock(controlContext, func(lockedContext context.Context) error {
		result, err = stop(lockedContext)
		return err
	})
	return result, err
}

func (c *Client) stopLocked(ctx context.Context, permit *taskdir.StopPermit) (StopResult, error) {
	result := StopResult{ObservedState: StateUnknown}
	if permit == nil {
		return result, task.ErrInvalidPermit
	}
	request, err := permit.Request()
	if err != nil {
		return result, err
	}
	result.Requested = true
	if request.Supervisor != c.binding {
		return result, ErrBinding
	}
	if request.NumericTaskID == nil {
		return result, fmt.Errorf("%w: stop request has no saved numeric target", ErrUnknown)
	}
	id := *request.NumericTaskID
	result.NumericTaskID = &id
	identity := Identity{RootID: request.RootID, TaskID: request.TaskID, SpecSHA256: request.SpecSHA256, MetaSHA256: request.MetaSHA256, NumericTaskID: &id}
	observation, err := settlePendingControlObservation(c, ctx, func(controlContext context.Context) (Observation, error) {
		return c.Reconcile(controlContext, identity)
	})
	result.Pending = pendingFrom(err)
	result.InFlight = result.Pending != nil
	if err != nil {
		return result, err
	}
	result.Matched, result.ObservedState = observation.Matched, observation.State
	if !observation.Matched {
		return result, ErrUnknown
	}
	return c.stopMatched(ctx, permit, result)
}

func (c *Client) stopMatched(ctx context.Context, permit *taskdir.StopPermit, result StopResult) (StopResult, error) {
	switch result.ObservedState {
	case StateStashed, StateQueued:
		result.Action = "remove"
	case StateRunning:
		result.Action = "kill"
	case StateEnded:
		return result, nil
	case StateUnknown:
		return result, ErrUnknown
	default:
		return result, ErrUnknown
	}
	// Recheck immutable files after the external observation, without replacing it
	// with an add, fallback endpoint or second mutation attempt.
	current, err := c.readBinding()
	if err != nil || current != c.runtimeBinding {
		return result, errors.Join(ErrBinding, err)
	}
	consume := func() error {
		if consumeErr := permit.Consume(); consumeErr != nil {
			return consumeErr
		}
		result.Attempted = true
		return nil
	}
	command, err := c.command(ctx, consume, result.Action, strconv.FormatInt(*result.NumericTaskID, 10))
	if p := pendingFrom(err); p != nil {
		result.Pending, result.InFlight = p, true
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

func commandAcknowledgment(result CommandResult, err error) *bool {
	if errors.Is(err, ErrControlLimit) || !result.Started {
		return nil
	}
	if err == nil && result.ExitCode == 0 {
		acknowledged := true
		return &acknowledged
	}
	if completedExit(err) {
		acknowledged := false
		return &acknowledged
	}
	return nil
}
