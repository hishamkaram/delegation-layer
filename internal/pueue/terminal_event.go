package pueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const (
	privateTerminalEventRunnerName = "delegate-terminal-event-v1"
	privateTerminalEventLockName   = "terminal-events.lock"
	privateTerminalEventTicketDir  = "terminal-event-tickets"
	privateTerminalEventRecordDir  = "terminal-events"
)

// TerminalEvent is Pueue's compact, callback-originated wake-up hint for one
// classified queue row. Pueue supplies only a reusable numeric ID and an exit
// code here; Succeeded is diagnostic and never an admission proof. The
// application must reconcile the current labeled queue row before recording
// worker-exit authority.
type TerminalEvent struct {
	SchemaVersion int   `json:"schema_version"`
	NumericTaskID int64 `json:"numeric_task_id"`
	Succeeded     bool  `json:"succeeded"` // callback diagnostic; never admission authority
}

// terminalEventTicket classifies one live numeric ID for callback publication.
// Cleanup clears it only after removing the event-backed terminal row. Every
// new submission retains the ticket and resets the reused ID's event before
// enqueue because the callback surface has no per-submission token.
type terminalEventTicket struct {
	SchemaVersion int   `json:"schema_version"`
	NumericTaskID int64 `json:"numeric_task_id"`
}

// PrivateTerminalEventRunnerPath is the stable state-root helper path invoked
// by Pueue's private terminal callback. The helper protocol is deliberately
// versioned so future incompatible changes require an explicit config
// migration rather than silently changing a live daemon callback.
func PrivateTerminalEventRunnerPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateTerminalEventRunnerName)
}

// PrivateTerminalEventCallback returns the exact Pueue callback command for a
// Delegate-owned state root. It contains only state-root paths and Pueue's
// constrained numeric task/exit-code template fields; provider command text
// is never interpolated into the callback shell command.
func PrivateTerminalEventCallback(stateRoot string) (string, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return "", ErrConfiguration
	}
	return privateTerminalEventCallback(filepath.Join(stateRoot, privateSupervisorDirectory)), nil
}

func privateTerminalEventCallback(base string) string {
	stateRoot := filepath.Dir(base)
	return escapedCommand(
		filepath.Join(base, privateTerminalEventRunnerName),
		"--terminal-event",
		"--root", stateRoot,
		"--pueue-id", "{{id}}",
		"--exit-code", "{{exit_code}}",
	)
}

func privateTerminalEventsConfigured(base string, callback *string) bool {
	return callback != nil && *callback == privateTerminalEventCallback(base)
}

func privateTerminalEventBase(stateRoot string) (string, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot || !IsPrivateConfig(stateRoot, PrivateConfigPath(stateRoot)) {
		return "", ErrConfiguration
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		return "", err
	}
	return base, nil
}

