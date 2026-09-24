package pueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

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

// Submit consumes only a real newly-created submission permit. A completed add
// response is followed by status reconciliation; no response alone proves admission.
func (c *Client) Submit(ctx context.Context, permit *taskdir.SubmissionPermit, launch Launch) (Observation, error) {
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
	result, err := c.command(ctx, permit.Consume, "add", "--escape", "--label", record.Label, "--print-task-id", "--", launch.RunnerExecutable, "--root", launch.RootPath, record.TaskID)
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
	observation, err := c.Reconcile(ctx, identity)
	if err != nil && !observation.Matched {
		return observation, errors.Join(ErrUnknown, ErrSubmissionUncertain, err)
	}
	if !observation.Matched {
		return observation, errors.Join(ErrUnknown, ErrSubmissionUncertain)
	}
	return observation, err
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
	observation, err := c.Reconcile(ctx, identity)
	result.Pending = observation.Pending
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
	case StateQueued:
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
	if err != nil || current != c.binding {
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
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil && exitErr.Exited() {
		acknowledged := false
		return &acknowledged
	}
	return nil
}
