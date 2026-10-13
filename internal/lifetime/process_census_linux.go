//go:build linux

package lifetime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

type linuxProcessCensusEntry struct {
	snapshot processScopeSnapshot
}

type linuxProcessCensusCall struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	entries []linuxProcessCensusEntry
	err     error
}

// linuxProcessCensusBroker owns one in-flight immutable /proc census. It keeps
// no completed cache: concurrent scopes may share the same observation, while
// every later observation starts fresh. The last departing waiter cancels the
// scan so an expired lifecycle operation leaves no detached census work.
type linuxProcessCensusBroker struct {
	mu     sync.Mutex
	active *linuxProcessCensusCall
	scan   func(context.Context) ([]linuxProcessCensusEntry, error)
}

const (
	linuxProcessDirectoryBatch = 128
)

var processCensus = newLinuxProcessCensusBroker(scanLinuxProcessTopology)

func newLinuxProcessCensusBroker(scan func(context.Context) ([]linuxProcessCensusEntry, error)) *linuxProcessCensusBroker {
	return &linuxProcessCensusBroker{scan: scan}
}

func (b *linuxProcessCensusBroker) observe(ctx context.Context) ([]linuxProcessCensusEntry, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b == nil || b.scan == nil {
		return nil, fmt.Errorf("%w: Linux process census is unavailable", ErrUnverifiableLaunch)
	}

	b.mu.Lock()
	call := b.active
	if call == nil {
		// A shared scan outlives any one observer, while release cancels it when
		// the final observer leaves. Keep caller values without inheriting that
		// observer's cancellation.
		scanCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call = &linuxProcessCensusCall{done: make(chan struct{}), cancel: cancel}
		b.active = call
		go b.run(call, scanCtx)
	}
	call.waiters++
	b.mu.Unlock()

	select {
	case <-call.done:
		entries, err := cloneLinuxProcessCensus(call.entries), call.err
		b.release(call)
		return entries, err
	default:
	}
	select {
	case <-call.done:
		entries, err := cloneLinuxProcessCensus(call.entries), call.err
		b.release(call)
		return entries, err
	case <-ctx.Done():
		b.release(call)
		return nil, ctx.Err()
	}
}

func (b *linuxProcessCensusBroker) run(call *linuxProcessCensusCall, ctx context.Context) {
	entries, err := b.scan(ctx)
	b.mu.Lock()
	call.entries = cloneLinuxProcessCensus(entries)
	call.err = err
	if b.active == call {
		b.active = nil
	}
	close(call.done)
	call.cancel()
	b.mu.Unlock()
}

func (b *linuxProcessCensusBroker) release(call *linuxProcessCensusCall) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if call.waiters > 0 {
		call.waiters--
	}
	if b.active == call && call.waiters == 0 {
		// The call is no longer joinable once its final observer has left. Clear
		// the owner before canceling it so a new observer cannot inherit the
		// canceled call while its scan goroutine is unwinding.
		b.active = nil
		call.cancel()
	}
}

func cloneLinuxProcessCensus(entries []linuxProcessCensusEntry) []linuxProcessCensusEntry {
	return append([]linuxProcessCensusEntry(nil), entries...)
}

func reapOwnedLinuxZombies(owner *ProcessRef, anchors []ProcessRef, entries []linuxProcessCensusEntry) ([]linuxProcessCensusEntry, error) {
	return reapOwnedLinuxZombiesWith(owner, anchors, entries, func(pid int) (int, error) {
		waited, err := unix.Wait4(pid, nil, unix.WNOHANG, nil)
		return waited, err
	})
}

func reapOwnedLinuxZombiesWith(
	owner *ProcessRef,
	anchors []ProcessRef,
	entries []linuxProcessCensusEntry,
	wait func(int) (int, error),
) ([]linuxProcessCensusEntry, error) {
	if owner == nil || owner.pid != os.Getpid() {
		return entries, nil
	}
	if wait == nil {
		return nil, fmt.Errorf("%w: Linux zombie reaper is unavailable", ErrUnverifiableLaunch)
	}
	present, token, err := observeProcessIdentity(owner.pid)
	if err != nil || !present || token != owner.startToken {
		return nil, errors.Join(fmt.Errorf("%w: Linux process-scope owner identity changed", ErrUnverifiableLaunch), err)
	}
	infrastructure := processInfrastructureRefs(owner, anchors)
	kept := make([]linuxProcessCensusEntry, 0, len(entries))
	for _, entry := range entries {
		if !linuxZombieNeedsReap(entry.snapshot, owner, infrastructure) {
			kept = append(kept, entry)
			continue
		}
		keep, reapErr := reapLinuxZombieEntry(entry.snapshot, wait)
		if reapErr != nil {
			return nil, reapErr
		}
		if keep {
			kept = append(kept, entry)
		}
	}
	return kept, nil
}

func linuxZombieNeedsReap(snapshot processScopeSnapshot, owner *ProcessRef, infrastructure []ProcessRef) bool {
	return snapshot.zombie && snapshot.parentPID == owner.pid && !snapshotMatchesProcessRef(snapshot, infrastructure)
}

func reapLinuxZombieEntry(snapshot processScopeSnapshot, wait func(int) (int, error)) (bool, error) {
	waited, waitErr := wait(snapshot.pid)
	if waitErr == nil && waited == snapshot.pid {
		// Keep the immutable zombie observation as an ancestry bridge. Its
		// launch reference is deliberately omitted later, but descendants
		// reparented through this zombie still need its PPID edge.
		return true, nil
	}
	if errors.Is(waitErr, unix.ECHILD) || errors.Is(waitErr, unix.ESRCH) || waitErr == nil {
		return true, nil
	}
	return false, fmt.Errorf("reap adopted Linux process %d: %w", snapshot.pid, waitErr)
}

