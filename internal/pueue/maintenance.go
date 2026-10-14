package pueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const privateQueueMaintenanceSettlePolls = 4

const (
	privateQueueMaintenanceRecordName    = "queue-maintenance.json"
	privateQueueMaintenanceStateLockName = "queue-maintenance-state.lock"
)

// privateQueueMaintenanceRecord is the durable wake-up state for optional
// queue cleanup. Delegate task and inspection journals remain the sole removal
// authority; this record only makes callback delivery and retries crash
// recoverable. The queue-maintenance election lock is the sole live-owner
// authority.
type privateQueueMaintenanceRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Requested     uint64 `json:"requested"`
	Attempted     uint64 `json:"attempted"`
	Completed     uint64 `json:"completed"`
	FullRequested uint64 `json:"full_requested"`
	FullCompleted uint64 `json:"full_completed"`
}

func requestPrivateQueueMaintenance(ctx context.Context, stateRoot string) (uint64, error) {
	return requestPrivateQueueMaintenanceScope(ctx, stateRoot, true)
}

func requestPrivateCandidateQueueMaintenance(ctx context.Context, stateRoot string) (uint64, error) {
	return requestPrivateQueueMaintenanceScope(ctx, stateRoot, false)
}

func requestPrivateInspectionQueueMaintenance(ctx context.Context, stateRoot string) (uint64, error) {
	return requestPrivateCandidateQueueMaintenance(ctx, stateRoot)
}

func requestPrivateQueueMaintenanceScope(ctx context.Context, stateRoot string, full bool) (uint64, error) {
	var requested uint64
	err := withPrivateQueueMaintenanceState(ctx, stateRoot, func(base string, record *privateQueueMaintenanceRecord) error {
		if record.Requested == ^uint64(0) {
			return fmt.Errorf("%w: private queue maintenance generation exhausted", ErrConfiguration)
		}
		record.Requested++
		requested = record.Requested
		if full {
			record.FullRequested = requested
		}
		return writePrivateQueueMaintenanceRecord(base, *record)
	})
	return requested, err
}

// RequestPrivateQueueMaintenance durably records optional cleanup work before
// a callback attempts to bind the current private supervisor. A failed bind or
// crashed maintenance owner therefore cannot consume the only wake-up signal.
func RequestPrivateQueueMaintenance(ctx context.Context, stateRoot string) error {
	_, err := requestPrivateQueueMaintenance(ctx, stateRoot)
	return err
}

// RequestPrivateCandidateQueueMaintenance records cleanup work bounded to
// rows with durable terminal-event candidates. A later full recovery request
// dominates it.
func RequestPrivateCandidateQueueMaintenance(ctx context.Context, stateRoot string) error {
	_, err := requestPrivateCandidateQueueMaintenance(ctx, stateRoot)
	return err
}

// RequestPrivateInspectionQueueMaintenance retains the original internal API
// name for existing embedders. Candidate maintenance now covers both ordinary
// and inspection terminal events.
func RequestPrivateInspectionQueueMaintenance(ctx context.Context, stateRoot string) error {
	return RequestPrivateCandidateQueueMaintenance(ctx, stateRoot)
}

// PrivateQueueMaintenanceRetryNeeded is an unlocked durable-work hint. Atomic
// record replacement makes each read coherent; only the election lock proves
// that a live owner exists. An unattempted or interrupted generation remains
// pending. A completed failed attempt requires a later wake before it
// is retried, which prevents an elected owner from spinning on one failure.
func PrivateQueueMaintenanceRetryNeeded(stateRoot string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultObservationTimeout)
	defer cancel()
	return PrivateQueueMaintenanceRetryNeededContext(ctx, stateRoot)
}

// PrivateQueueMaintenanceRetryNeededContext is the bounded-context form used
// by callers that already own an observation lifetime.
func PrivateQueueMaintenanceRetryNeededContext(ctx context.Context, stateRoot string) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	base, err := privateTerminalEventExistingBase(stateRoot)
	if err != nil {
		return false, err
	}
	record, err := readPrivateQueueMaintenanceRecord(base)
	if err != nil {
		return false, err
	}
	if record.Requested > record.Attempted {
		return true, nil
	}
	return privateTerminalEventWakePending(ctx, stateRoot)
}

// WaitForPrivateQueueMaintenanceIdle waits for the current elected owner, if
// any, to release the per-root maintenance election. It never consumes a wake
// or runs cleanup; callers that need durable completion must first observe the
// requested generation as attempted.
func WaitForPrivateQueueMaintenanceIdle(ctx context.Context, stateRoot string) error {
	if ctx == nil {
		return context.Canceled
	}
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return ErrConfiguration
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	present, err := privateLockFilePresent(base, privateQueueMaintenanceLockName)
	if err != nil || !present {
		return err
	}
	return withPrivateQueueMaintenance(ctx, stateRoot, func() error { return nil })
}

type privateQueueMaintenanceWake struct {
	Requested     uint64
	Attempted     uint64
	FullRequested uint64
}

