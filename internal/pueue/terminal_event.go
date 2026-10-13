package pueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

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
// opted-in inspection worker. Pueue supplies only a reusable numeric ID and an
// exit code here; Succeeded is diagnostic and never an admission proof. The
// application must reconcile the current labeled queue row before recording
// worker-exit authority.
type TerminalEvent struct {
	SchemaVersion int   `json:"schema_version"`
	NumericTaskID int64 `json:"numeric_task_id"`
	Succeeded     bool  `json:"succeeded"` // callback diagnostic; never admission authority
}

// terminalEventTicket suppresses callback state for ordinary tasks. It cannot
// identify a submission across Pueue numeric-ID reuse because the callback
// surface has no per-submission token.
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
// for every task, while only inspection tasks have tickets. Keeping the
// ordinary no-ticket path free of directory creation and locking prevents a
// completed provider task from contending with unrelated inspection proof.
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
		return callback(base)
	})
}

// withExistingPrivateTerminalEventLock observes callback state only when a
// ticket/event mutation has already published its lock. A missing lock means
// there can be no durable callback state to consume; callers fall back to
// queue reconciliation without making a read path provision optional files.
func withExistingPrivateTerminalEventLock(ctx context.Context, stateRoot string, callback func(string) error) (present bool, resultErr error) {
	if callback == nil {
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
	resultErr = withPublishedPrivateLock(ctx, stateRoot, privateTerminalEventLockName, unix.LOCK_SH, func() error {
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
// exact callback contract that can emit terminal inspection events. Legacy
// private roots remain valid and use queue reconciliation as their fallback.
func (c *Client) PrivateTerminalEventsEnabled(ctx context.Context) (bool, error) {
	_, enabled, err := privateTerminalEventsEnabled(ctx, c)
	return enabled, err
}

// RegisterPrivateTerminalEvent opts one inspection worker into terminal
// callback delivery. The ticket is written after Pueue assigns its numeric ID;
// if a task finishes in the tiny add-to-ticket race, the normal queue
// reconciliation path remains the conservative fallback.
func (c *Client) RegisterPrivateTerminalEvent(ctx context.Context, id int64) error {
	stateRoot, enabled, err := privateTerminalEventsEnabled(ctx, c)
	if err != nil || !enabled {
		return err
	}
	return withPrivateTerminalEventLock(ctx, stateRoot, func(base string) error {
		return writeTerminalEventTicket(base, id)
	})
}

// RecordPrivateTerminalEvent is the state-root callback receiver used by the
// fixed delegate-run helper. It writes only a wake-up hint when a prior
// inspection submission created the matching ticket, so ordinary provider
// tasks never accumulate callback state. Callers must reconcile the current
// queue row before treating the callback as terminal evidence.
func RecordPrivateTerminalEvent(ctx context.Context, stateRoot string, id int64, exitCode string) error {
	if ctx == nil {
		return context.Canceled
	}
	succeeded, err := terminalEventSucceeded(exitCode)
	if err != nil {
		return err
	}
	base, err := privateTerminalEventExistingBase(stateRoot)
	if err != nil {
		return err
	}
	// The first unlocked read is intentionally only a fast negative filter.
	// A callback racing ticket publication can miss it, in which case the
	// caller falls back to normal queue reconciliation. A present ticket is
	// rechecked under the event lock before an event is published.
	present, err := readTerminalEventTicket(base, id)
	if err != nil || !present {
		return err
	}
	return withPrivateTerminalEventLock(ctx, stateRoot, func(base string) error {
		lockedPresent, ticketErr := readTerminalEventTicket(base, id)
		if ticketErr != nil || !lockedPresent {
			return ticketErr
		}
		return writeTerminalEvent(base, TerminalEvent{SchemaVersion: task.SchemaVersion, NumericTaskID: id, Succeeded: succeeded})
	})
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

// ReadPrivateTerminalEvent reads one event without consuming it. Callers must
// first persist their own exact worker observation; ClearPrivateTerminalEvents
// is then safe to remove the transient ticket and event together.
func (c *Client) ReadPrivateTerminalEvent(ctx context.Context, id int64) (TerminalEvent, bool, error) {
	stateRoot, enabled, err := privateTerminalEventsEnabled(ctx, c)
	if err != nil || !enabled {
		return TerminalEvent{}, false, err
	}
	var event TerminalEvent
	found := false
	lockPresent, err := withExistingPrivateTerminalEventLock(ctx, stateRoot, func(base string) error {
		var readErr error
		event, found, readErr = readTerminalEvent(base, id)
		return readErr
	})
	if !lockPresent {
		return TerminalEvent{}, false, err
	}
	return event, found, err
}

// ClearPrivateTerminalEvents retires transient callback state only after a
// durable application-owned observation is present or Pueue has removed the
// authorized terminal row. The shared lock closes the callback/cleanup race.
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
	changed := make(map[string]bool, 2)
	for _, id := range ids {
		if err := removePrivateTerminalEventRecordsForID(base, id, changed); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

func removePrivateTerminalEventRecordsForID(base string, id int64, changed map[string]bool) error {
	if id < 0 {
		return ErrUnknown
	}
	for _, directory := range []string{privateTerminalEventTicketDir, privateTerminalEventRecordDir} {
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
