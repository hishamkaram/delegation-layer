package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const rootAdmissionTestTimeout = 5 * time.Second

type rootAdmissionFixture struct {
	parent string
	store  *Store
	stores []*Store
	leases []*RootAdmissionLease
	failed bool
}

func newRootAdmissionFixture(t *testing.T) *rootAdmissionFixture {
	t.Helper()
	parent, err := os.MkdirTemp("", "root-admission-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &rootAdmissionFixture{parent: parent}
	t.Cleanup(func() {
		for index := len(fixture.leases) - 1; index >= 0; index-- {
			if closeErr := fixture.leases[index].Release(); closeErr != nil {
				fixture.failed = true
				t.Error(closeErr)
			}
		}
		for index := len(fixture.stores) - 1; index >= 0; index-- {
			if closeErr := fixture.stores[index].Close(); closeErr != nil {
				fixture.failed = true
				t.Error(closeErr)
			}
		}
		if fixture.failed {
			t.Logf("retained root admission fixture after unresolved closure: %s", fixture.parent)
			return
		}
		if removeErr := os.RemoveAll(fixture.parent); removeErr != nil {
			t.Error(removeErr)
		}
	})
	fixture.store, err = InitStore(filepath.Join(parent, "state"))
	if fixture.store != nil {
		fixture.stores = append(fixture.stores, fixture.store)
	}
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *rootAdmissionFixture) other(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(f.store.Root)
	if store != nil {
		f.stores = append(f.stores, store)
	}
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func (f *rootAdmissionFixture) own(lease *RootAdmissionLease) {
	if lease != nil {
		f.leases = append(f.leases, lease)
	}
}

func (f *rootAdmissionFixture) shared(t *testing.T, store *Store) *RootAdmissionLease {
	t.Helper()
	lease, err := store.AcquireRootAdmissionContext(t.Context())
	f.own(lease)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func (f *rootAdmissionFixture) exclusive(t *testing.T, store *Store) *RootAdmissionLease {
	t.Helper()
	lease, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	f.own(lease)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestRootAdmissionSharedLeasesExcludeExclusiveAcrossStores(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	other := fixture.other(t)
	first := fixture.shared(t, fixture.store)
	second := fixture.shared(t, fixture.store)
	third := fixture.shared(t, other)
	if err := first.CheckRootContext(t.Context(), other); err != nil {
		t.Fatalf("same native root with another Store refused: %v", err)
	}
	flags, err := unix.FcntlInt(uintptr(first.lock.Fd()), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("root fence descriptor is inheritable: %v", err)
	}
	if first.lock.level != LockLevelRootAdmission || LockLevelRootAdmission >= LockLevelMaintenance {
		t.Fatal("root admission lost its earlier named lock rank")
	}
	requireRootAdmissionBusy(t, fixture, other)
	must(t, first.Release())
	must(t, second.Release())
	requireRootAdmissionBusy(t, fixture, fixture.store)
	must(t, third.Release())
	exclusive := fixture.exclusive(t, other)
	must(t, exclusive.Release())
	must(t, exclusive.Release())
	if err = exclusive.CheckRootContext(t.Context(), other); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("released fence accepted: %v", err)
	}
}

func requireRootAdmissionBusy(t *testing.T, fixture *rootAdmissionFixture, store *Store) {
	t.Helper()
	lease, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(lease)
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("conflicting EX did not return canonical busy: %v", err)
	}
}

func TestRootAdmissionCancellationDoesNotReleaseCallerWork(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	other := fixture.other(t)
	ctx, cancel := context.WithCancel(t.Context())
	lease, err := fixture.store.AcquireRootAdmissionContext(ctx)
	fixture.own(lease)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if err = lease.CheckRootContext(ctx, fixture.store); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled validation: %v", err)
	}
	requireRootAdmissionBusy(t, fixture, other)
	must(t, lease.Release())
	fixture.exclusive(t, other)
}

func TestRootAdmissionRejectsDoneContextsBeforeLazyCreation(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	before, err := os.ReadDir(fixture.store.Root)
	must(t, err)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer cancelExpired()
	var absent context.Context
	for _, input := range []struct {
		ctx  context.Context
		want error
	}{{canceled, context.Canceled}, {expired, context.DeadlineExceeded}, {absent, os.ErrInvalid}} {
		lease, acquireErr := fixture.store.AcquireRootAdmissionContext(input.ctx)
		fixture.own(lease)
		if !errors.Is(acquireErr, input.want) {
			t.Fatalf("shared context error: %v", acquireErr)
		}
		lease, acquireErr = fixture.store.TryAcquireRootAdmissionExclusiveContext(input.ctx)
		fixture.own(lease)
		if !errors.Is(acquireErr, input.want) {
			t.Fatalf("exclusive context error: %v", acquireErr)
		}
	}
	after, err := os.ReadDir(fixture.store.Root)
	must(t, err)
	names := func(entries []os.DirEntry) []string {
		result := []string{}
		for _, entry := range entries {
			result = append(result, entry.Name())
		}
		return result
	}
	if !slices.Equal(names(before), names(after)) {
		t.Fatal("refused acquisition created root entries")
	}
	requireAbsent(t, filepath.Join(fixture.store.Root, rootAdmissionLockName))
}

func TestRootAdmissionIsLazyAndRequiresExistingRootOwner(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	path := filepath.Join(fixture.store.Root, rootAdmissionLockName)
	requireAbsent(t, path)
	lease := fixture.shared(t, fixture.store)
	before, err := os.Lstat(path)
	must(t, err)
	if before.Mode() != 0o600 {
		t.Fatal("lazy root admission inode is not private")
	}
	must(t, lease.Release())
	again := fixture.exclusive(t, fixture.store)
	after, err := os.Lstat(path)
	must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("release/reacquisition replaced the stable inode")
	}
	must(t, again.Release())
	must(t, fixture.store.Close())
	closed, err := fixture.store.AcquireRootAdmissionContext(t.Context())
	fixture.own(closed)
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed Store acquired fence: %v", err)
	}
	var absent *Store
	if _, err = absent.AcquireRootAdmissionContext(t.Context()); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil Store: %v", err)
	}
	if _, err = (&Store{}).TryAcquireRootAdmissionExclusiveContext(t.Context()); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("zero Store initialized root: %v", err)
	}
}

