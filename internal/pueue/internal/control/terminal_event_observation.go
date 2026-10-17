package control

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

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