// privateTerminalEventExistingBase verifies the already-published private
// supervisor directory without creating any state. Pueue invokes the callback
// for every task, including legacy rows without tickets. Keeping the no-ticket
// path free of directory creation and locking avoids manufacturing callback
// state while observing an unclassified row.
func privateTerminalEventExistingBase(stateRoot string) (string, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot || !IsPrivateConfig(stateRoot, PrivateConfigPath(stateRoot)) {
		return "", ErrConfiguration
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := rejectSymlinkComponents(base); err != nil {
		return "", err
	}
	info, err := os.Lstat(base)
	if err != nil {
		return "", fmt.Errorf("%w: inspect private terminal event base: %w", ErrConfiguration, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", ErrConfiguration
	}
	return base, nil
}

func terminalEventName(id int64) (string, error) {
	if id < 0 {
		return "", ErrUnknown
	}
	return strconv.FormatInt(id, 10) + ".json", nil
}

func privateTerminalEventPath(base, directory string, id int64) (string, error) {
	name, err := terminalEventName(id)
	if err != nil {
		return "", err
	}
	if directory != privateTerminalEventTicketDir && directory != privateTerminalEventRecordDir {
		return "", ErrConfiguration
	}
	return filepath.Join(base, directory, name), nil
}

func ensurePrivateTerminalEventDirectory(base, name string) (string, error) {
	if name != privateTerminalEventTicketDir && name != privateTerminalEventRecordDir {
		return "", ErrConfiguration
	}
	directory := filepath.Join(base, name)
	if err := rejectSymlinkComponents(directory); err != nil {
		return "", err
	}
	created := false
	if err := os.Mkdir(directory, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("%w: create private terminal event directory: %w", ErrConfiguration, err)
		}
	} else {
		created = true
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", fmt.Errorf("%w: inspect private terminal event directory: %w", ErrConfiguration, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", ErrConfiguration
	}
	if created {
		if err = syncPrivateDirectory(filepath.Dir(directory)); err != nil {
			return "", err
		}
	}
	return directory, nil
}

func withPrivateTerminalEventLock(ctx context.Context, stateRoot string, callback func(string) error) error {
	if callback == nil {
		return ErrConfiguration
	}
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Terminal-event state is optional for a private root. Provision its lock
	// only on the ticket/event mutation path: generic private-base validation is
	// also used by read-only recovery and must not manufacture callback files.
	base, err := privateTerminalEventBase(stateRoot)
	if err != nil {
		return err
	}
	if err = ensurePrivateLockFile(base, privateTerminalEventLockName); err != nil {
		return err
	}
	return withPublishedPrivateLock(ctx, stateRoot, privateTerminalEventLockName, unix.LOCK_EX, func() error {
		base, err := privateTerminalEventBase(stateRoot)
		if err != nil {
			return err
		}
		if err = recoverPrivateTerminalEventStages(base); err != nil {
			return err
		}
		return callback(base)
	})
}

// withExistingPrivateTerminalEventLock observes callback state only when a
// ticket/event mutation has already published its lock. A missing lock means
// there can be no durable callback state to consume; callers fall back to
// queue reconciliation without making a read path provision optional files.
func withExistingPrivateTerminalEventLock(ctx context.Context, stateRoot string, mode int, callback func(string) error) (present bool, resultErr error) {
	if callback == nil {
		return false, ErrConfiguration
	}
	if mode != unix.LOCK_SH && mode != unix.LOCK_EX {
		return false, ErrConfiguration
	}
	if ctx == nil {
		return false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	base, err := privateTerminalEventExistingBase(stateRoot)
	if err != nil {
		return false, err
	}
	present, err = privateLockFilePresent(base, privateTerminalEventLockName)
	if err != nil || !present {
		return present, err
	}
	resultErr = withPublishedPrivateLock(ctx, stateRoot, privateTerminalEventLockName, mode, func() error {
		return callback(base)
	})
	return present, resultErr
}

func writeTerminalEventTicket(base string, id int64) error {
	directory, err := ensurePrivateTerminalEventDirectory(base, privateTerminalEventTicketDir)
	if err != nil {
		return err
	}
	path, err := privateTerminalEventPath(base, privateTerminalEventTicketDir, id)
	if err != nil {
		return err
	}
	data, err := task.MarshalCanonical(terminalEventTicket{SchemaVersion: task.SchemaVersion, NumericTaskID: id})
	if err != nil {
		return err
	}
	return publishPrivateRecord(directory, path, data)
}

func readTerminalEventTicket(base string, id int64) (bool, error) {
	path, err := privateTerminalEventPath(base, privateTerminalEventTicketDir, id)
	if err != nil {
		return false, err
	}
	data, err := readRegular(path, MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var ticket terminalEventTicket
	if err = task.DecodeStrict(data, &ticket); err != nil || ticket.SchemaVersion != task.SchemaVersion || ticket.NumericTaskID != id {
		return false, ErrBinding
	}
	return true, nil
}

func writeTerminalEvent(base string, event TerminalEvent) error {
	directory, err := ensurePrivateTerminalEventDirectory(base, privateTerminalEventRecordDir)
	if err != nil {
		return err
	}
	path, err := privateTerminalEventPath(base, privateTerminalEventRecordDir, event.NumericTaskID)
	if err != nil {
		return err
	}
	data, err := task.MarshalCanonical(event)
	if err != nil {
		return err
	}
	return publishPrivateRecord(directory, path, data)
}

func readTerminalEvent(base string, id int64) (TerminalEvent, bool, error) {
	path, err := privateTerminalEventPath(base, privateTerminalEventRecordDir, id)
	if err != nil {
		return TerminalEvent{}, false, err
	}
	data, err := readRegular(path, MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return TerminalEvent{}, false, nil
	}
	if err != nil {
		return TerminalEvent{}, false, err
	}
	var event TerminalEvent
	if err = task.DecodeStrict(data, &event); err != nil || event.SchemaVersion != task.SchemaVersion || event.NumericTaskID != id {
		return TerminalEvent{}, false, ErrBinding
	}
	return event, true, nil
}

func removeTerminalEventRecord(base, directory string, id int64) (bool, error) {
	path, err := privateTerminalEventPath(base, directory, id)
	if err != nil {
		return false, err
	}
	if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func privateTerminalEventsEnabled(ctx context.Context, c *Client) (string, bool, error) {
	if ctx == nil {
		return "", false, context.Canceled
	}
	if c == nil {
		return "", false, ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return "", false, nil
	}
	resolution, err := c.resolution()
	if err != nil {
		return "", false, err
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	resolved, err := readPrivateConfig(c.binding.ConfigPath, base, resolution)
	if err != nil {
		return "", false, err
	}
	return stateRoot, privateTerminalEventsConfigured(base, resolved.Daemon.Callback), nil
}

// PrivateTerminalEventsEnabled reports whether this client is bound to the
// exact callback contract that can emit terminal events. Legacy
// private roots remain valid and use queue reconciliation as their fallback.
func (c *Client) PrivateTerminalEventsEnabled(ctx context.Context) (bool, error) {
	_, enabled, err := privateTerminalEventsEnabled(ctx, c)
	return enabled, err
}

// RegisterPrivateTerminalEvent opts one row into terminal callback delivery.
// The ticket is written after Pueue assigns its numeric ID and stale payload is
// retired before enqueue.
func (c *Client) RegisterPrivateTerminalEvent(ctx context.Context, id int64) error {
	stateRoot, enabled, err := privateTerminalEventsEnabled(ctx, c)
	if err != nil || !enabled {
		return err
	}
	return withPrivateTerminalEventLock(ctx, stateRoot, func(base string) error {
		if err := writeTerminalEventTicket(base, id); err != nil {
			return err
		}
		changed, err := removePrivateTerminalEventRecordsFromDirectories(base, []int64{id}, privateTerminalEventRecordDir)
		if err != nil {
			return err
		}
		return syncPrivateTerminalEventDirectories(base, changed)
	})
}

// RegisterPrivateOrdinaryTerminalEvent classifies a newly allocated ordinary
// row by retaining its callback ticket and clearing any event left by a prior
// use of its numeric ID. Callers perform this required step before enqueue
// while holding the queue identity lease.
func (c *Client) RegisterPrivateOrdinaryTerminalEvent(ctx context.Context, id int64) error {
	return c.RegisterPrivateTerminalEvent(ctx, id)
}

// RecordPrivateTerminalObservation publishes the same bounded candidate hint
// when the application has already reconciled an exact ended inspection row.
// It revalidates the immutable inspection identity while queue-ID reuse is
// excluded, then publishes only for that still-current row. The application
// cleanup authorizer must still validate its own durable task evidence before
// Pueue can remove the row.
func (c *Client) RecordPrivateTerminalObservation(ctx context.Context, observation InspectionObservation) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if err := validatePrivateTerminalObservation(c, observation); err != nil {
		return false, err
	}
	observation.Identity = snapshotInspectionIdentity(observation.Identity)
	stateRoot, enabled, err := privateTerminalEventsEnabled(ctx, c)
	if err != nil || !enabled {
		return false, err
	}
	found, readErr := c.privateTerminalObservationRecorded(ctx, observation.Job.ID)
	if readErr != nil || found {
		return false, readErr
	}
	return c.publishPrivateTerminalObservation(ctx, stateRoot, observation)
}

func validatePrivateTerminalObservation(c *Client, observation InspectionObservation) error {
	if c == nil || !observation.Matched || observation.Binding != c.binding || observation.Job.State != StateEnded {
		return ErrConfiguration
	}
	return validateInspectionIdentity(snapshotInspectionIdentity(observation.Identity))
}

func (c *Client) privateTerminalObservationRecorded(ctx context.Context, id int64) (bool, error) {
	_, found, err := c.ReadPrivateTerminalEvent(ctx, id)
	return found, err
}

func (c *Client) publishPrivateTerminalObservation(ctx context.Context, stateRoot string, observation InspectionObservation) (bool, error) {
	var published bool
	leaseErr := c.withPrivateSupervisorLease(ctx, func(lockedContext context.Context) error {
		return c.withPrivateQueueIdentityLease(lockedContext, func(identityContext context.Context) error {
			var eventErr error
			published, eventErr = c.publishPrivateTerminalObservationLocked(identityContext, stateRoot, observation)
			return eventErr
		})
	})
	return published, leaseErr
}

func (c *Client) publishPrivateTerminalObservationLocked(ctx context.Context, stateRoot string, observation InspectionObservation) (bool, error) {
	current, reconcileErr := c.reconcileInspectionLocked(ctx, observation.Identity)
	if reconcileErr != nil && current.Pending == nil && errors.Is(reconcileErr, ErrUnknown) {
		return false, nil
	}
	if reconcileErr != nil {
		return false, reconcileErr
	}
	if !current.Matched {
		return false, nil
	}
	if current.Job.ID != observation.Job.ID || !sameTerminalQueueJob(observation.Job, current.Job) {
		return false, task.ErrIdentityMismatch
	}
	return writePrivateTerminalObservationEvent(ctx, stateRoot, current.Job)
}

func writePrivateTerminalObservationEvent(ctx context.Context, stateRoot string, job Job) (bool, error) {
	published := false
	err := withPrivateTerminalEventLock(ctx, stateRoot, func(base string) error {
		if writeErr := writeTerminalEventTicket(base, job.ID); writeErr != nil {
			return writeErr
		}
		_, found, readErr := readTerminalEvent(base, job.ID)
		if readErr != nil || found {
			return readErr
		}
		if writeErr := writeTerminalEvent(base, TerminalEvent{
			SchemaVersion: task.SchemaVersion,
			NumericTaskID: job.ID,
			Succeeded:     job.Succeeded,
		}); writeErr != nil {
			return writeErr
		}
		published = true
		return nil
	})
	return published, err
}

// RecordPrivateTerminalEvent is the state-root callback receiver used by the
// fixed delegate-run helper. It writes only a wake-up hint when a prior
// submission created the matching ticket. Callers must reconcile the current
// queue row before treating the callback as terminal evidence.
func RecordPrivateTerminalEvent(ctx context.Context, stateRoot string, id int64, exitCode string) error {
	_, err := recordPrivateTerminalEvent(ctx, stateRoot, id, exitCode)
	return err
}

// RecordPrivateTerminalEventForMaintenance reports whether this callback
// published a durable candidate event. The caller uses that fact only to
// select an event-backed cleanup pass; application journals still own every
// removal decision.
func RecordPrivateTerminalEventForMaintenance(ctx context.Context, stateRoot string, id int64, exitCode string) (bool, error) {
	return recordPrivateTerminalEvent(ctx, stateRoot, id, exitCode)
}

func recordPrivateTerminalEvent(ctx context.Context, stateRoot string, id int64, exitCode string) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	succeeded, err := terminalEventSucceeded(exitCode)
	if err != nil {
		return false, err
	}
	base, err := privateTerminalEventExistingBase(stateRoot)
	if err != nil {
		return false, err
	}
	// The first unlocked read is intentionally only a fast negative filter.
	// A callback racing ticket publication can miss it, in which case the
	// caller falls back to normal queue reconciliation. A present ticket is
	// rechecked under the event lock before an event is published.
	present, err := readTerminalEventTicket(base, id)
	if err != nil || !present {
		return false, err
	}
	recorded := false
	err = withPrivateTerminalEventLock(ctx, stateRoot, func(base string) error {
		lockedPresent, ticketErr := readTerminalEventTicket(base, id)
		if ticketErr != nil || !lockedPresent {
			return ticketErr
		}
		if writeErr := writeTerminalEvent(base, TerminalEvent{SchemaVersion: task.SchemaVersion, NumericTaskID: id, Succeeded: succeeded}); writeErr != nil {
			return writeErr
		}
		recorded = true
		return nil
	})
	return recorded, err
}

func terminalEventSucceeded(exitCode string) (bool, error) {
	if exitCode == "0" {
		return true, nil
	}
	if exitCode == "None" {
		return false, nil
	}
	parsed, err := strconv.ParseInt(exitCode, 10, 32)
	if err != nil || strconv.FormatInt(parsed, 10) != exitCode || parsed == 0 {
		return false, ErrConfiguration
	}
	return false, nil
}

// ReadPrivateTerminalEvent reads one event without consuming it. Queue cleanup
// retires the ticket and event only after the exact terminal row is removed.
func (c *Client) ReadPrivateTerminalEvent(ctx context.Context, id int64) (TerminalEvent, bool, error) {
	stateRoot, enabled, err := privateTerminalEventsEnabled(ctx, c)
	if err != nil || !enabled {
		return TerminalEvent{}, false, err
	}
	var event TerminalEvent
	found := false
	lockPresent, err := withExistingPrivateTerminalEventLock(ctx, stateRoot, unix.LOCK_SH, func(base string) error {
		var readErr error
		event, found, readErr = readTerminalEvent(base, id)
		return readErr
	})
	if !lockPresent {
		return TerminalEvent{}, false, err
	}
	return event, found, err
}

// privateTerminalEventWakePending observes the durable callback inbox without
// provisioning its lock or directories. Every entry and record is validated
// before a non-empty inbox is reported.
func privateTerminalEventWakePending(ctx context.Context, stateRoot string) (bool, error) {
	ids, err := privateTerminalEventInboxIDs(ctx, stateRoot)
	if err != nil {
		return false, err
	}
	return len(ids) != 0, nil
}

func privateTerminalEventInboxIDs(ctx context.Context, stateRoot string) (map[int64]struct{}, error) {
	ids := make(map[int64]struct{})
	_, err := withExistingPrivateTerminalEventLock(ctx, stateRoot, unix.LOCK_EX, func(base string) error {
		if recoverErr := recoverPrivateTerminalEventStages(base); recoverErr != nil {
			return recoverErr
		}
		var readErr error
		ids, readErr = readPrivateTerminalEventInbox(base)
		return readErr
	})
	return ids, err
}

// clearPrivateTerminalEventsForAbsentRows retires only event-backed candidate
// IDs that are absent from the supplied full queue snapshot. The caller must
// hold the queue-identity mutation lease; the snapshot is authoritative for
// absence, while application journals remain the only removal authority.
func (c *Client) clearPrivateTerminalEventsForAbsentRows(ctx context.Context, snapshot QueueSnapshot) error {
	stateRoot, enabled, err := privateTerminalEventsEnabled(ctx, c)
	if err != nil || !enabled {
		return err
	}
	presentIDs := make(map[int64]struct{}, len(snapshot.Jobs))
	for _, job := range snapshot.Jobs {
		presentIDs[job.ID] = struct{}{}
	}
	return withPrivateTerminalEventLock(ctx, stateRoot, func(base string) error {
		candidateIDs, readErr := readPrivateTerminalEventInbox(base)
		if readErr != nil {
			return readErr
		}
		staleIDs := make([]int64, 0, len(candidateIDs))
		for id := range candidateIDs {
			if _, present := presentIDs[id]; !present {
				staleIDs = append(staleIDs, id)
			}
		}
		if len(staleIDs) == 0 {
			return nil
		}
		sort.Slice(staleIDs, func(left, right int) bool { return staleIDs[left] < staleIDs[right] })
		changed, removeErr := removePrivateTerminalEventRecords(base, staleIDs)
		if removeErr != nil {
			return removeErr
		}
		return syncPrivateTerminalEventDirectories(base, changed)
	})
}

func readPrivateTerminalEventInbox(base string) (map[int64]struct{}, error) {
	ids := make(map[int64]struct{})
	directory, present, err := privateTerminalEventExistingDirectory(base)
	if err != nil || !present {
		return ids, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: read private terminal event inbox: %w", ErrConfiguration, err)
	}
	for _, entry := range entries {
		id, entryErr := terminalEventIDFromEntry(entry.Name())
		if entryErr != nil {
			return nil, entryErr
		}
		_, found, readErr := readTerminalEvent(base, id)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			return nil, fmt.Errorf("%w: private terminal event disappeared during locked read", ErrBinding)
		}
		ids[id] = struct{}{}
	}
	return ids, nil
}

// recoverPrivateTerminalEventStages removes only publisher-owned temporary
// records after validating their reserved random name and file shape. Stages
// are never authority: committed ticket and event records retain their strict
// name and payload validation. The caller holds the terminal-event mutation
// lock, so a stage cannot still belong to an active publisher.
func recoverPrivateTerminalEventStages(base string) error {
	for _, name := range []string{privateTerminalEventTicketDir, privateTerminalEventRecordDir} {
		if err := recoverPrivateTerminalEventDirectoryStages(base, name); err != nil {
			return err
		}
	}
	return nil
}

func recoverPrivateTerminalEventDirectoryStages(base, name string) error {
	directory, present, err := privateTerminalEventExistingNamedDirectory(base, name)
	if err != nil || !present {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("%w: read private terminal event directory: %w", ErrConfiguration, err)
	}
	stages := make([]string, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), privateRecordStagePrefix) {
			continue
		}
		if err = validatePrivateTerminalEventStage(name, directory, entry.Name()); err != nil {
			return err
		}
		stages = append(stages, filepath.Join(directory, entry.Name()))
	}
	for _, stage := range stages {
		if err = os.Remove(stage); err != nil {
			return fmt.Errorf("%w: remove private terminal event stage: %w", ErrConfiguration, err)
		}
	}
	if len(stages) == 0 {
		return nil
	}
	return syncPrivateDirectory(directory)
}