func TestRootAdmissionCheckRefusesDifferentRoot(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	other := newRootAdmissionFixture(t)
	lease := fixture.shared(t, fixture.store)
	if err := lease.CheckRootContext(t.Context(), other.store); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("another native root accepted: %v", err)
	}
}

type rootAdmissionWaitGate struct {
	BaseFaultInjector
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newRootAdmissionWaitGate() *rootAdmissionWaitGate {
	return &rootAdmissionWaitGate{entered: make(chan struct{}), release: make(chan struct{})}
}
func (*rootAdmissionWaitGate) OnLockFileBarrier(string) error      { return nil }
func (*rootAdmissionWaitGate) OnLockParentDirBarrier(string) error { return nil }
func (g *rootAdmissionWaitGate) OnFlock(path string, operation int) error {
	if filepath.Base(path) != rootAdmissionLockName || operation != unix.LOCK_SH|unix.LOCK_NB {
		return nil
	}
	g.mu.Lock()
	g.calls++
	second := g.calls == 2
	g.mu.Unlock()
	if second {
		close(g.entered)
		<-g.release
	}
	return nil
}
func (g *rootAdmissionWaitGate) unblock() { g.once.Do(func() { close(g.release) }) }
func (g *rootAdmissionWaitGate) await(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered: // The first real flock saw EX busy; this is its retry.
	case <-time.After(rootAdmissionTestTimeout):
		t.Fatal("shared fence did not reach its actual native contention retry")
	}
}

type (
	rootAdmissionResult struct {
		lease *RootAdmissionLease
		err   error
	}
	rootAdmissionPending struct {
		done    chan rootAdmissionResult
		cancel  context.CancelFunc
		joined  bool
		fixture *rootAdmissionFixture
	}
)

func startRootAdmissionPending(t *testing.T, fixture *rootAdmissionFixture, store *Store, parent context.Context) *rootAdmissionPending {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	pending := &rootAdmissionPending{done: make(chan rootAdmissionResult, 1), cancel: cancel, fixture: fixture}
	t.Cleanup(func() {
		cancel()
		if !pending.joined {
			result := <-pending.done
			fixture.own(result.lease)
			pending.joined = true
		}
	})
	go func() {
		lease, err := store.AcquireRootAdmissionContext(ctx)
		pending.done <- rootAdmissionResult{lease, err}
	}()
	return pending
}