// settlePrivateQueueMaintenanceWake gives callbacks in one completion burst a
// short bounded window to publish wake generations before the elected owner
// captures their latest value and snapshots the queue. The durable attempted
// watermark provides the correctness boundary; this wait only avoids
// control-plane scan fan-out.
func settlePrivateQueueMaintenanceWake(ctx context.Context, stateRoot string) error {
	previous, err := readPrivateQueueMaintenanceWake(ctx, stateRoot)
	if err != nil {
		return err
	}
	for range privateQueueMaintenanceSettlePolls {
		timer := time.NewTimer(privateQueueSettlementPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		current, readErr := readPrivateQueueMaintenanceWake(ctx, stateRoot)
		if readErr != nil {
			return readErr
		}
		if current == previous {
			return nil
		}
		previous = current
	}
	return nil
}

func readPrivateQueueMaintenanceWake(ctx context.Context, stateRoot string) (privateQueueMaintenanceWake, error) {
	var wake privateQueueMaintenanceWake
	err := withPrivateQueueMaintenanceState(ctx, stateRoot, func(_ string, record *privateQueueMaintenanceRecord) error {
		wake = privateQueueMaintenanceWake{
			Requested:     record.Requested,
			Attempted:     record.Attempted,
			FullRequested: record.FullRequested,
		}
		return nil
	})
	return wake, err
}

func beginPrivateQueueMaintenancePass(ctx context.Context, stateRoot string, force bool) (uint64, bool, bool, error) {
	var target uint64
	full := force
	run := false
	err := withPrivateQueueMaintenanceState(ctx, stateRoot, func(_ string, record *privateQueueMaintenanceRecord) error {
		eventPending, eventErr := privateTerminalEventWakePending(ctx, stateRoot)
		if eventErr != nil {
			return eventErr
		}
		target = record.Requested
		full = full || record.FullRequested > record.FullCompleted
		run = force || record.Requested > record.Attempted || eventPending
		return nil
	})
	return target, full, run, err
}

func finishPrivateQueueMaintenancePass(ctx context.Context, stateRoot string, target uint64, full, succeeded bool) (bool, error) {
	pending := false
	err := withPrivateQueueMaintenanceState(ctx, stateRoot, func(base string, record *privateQueueMaintenanceRecord) error {
		if target > record.Attempted {
			record.Attempted = target
		}
		if succeeded && target > record.Completed {
			record.Completed = target
		}
		if succeeded && full && record.FullRequested <= target && record.FullRequested > record.FullCompleted {
			record.FullCompleted = record.FullRequested
		}
		pending = record.Requested > record.Attempted
		return writePrivateQueueMaintenanceRecord(base, *record)
	})
	return pending, err
}

func readPrivateQueueMaintenanceRecord(base string) (privateQueueMaintenanceRecord, error) {
	path := filepath.Join(base, privateQueueMaintenanceRecordName)
	data, err := readRegular(path, MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return privateQueueMaintenanceRecord{SchemaVersion: task.SchemaVersion}, nil
	}
	if err != nil {
		return privateQueueMaintenanceRecord{}, fmt.Errorf("%w: read private queue maintenance state: %w", ErrBinding, err)
	}
	var record privateQueueMaintenanceRecord
	if err = task.DecodeStrict(data, &record); err != nil {
		return privateQueueMaintenanceRecord{}, fmt.Errorf("%w: decode private queue maintenance state: %w", ErrBinding, err)
	}
	if record.SchemaVersion != task.SchemaVersion {
		return privateQueueMaintenanceRecord{}, fmt.Errorf("%w: unsupported private queue maintenance schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if record.Completed > record.Attempted || record.Attempted > record.Requested ||
		record.FullCompleted > record.Completed || record.FullCompleted > record.FullRequested || record.FullRequested > record.Requested {
		return privateQueueMaintenanceRecord{}, fmt.Errorf("%w: invalid private queue maintenance generations", ErrBinding)
	}
	return record, nil
}

func writePrivateQueueMaintenanceRecord(base string, record privateQueueMaintenanceRecord) error {
	if record.SchemaVersion != task.SchemaVersion || record.Completed > record.Attempted || record.Attempted > record.Requested ||
		record.FullCompleted > record.Completed || record.FullCompleted > record.FullRequested || record.FullRequested > record.Requested {
		return ErrConfiguration
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return fmt.Errorf("%w: marshal private queue maintenance state: %w", ErrConfiguration, err)
	}
	return replacePrivateRecordAtomic(
		base,
		filepath.Join(base, privateQueueMaintenanceRecordName),
		".queue-maintenance-",
		"private queue maintenance state",
		data,
		false,
	)
}

func withPrivateQueueMaintenanceState(ctx context.Context, stateRoot string, callback func(string, *privateQueueMaintenanceRecord) error) error {
	if ctx == nil {
		return context.Canceled
	}
	if callback == nil || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return ErrConfiguration
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateLockFile(base, privateQueueMaintenanceStateLockName); err != nil {
		return err
	}
	return withPublishedPrivateLock(ctx, stateRoot, privateQueueMaintenanceStateLockName, unix.LOCK_EX, func() error {
		record, err := readPrivateQueueMaintenanceRecord(base)
		if err != nil {
			return err
		}
		return callback(base, &record)
	})
}