func validatePrivateTerminalEventStage(directory, parent, name string) error {
	if directory != privateTerminalEventTicketDir && directory != privateTerminalEventRecordDir {
		return ErrConfiguration
	}
	if task.ValidateTaskID(strings.TrimPrefix(name, privateRecordStagePrefix)) != nil {
		return fmt.Errorf("%w: invalid private terminal event stage %q", ErrBinding, name)
	}
	stagePath := filepath.Join(parent, name)
	info, err := os.Lstat(stagePath)
	if err != nil {
		return fmt.Errorf("%w: inspect private terminal event stage: %w", ErrBinding, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o177 != 0 {
		return fmt.Errorf("%w: invalid private terminal event stage %q", ErrBinding, name)
	}
	return nil
}

func privateTerminalEventExistingDirectory(base string) (string, bool, error) {
	return privateTerminalEventExistingNamedDirectory(base, privateTerminalEventRecordDir)
}

func privateTerminalEventExistingNamedDirectory(base, name string) (string, bool, error) {
	if name != privateTerminalEventTicketDir && name != privateTerminalEventRecordDir {
		return "", false, ErrConfiguration
	}
	directory := filepath.Join(base, name)
	if err := rejectSymlinkComponents(directory); err != nil {
		return "", false, err
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return directory, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: inspect private terminal event inbox: %w", ErrConfiguration, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", false, ErrConfiguration
	}
	return directory, true, nil
}

func terminalEventIDFromEntry(name string) (int64, error) {
	const suffix = ".json"
	if !strings.HasSuffix(name, suffix) {
		return 0, fmt.Errorf("%w: invalid private terminal event entry %q", ErrBinding, name)
	}
	id, err := strconv.ParseInt(strings.TrimSuffix(name, suffix), 10, 64)
	if err != nil || id < 0 || strconv.FormatInt(id, 10)+suffix != name {
		return 0, fmt.Errorf("%w: invalid private terminal event entry %q", ErrBinding, name)
	}
	return id, nil
}

// privateTerminalEventJobs selects candidate labels by exact event-backed
// numeric ID during inbox processing, then preserves every row sharing those
// labels so the application can reject ambiguous identities. Recovery passes
// keep ordinary rows unchanged but expose an inspection row only after an
// exact callback or application observation has published its durable
// candidate event. The application still owns every removal decision.
func (c *Client) privateTerminalEventJobs(ctx context.Context, stateRoot string, jobs []Job, eventOnly bool) ([]Job, error) {
	enabled, err := c.PrivateTerminalEventsEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return jobs, nil
	}
	if eventOnly {
		candidateIDs, inboxErr := privateTerminalEventInboxIDs(ctx, stateRoot)
		if inboxErr != nil {
			return nil, inboxErr
		}
		return filterPrivateTerminalEventJobsByID(jobs, candidateIDs), nil
	}
	inspectionRows := endedPrivateInspectionJobs(jobs)
	if len(inspectionRows) == 0 {
		return filterPrivateOrdinaryJobs(jobs), nil
	}
	candidateLabels, lockPresent, err := privateTerminalEventCandidateLabels(ctx, stateRoot, inspectionRows)
	if err != nil {
		return nil, err
	}
	if !lockPresent {
		clear(candidateLabels)
	}
	return filterPrivateTerminalEventJobs(jobs, candidateLabels), nil
}

func filterPrivateTerminalEventJobsByID(jobs []Job, candidateIDs map[int64]struct{}) []Job {
	candidateLabels, candidateWithoutLabel := privateTerminalEventLabelsByID(jobs, candidateIDs)
	filtered := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if job.Label == nil {
			if candidateWithoutLabel {
				filtered = append(filtered, job)
			}
			continue
		}
		if _, candidate := candidateLabels[*job.Label]; candidate {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func privateTerminalEventLabelsByID(jobs []Job, candidateIDs map[int64]struct{}) (map[string]struct{}, bool) {
	candidateLabels := make(map[string]struct{}, len(candidateIDs))
	candidateWithoutLabel := false
	for _, job := range jobs {
		if _, candidate := candidateIDs[job.ID]; !candidate {
			continue
		}
		if job.Label == nil {
			candidateWithoutLabel = true
			continue
		}
		candidateLabels[*job.Label] = struct{}{}
	}
	return candidateLabels, candidateWithoutLabel
}

func endedPrivateInspectionJobs(jobs []Job) []Job {
	inspectionRows := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if job.Label != nil && strings.HasPrefix(*job.Label, inspectionGroupPrefix) && job.State == StateEnded {
			inspectionRows = append(inspectionRows, job)
		}
	}
	return inspectionRows
}

func filterPrivateOrdinaryJobs(jobs []Job) []Job {
	filtered := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if job.Label == nil || !strings.HasPrefix(*job.Label, inspectionGroupPrefix) {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func privateTerminalEventCandidateLabels(ctx context.Context, stateRoot string, jobs []Job) (map[string]struct{}, bool, error) {
	candidateLabels := make(map[string]struct{}, len(jobs))
	lockPresent, err := withExistingPrivateTerminalEventLock(ctx, stateRoot, unix.LOCK_SH, func(base string) error {
		for _, job := range jobs {
			_, found, readErr := readTerminalEvent(base, job.ID)
			if readErr != nil {
				return readErr
			}
			if found {
				candidateLabels[*job.Label] = struct{}{}
			}
		}
		return nil
	})
	return candidateLabels, lockPresent, err
}

func filterPrivateTerminalEventJobs(jobs []Job, candidateLabels map[string]struct{}) []Job {
	filtered := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if job.Label == nil || !strings.HasPrefix(*job.Label, inspectionGroupPrefix) {
			filtered = append(filtered, job)
			continue
		}
		if _, candidate := candidateLabels[*job.Label]; candidate {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

// ClearPrivateTerminalEvents resets callback classification after an exact row
// was removed or before a newly allocated numeric ID is enqueued. The shared
// lock closes the callback/classification race.
func (c *Client) ClearPrivateTerminalEvents(ctx context.Context, ids ...int64) error {
	stateRoot, enabled, err := privateTerminalEventsEnabled(ctx, c)
	if err != nil || !enabled || len(ids) == 0 {
		return err
	}
	return withPrivateTerminalEventLock(ctx, stateRoot, func(base string) error {
		changed, err := removePrivateTerminalEventRecords(base, ids)
		if err != nil {
			return err
		}
		return syncPrivateTerminalEventDirectories(base, changed)
	})
}

func removePrivateTerminalEventRecords(base string, ids []int64) (map[string]bool, error) {
	return removePrivateTerminalEventRecordsFromDirectories(base, ids, privateTerminalEventTicketDir, privateTerminalEventRecordDir)
}

func removePrivateTerminalEventRecordsFromDirectories(base string, ids []int64, directories ...string) (map[string]bool, error) {
	changed := make(map[string]bool, 2)
	for _, id := range ids {
		if err := removePrivateTerminalEventRecordsForID(base, id, changed, directories); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

func removePrivateTerminalEventRecordsForID(base string, id int64, changed map[string]bool, directories []string) error {
	if id < 0 {
		return ErrUnknown
	}
	for _, directory := range directories {
		if directory != privateTerminalEventTicketDir && directory != privateTerminalEventRecordDir {
			return ErrConfiguration
		}
		removed, err := removeTerminalEventRecord(base, directory, id)
		if err != nil {
			return err
		}
		changed[directory] = changed[directory] || removed
	}
	return nil
}

func syncPrivateTerminalEventDirectories(base string, changed map[string]bool) error {
	for directory, modified := range changed {
		if !modified {
			continue
		}
		if err := syncPrivateDirectory(filepath.Join(base, directory)); err != nil {
			return err
		}
	}
	return nil
}