func (p *rootAdmissionPending) await(t *testing.T) rootAdmissionResult {
	t.Helper()
	select {
	case result := <-p.done:
		p.joined = true
		p.fixture.own(result.lease)
		return result
	case <-time.After(rootAdmissionTestTimeout):
		t.Fatal("root admission worker did not complete")
	}
	return rootAdmissionResult{}
}

func TestRootAdmissionWaitDropsMaintenanceBeforeNativeContention(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	holder := fixture.exclusive(t, fixture.store)
	waitingStore := fixture.other(t)
	maintenanceProbe := fixture.other(t)
	gate := newRootAdmissionWaitGate()
	waitingStore.SetFaultInjector(gate)
	defer gate.unblock()
	pending := startRootAdmissionPending(t, fixture, waitingStore, t.Context())
	gate.await(t)
	if err := maintenanceProbe.maintLock.LockEXNonblocking(); err != nil {
		t.Errorf("admission waiter retained maintenance SH while root EX is held: %v", err)
	} else {
		must(t, maintenanceProbe.maintLock.Unlock())
	}
	must(t, holder.Release())
	gate.unblock()
	result := pending.await(t)
	if result.err != nil || result.lease == nil {
		t.Fatalf("shared fence after EX release: %v", result.err)
	}
	must(t, result.lease.CheckRootContext(t.Context(), waitingStore))
}

func TestRootAdmissionExclusiveBlocksSharedUntilActualDeadline(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	fixture.exclusive(t, fixture.store)
	waitingStore := fixture.other(t)
	gate := newRootAdmissionWaitGate()
	waitingStore.SetFaultInjector(gate)
	defer gate.unblock()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	pending := startRootAdmissionPending(t, fixture, waitingStore, ctx)
	gate.await(t)
	if ctx.Err() != nil {
		t.Fatal("fixture deadline expired before native contention was established")
	}
	gate.unblock()
	result := pending.await(t)
	if !errors.Is(result.err, context.DeadlineExceeded) || !errors.Is(result.err, task.ErrLockBusy) {
		t.Fatalf("contended SH did not honor caller deadline: %v", result.err)
	}
}

type rootAdmissionUnlockFault struct {
	BaseFaultInjector
	mu       sync.Mutex
	name     string
	failures int
	fault    error
}

func (*rootAdmissionUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (*rootAdmissionUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *rootAdmissionUnlockFault) OnFlock(path string, operation int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if operation == unix.LOCK_UN && filepath.Base(path) == f.name && f.failures > 0 {
		f.failures--
		return f.fault
	}
	return nil
}
func (f *rootAdmissionUnlockFault) clear() { f.mu.Lock(); defer f.mu.Unlock(); f.failures = 0 }

func TestRootAdmissionReleaseRetainsFailedUnlockOwnership(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	other := fixture.other(t)
	lease := fixture.exclusive(t, fixture.store)
	fault := &rootAdmissionUnlockFault{name: rootAdmissionLockName, failures: 1, fault: errors.New("root admission unlock fault")}
	fixture.store.SetFaultInjector(fault)
	defer fault.clear()
	if err := lease.Release(); !errors.Is(err, fault.fault) {
		t.Fatalf("failed unlock hidden: %v", err)
	}
	requireRootAdmissionBusy(t, fixture, other)
	must(t, lease.CheckRootContext(t.Context(), other))
	must(t, lease.Release())
	fixture.exclusive(t, other)
}

func TestRootAdmissionOpeningUnlockFailureRetainsPartialHandle(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	other := fixture.other(t)
	fault := &rootAdmissionUnlockFault{name: ".maintenance.lock", failures: 2, fault: errors.New("opening maintenance unlock fault")}
	fixture.store.SetFaultInjector(fault)
	defer fault.clear()
	lease, err := fixture.store.AcquireRootAdmissionContext(t.Context())
	fixture.own(lease)
	if lease == nil || !errors.Is(err, fault.fault) {
		t.Fatalf("unresolved opening hold discarded: %v", err)
	}
	if err = lease.CheckRootContext(t.Context(), fixture.store); err == nil {
		t.Fatal("partial error handle accepted as admission")
	}
	if err = other.maintLock.LockEXNonblocking(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("failed opening unlock dropped maintenance ownership: %v", err)
	}
	fault.clear()
	must(t, lease.Release())
	must(t, other.maintLock.LockEXNonblocking())
	must(t, other.maintLock.Unlock())
}

func TestRootAdmissionRejectsUnsafeNamedInodes(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newRootAdmissionFixture(t)
			path := filepath.Join(fixture.store.Root, rootAdmissionLockName)
			prepareRootAdmissionUnsafePath(t, path, kind)
			before, err := os.Lstat(path)
			must(t, err)
			lease, err := fixture.store.AcquireRootAdmissionContext(t.Context())
			fixture.own(lease)
			if err == nil {
				t.Fatalf("unsafe %s acquired root admission", kind)
			}
			after, statErr := os.Lstat(path)
			if statErr != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("refusal replaced/chmodded unsafe inode: %v", statErr)
			}
		})
	}
}

