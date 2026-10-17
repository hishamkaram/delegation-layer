package control

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