func discoverProcessScopeLaunches(
	ctx context.Context,
	_ string,
	roots []LaunchRef,
	retained []LaunchRef,
	owner *ProcessRef,
	anchors []ProcessRef,
	pendingStart bool,
) ([]LaunchRef, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if len(roots) == 0 && len(retained) == 0 && owner == nil && !pendingStart {
		return nil, nil
	}
	if pendingStart && owner == nil {
		return nil, fmt.Errorf("%w: pending process-scope start has no live owner", ErrUnverifiableLaunch)
	}
	entries, err := processCensus.observe(ctx)
	if err != nil {
		return nil, err
	}
	entries, err = reapOwnedLinuxZombies(owner, anchors, entries)
	if err != nil {
		return nil, err
	}
	snapshots := make([]processScopeSnapshot, 0, len(entries))
	for _, entry := range entries {
		snapshots = append(snapshots, entry.snapshot)
	}
	return discoverOwnedProcessLaunchesWithOwner(roots, retained, owner, anchors, snapshots)
}

func scanLinuxProcessTopology(ctx context.Context) ([]linuxProcessCensusEntry, error) {
	return scanLinuxProcessEntries(ctx, readLinuxProcessTopologyEntry)
}

func scanLinuxProcessEntries(
	ctx context.Context,
	readEntry func(context.Context, int) (linuxProcessCensusEntry, bool, error),
) (entries []linuxProcessCensusEntry, resultErr error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if readEntry == nil {
		return nil, fmt.Errorf("%w: Linux process census reader is unavailable", ErrUnverifiableLaunch)
	}
	directory, err := os.Open("/proc")
	if err != nil {
		return nil, fmt.Errorf("inspect Linux process table: %w", err)
	}
	entries, resultErr = scanLinuxProcessDirectory(ctx, readEntry, func() ([]string, error) {
		return directory.Readdirnames(linuxProcessDirectoryBatch)
	})
	if closeErr := directory.Close(); closeErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("close Linux process table: %w", closeErr))
	}
	return entries, resultErr
}

func scanLinuxProcessDirectory(
	ctx context.Context,
	readEntry func(context.Context, int) (linuxProcessCensusEntry, bool, error),
	nextBatch func() ([]string, error),
) ([]linuxProcessCensusEntry, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if readEntry == nil || nextBatch == nil {
		return nil, fmt.Errorf("%w: Linux process census reader is unavailable", ErrUnverifiableLaunch)
	}
	result := make([]linuxProcessCensusEntry, 0)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		names, batchErr := nextBatch()
		if len(names) > 0 {
			batch, err := scanLinuxProcessEntryNames(ctx, names, readEntry)
			if err != nil {
				return nil, err
			}
			result = append(result, batch...)
		}
		if errors.Is(batchErr, io.EOF) {
			return result, nil
		}
		if batchErr != nil {
			return nil, fmt.Errorf("read Linux process table: %w", batchErr)
		}
	}
}

func scanLinuxProcessEntryNames(
	ctx context.Context,
	names []string,
	readEntry func(context.Context, int) (linuxProcessCensusEntry, bool, error),
) ([]linuxProcessCensusEntry, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if readEntry == nil {
		return nil, fmt.Errorf("%w: Linux process census reader is unavailable", ErrUnverifiableLaunch)
	}
	result := make([]linuxProcessCensusEntry, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, parseErr := strconv.Atoi(name)
		if parseErr != nil || pid <= 1 {
			continue
		}
		observed, observedPresent, observeErr := readEntry(ctx, pid)
		if observeErr != nil {
			return nil, observeErr
		}
		if observedPresent {
			result = append(result, observed)
		}
	}
	return result, nil
}

func readLinuxProcessTopologyEntry(ctx context.Context, pid int) (linuxProcessCensusEntry, bool, error) {
	return readLinuxProcessTopologyEntryWith(ctx, pid, os.ReadFile)
}

func readLinuxProcessTopologyEntryWith(
	ctx context.Context,
	pid int,
	readFile func(string) ([]byte, error),
) (linuxProcessCensusEntry, bool, error) {
	if ctx == nil {
		return linuxProcessCensusEntry{}, false, ErrNilContext
	}
	if readFile == nil {
		return linuxProcessCensusEntry{}, false, fmt.Errorf("%w: Linux process identity reader is unavailable", ErrUnverifiableLaunch)
	}
	if err := ctx.Err(); err != nil {
		return linuxProcessCensusEntry{}, false, err
	}
	statPath := "/proc/" + strconv.Itoa(pid) + "/stat"
	data, err := readFile(statPath)
	if isLinuxProcessAbsentError(err) {
		return linuxProcessCensusEntry{}, false, nil
	}
	if err != nil {
		return linuxProcessCensusEntry{}, false, fmt.Errorf("read Linux process identity for PID %d: %w", pid, err)
	}
	record, err := parseLinuxProcessScopeStat(data)
	if err != nil {
		return linuxProcessCensusEntry{}, false, fmt.Errorf("parse Linux process identity for PID %d: %w", pid, err)
	}
	if record.snapshot.pid != pid {
		return linuxProcessCensusEntry{}, false, nil
	}
	if err = ctx.Err(); err != nil {
		return linuxProcessCensusEntry{}, false, err
	}
	record.snapshot.zombie = record.zombie
	return linuxProcessCensusEntry{snapshot: record.snapshot}, true, nil
}