func prepareRootAdmissionUnsafePath(t *testing.T, path, kind string) {
	t.Helper()
	switch kind {
	case "mode":
		must(t, os.WriteFile(path, nil, 0o644))
	case "symlink":
		must(t, os.WriteFile(path+"-target", []byte("preserve"), 0o600))
		must(t, os.Symlink(filepath.Base(path)+"-target", path))
	case "directory":
		must(t, os.Mkdir(path, 0o700))
	case "fifo":
		must(t, unix.Mkfifo(path, 0o600))
	}
}

func replaceRootAdmissionName(t *testing.T, fixture *rootAdmissionFixture) {
	t.Helper()
	path := filepath.Join(fixture.store.Root, rootAdmissionLockName)
	retained := path + "-retained"
	must(t, os.Rename(path, retained))
	t.Cleanup(func() {
		if removeErr := os.Remove(path); removeErr != nil {
			fixture.failed = true
			t.Error(removeErr)
			return
		}
		if renameErr := os.Rename(retained, path); renameErr != nil {
			fixture.failed = true
			t.Error(renameErr)
		}
	})
	must(t, os.WriteFile(path, nil, 0o600))
}

func TestRootAdmissionCheckRefusesLockAndRootReplacement(t *testing.T) {
	for _, kind := range []string{"lock", "root"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newRootAdmissionFixture(t)
			lease := fixture.shared(t, fixture.store)
			if kind == "lock" {
				replaceRootAdmissionName(t, fixture)
			} else {
				replaceRootAdmissionDirectory(t, fixture)
			}
			if err := lease.CheckRootContext(t.Context(), fixture.store); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("replaced %s accepted: %v", kind, err)
			}
			must(t, lease.Release()) // Release owns its descriptor, never the replacement.
		})
	}
}

func replaceRootAdmissionDirectory(t *testing.T, fixture *rootAdmissionFixture) {
	t.Helper()
	root := fixture.store.Root
	metadata, err := os.ReadFile(filepath.Join(root, "root.json"))
	must(t, err)
	retained := root + "-retained"
	must(t, os.Rename(root, retained))
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(root); removeErr != nil {
			fixture.failed = true
			t.Error(removeErr)
			return
		}
		if renameErr := os.Rename(retained, root); renameErr != nil {
			fixture.failed = true
			t.Error(renameErr)
		}
	})
	must(t, os.Mkdir(root, 0o700))
	must(t, os.WriteFile(filepath.Join(root, "root.json"), metadata, 0o600))
}

func TestRootAdmissionRechecksAfterWaitingOnReplacedRootOrLock(t *testing.T) {
	for _, kind := range []string{"lock", "root"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newRootAdmissionFixture(t)
			held := fixture.exclusive(t, fixture.store)
			waitingStore := fixture.other(t)
			gate := newRootAdmissionWaitGate()
			waitingStore.SetFaultInjector(gate)
			defer gate.unblock()
			pending := startRootAdmissionPending(t, fixture, waitingStore, t.Context())
			gate.await(t)
			if kind == "lock" {
				replaceRootAdmissionName(t, fixture)
			} else {
				replaceRootAdmissionDirectory(t, fixture)
			}
			must(t, held.Release())
			gate.unblock()
			result := pending.await(t)
			if !errors.Is(result.err, task.ErrIdentityMismatch) {
				t.Fatalf("post-wait %s replacement accepted: %v", kind, result.err)
			}
		})
	}
}

func TestRootAdmissionPostcheckCleanupFailureKeepsOwnedDescriptor(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	held := fixture.exclusive(t, fixture.store)
	waitingStore := fixture.other(t)
	gate := newRootAdmissionWaitGate()
	waitingStore.SetFaultInjector(gate)
	defer gate.unblock()
	pending := startRootAdmissionPending(t, fixture, waitingStore, t.Context())
	gate.await(t)
	replaceRootAdmissionName(t, fixture)
	fault := &rootAdmissionUnlockFault{name: rootAdmissionLockName, failures: 1, fault: errors.New("postcheck cleanup unlock fault")}
	waitingStore.SetFaultInjector(fault)
	defer fault.clear()
	must(t, held.Release())
	gate.unblock()
	result := pending.await(t)
	if result.lease == nil || !errors.Is(result.err, task.ErrIdentityMismatch) || !errors.Is(result.err, fault.fault) {
		t.Fatalf("unresolved postcheck resource lost: %v", result.err)
	}
	must(t, result.lease.Release())
}

func TestRootAdmissionNativeSharedWorkersOverlap(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	other := fixture.other(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	acquired := make(chan error, 2)
	done := make(chan error, 2)
	defer func() {
		unblock()
		for range 2 {
			if err := <-done; err != nil {
				fixture.failed = true
				t.Error(err)
			}
		}
	}()
	for _, store := range []*Store{fixture.store, other} {
		go func() {
			lease, err := store.AcquireRootAdmissionContext(t.Context())
			acquired <- err
			<-release
			if lease != nil {
				err = errors.Join(err, lease.Release())
			}
			done <- err
		}()
	}
	for range 2 {
		select {
		case err := <-acquired:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(rootAdmissionTestTimeout):
			t.Fatal("shared worker could not overlap its peer")
		}
	}
	requireRootAdmissionBusy(t, fixture, fixture.store)
	unblock()
}

func TestRootAdmissionWaitingCancellationKeepsOtherOwnerHeld(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	held := fixture.exclusive(t, fixture.store)
	waitingStore := fixture.other(t)
	gate := newRootAdmissionWaitGate()
	waitingStore.SetFaultInjector(gate)
	defer gate.unblock()
	pending := startRootAdmissionPending(t, fixture, waitingStore, t.Context())
	gate.await(t)
	pending.cancel()
	gate.unblock()
	result := pending.await(t)
	if !errors.Is(result.err, context.Canceled) || !errors.Is(result.err, task.ErrLockBusy) {
		t.Fatalf("native pending acquisition lost cancellation: %v", result.err)
	}
	must(t, held.CheckRootContext(t.Context(), fixture.store))
	requireRootAdmissionBusy(t, fixture, waitingStore)
	must(t, held.Release())
	fixture.exclusive(t, waitingStore)
}

func TestRootAdmissionValidationRefusesChangedProtection(t *testing.T) {
	for _, kind := range []string{"mode", "cloexec"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newRootAdmissionFixture(t)
			lease := fixture.shared(t, fixture.store)
			path := filepath.Join(fixture.store.Root, rootAdmissionLockName)
			if kind == "mode" {
				must(t, os.Chmod(path, 0o644))
				defer func() { must(t, os.Chmod(path, 0o600)) }()
			} else {
				flags, err := unix.FcntlInt(uintptr(lease.lock.Fd()), unix.F_GETFD, 0)
				must(t, err)
				_, err = unix.FcntlInt(uintptr(lease.lock.Fd()), unix.F_SETFD, flags&^unix.FD_CLOEXEC)
				must(t, err)
				defer func() {
					_, restoreErr := unix.FcntlInt(uintptr(lease.lock.Fd()), unix.F_SETFD, flags)
					must(t, restoreErr)
				}()
			}
			if err := lease.CheckRootContext(t.Context(), fixture.store); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("changed %s accepted: %v", kind, err)
			}
		})
	}
}
