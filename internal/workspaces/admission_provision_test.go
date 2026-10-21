package workspaces

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

// provisionObserverBudget bounds the admission observer of a scheduled
// helper. It is far shorter than the helper deadline and the scheduling
// control cutoff. Each observer runs in a synctest bubble after the real
// fixture setup: its lock and observation waits sleep on bubble timers, so
// the budget elapses only once the observer is durably blocked, never while
// Schedule and Close run. Fixtures carry wall-clock timestamps from setup,
// while each bubble starts at the synthetic year 2000; these cases exercise
// lock and observation waits, not elapsed-time policy. The barrier asserts
// that before it holds a lock.
const provisionObserverBudget = 2 * time.Second

// acceptedProvisionAdmission is an admission whose provision purpose names a
// helper with a closed native lifetime and a succeeded result. Replaying
// that recorded intent schedules nothing new, so ScheduleOperation and
// CloseOperation succeed without an available runtime. No registration is
// recorded.
func acceptedProvisionAdmission(t *testing.T) finishedAdmission {
	t.Helper()
	admission := newFinishedAdmission(t)
	t.Cleanup(func() {
		if err := admission.admission.Close(); err != nil {
			t.Error(err)
		}
	})
	operation := admission.openProvisionHelper(t, time.Hour)
	attempt, err := admission.fixture.Resource.ResolveAdmissionAttempt(t.Context(), admission.reservation.Sequence, admission.reservation.Subject)
	if err != nil {
		t.Fatal(err)
	}
	purpose, err := attempt.ProvisionPurpose()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admission.fixture.Resource.AssociateOperation(t.Context(), purpose, operation.Request()); err != nil {
		t.Fatal(err)
	}
	workspacefixture.RunRealGuardianLifetime(t, admission.fixture.Store, operation, func() {
		if recordErr := operation.RecordResult(t.Context(), workspacefixture.SucceededResult(operation)); recordErr != nil {
			t.Fatal(recordErr)
		}
	})
	return admission
}

func (f finishedAdmission) provision(ctx context.Context) error {
	_, err := provisionIfNeeded(ctx, DispatchRequest{}, OperationOptions{Runtime: unavailableOperationRuntime{}}, f.fixture.Store, f.admission, time.Minute)
	return err
}

// provisionBarrier holds one lock from a second descriptor once the admission
// observer reaches the scheduled helper. The helper lock's first open belongs
// to Schedule's purpose replay; its second open is the observation load, after
// Schedule, Close and the post-Schedule context check. The held lock is either
// that helper lock or the resource lock of the registration read that follows
// an accepted observation; its reader then waits for the observer.
type provisionBarrier struct {
	taskdir.BaseFaultInjector
	t                *testing.T
	observer         context.Context
	helperLock       string
	resourceLock     string
	holdRegistration bool
	onHold           func()
	opens            int
	observed         bool
	held             int
}

func newProvisionBarrier(t *testing.T, admission finishedAdmission, observer context.Context) *provisionBarrier {
	t.Helper()
	store := admission.fixture.Store
	barrier := &provisionBarrier{
		t: t, observer: observer, held: -1,
		helperLock:   workspacefixture.OperationDirectory(store, admissionHelperID) + "/.operation.lock",
		resourceLock: store.Root + "/workspaces/" + admission.fixture.Resource.Request().WorkspaceID + "/.operation.lock",
	}
	t.Cleanup(func() {
		if barrier.held >= 0 {
			if err := unix.Close(barrier.held); err != nil {
				t.Error(err)
			}
		}
	})
	return barrier
}

func (b *provisionBarrier) install(admission finishedAdmission) {
	setStoreFault(b.t, admission.fixture.Store, b)
}

func (b *provisionBarrier) OnLockParentDirBarrier(string) error { return nil }

func (b *provisionBarrier) OnLockFileBarrier(path string) error {
	if path != b.helperLock {
		return nil
	}
	if b.opens++; b.opens != 2 {
		return nil
	}
	if err := b.observer.Err(); err != nil {
		b.t.Errorf("observer ended before the scheduled helper observation: %v", err)
		return nil
	}
	b.observed = true
	if b.holdRegistration {
		return nil
	}
	return b.hold(path)
}

// OnFlock holds the first resource lock attempt after the observation began:
// the observation reads only the helper journal, so that attempt is the
// registration read.
func (b *provisionBarrier) OnFlock(path string, operation int) error {
	if b.holdRegistration && b.observed && b.held < 0 && path == b.resourceLock && operation&unix.LOCK_UN == 0 {
		return b.hold(path)
	}
	return nil
}

func (b *provisionBarrier) hold(path string) error {
	held, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	if err = unix.Flock(held, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.Join(err, unix.Close(held))
	}
	b.held = held
	if b.onHold != nil {
		b.onHold()
	}
	return nil
}

func (b *provisionBarrier) requireHeld(admission finishedAdmission) {
	b.t.Helper()
	if !b.observed || b.held < 0 || b.opens != 2 {
		b.t.Fatalf("barrier observed %t, held %t after %d helper opens; want a hold after Schedule", b.observed, b.held >= 0, b.opens)
	}
	if admission.admission.operationID != admissionHelperID {
		b.t.Fatalf("admission helper = %q, want %q", admission.admission.operationID, admissionHelperID)
	}
}

func requireProvisionFault(t *testing.T, label string, err, cause error) {
	t.Helper()
	if !errors.Is(err, cause) || errors.Is(err, workspace.ErrPending) || PendingError(err) {
		t.Fatalf("%s = %v; want a fault carrying %v without pending", label, err, cause)
	}
}

func TestProvisionObservationDeadlineKeepsScheduledHelperPending(t *testing.T) {
	admission := acceptedProvisionAdmission(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), provisionObserverBudget)
		defer cancel()
		barrier := newProvisionBarrier(t, admission, ctx)
		barrier.install(admission)
		err := admission.provision(ctx)
		barrier.requireHeld(admission)
		if !errors.Is(err, workspace.ErrPending) || !errors.Is(err, context.DeadlineExceeded) || !PendingError(err) {
			t.Fatalf("scheduled helper observation deadline = %v, want pending", err)
		}
	})
}

func TestProvisionObservationCancellationDuringPollRemainsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, _ := openUnfinishedHelper(t, workspace.OperationProvision)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := awaitProvision(ctx, fixture.Store, fixture.Resource, genericDescribedHelperID)
			result <- err
		}()
		synctest.Wait()
		cancel()
		err := <-result
		if !errors.Is(err, workspace.ErrPending) || !errors.Is(err, context.Canceled) || PendingError(err) {
			t.Fatalf("canceled poll = %v; want pending helper plus cancellation, not retryable pending", err)
		}
	})
}

func TestProvisionObservationDeadlineControls(t *testing.T) {
	t.Run("deadline before any helper", func(t *testing.T) {
		admission := acceptedProvisionAdmission(t)
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		barrier := newProvisionBarrier(t, admission, ctx)
		barrier.install(admission)
		requireProvisionFault(t, "entry deadline", admission.provision(ctx), context.DeadlineExceeded)
		if barrier.opens != 0 || admission.admission.operationID != "" {
			t.Fatalf("entry deadline reached helper %q after %d opens", admission.admission.operationID, barrier.opens)
		}
	})
	t.Run("canceled observation", func(t *testing.T) {
		admission := acceptedProvisionAdmission(t)
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), provisionObserverBudget)
			defer cancel()
			barrier := newProvisionBarrier(t, admission, ctx)
			barrier.onHold = cancel
			barrier.install(admission)
			err := admission.provision(ctx)
			barrier.requireHeld(admission)
			requireProvisionFault(t, "canceled observation", err, context.Canceled)
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("canceled observation = %v, want no deadline", err)
			}
		})
	})
	t.Run("registration deadline after accepted observation", func(t *testing.T) {
		admission := acceptedProvisionAdmission(t)
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), provisionObserverBudget)
			defer cancel()
			barrier := newProvisionBarrier(t, admission, ctx)
			barrier.holdRegistration = true
			barrier.install(admission)
			err := admission.provision(ctx)
			barrier.requireHeld(admission)
			requireProvisionFault(t, "registration deadline", err, context.DeadlineExceeded)
		})
	})
}

// A live observer's inner deadline belongs to some other owner, so it stays a
// fault; only the observer's own expired deadline makes it a pending helper.
func TestPendingProvisionObservationRequiresObserverDeadline(t *testing.T) {
	requireProvisionFault(t, "live observer inner deadline", PendingAtDeadline(t.Context(), context.DeadlineExceeded), context.DeadlineExceeded)
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if err := PendingAtDeadline(expired, context.DeadlineExceeded); !errors.Is(err, workspace.ErrPending) || !errors.Is(err, context.DeadlineExceeded) || !PendingError(err) {
		t.Fatalf("expired observer inner deadline = %v, want pending", err)
	}
	joinedFault := errors.Join(context.DeadlineExceeded, context.Canceled)
	requireProvisionFault(t, "deadline joined with cancellation", PendingAtDeadline(expired, joinedFault), context.Canceled)
}

// ownerDeadlineFault lets an unfinished helper's recovery acquire its
// lifetime owner only after the observer's deadline. A non-nil release fails
// that owner's first unlock, and recovery returns the deadline joined with
// it; a nil release keeps every unlock real.
type ownerDeadlineFault struct {
	taskdir.BaseFaultInjector
	observer context.Context
	path     string
	release  error
	waited   bool
	fired    bool
}

func (f *ownerDeadlineFault) OnLockFileBarrier(string) error      { return nil }
func (f *ownerDeadlineFault) OnLockParentDirBarrier(string) error { return nil }
func (f *ownerDeadlineFault) OnFlock(path string, operation int) error {
	if path != f.path {
		return nil
	}
	if operation&unix.LOCK_EX != 0 && !f.waited {
		f.waited = true
		<-f.observer.Done()
		return nil
	}
	if operation == unix.LOCK_UN && f.release != nil && !f.fired {
		f.fired = true
		return f.release
	}
	return nil
}

func helperOwnerLock(store *taskdir.Store) string {
	return workspacefixture.OperationDirectory(store, genericDescribedHelperID) + "/.lifetime-owner.lock"
}

// The observer's deadline while it waits to inspect the helper's owner
// closure leaves that helper pending, and the observer releases the owner: a
// separate store then acquires it with a live context.
func TestProvisionObservationOwnerDeadlineKeepsHelperPending(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	persistUnfinishedProvision(t, fixture)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), provisionObserverBudget)
		defer cancel()
		fault := &ownerDeadlineFault{observer: ctx, path: helperOwnerLock(fixture.Store)}
		setStoreFault(t, fixture.Store, fault)
		_, err := awaitProvision(ctx, fixture.Store, fixture.Resource, genericDescribedHelperID)
		if !fault.waited || !errors.Is(err, workspace.ErrPending) || !errors.Is(err, context.DeadlineExceeded) || !PendingError(err) {
			t.Fatalf("provision observation = %v (owner waited %t, release fault %t); want pending at its deadline", err, fault.waited, fault.fired)
		}
	})
	store, err := taskdir.OpenStore(fixture.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	operation, err := workspace.LoadOperation(t.Context(), store, genericDescribedHelperID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := operation.TryAcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(errors.Join(err, operation.Close()))
	}
	if err = errors.Join(lease.Release(), operation.Close()); err != nil {
		t.Fatal(err)
	}
}

// The failed owner release keeps its lock until the child process exits; the
// parent then proves the owner free.
func TestProvisionObservationDeadlineKeepsJoinedReleaseFault(t *testing.T) {
	fixture := workspacefixture.OwnerReleaseRecoveryFixture(t, persistUnfinishedProvision, genericDescribedHelperID)
	if fixture == nil {
		return
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), provisionObserverBudget)
		defer cancel()
		fault := &ownerDeadlineFault{observer: ctx, path: helperOwnerLock(fixture.Store), release: errOwnerUnlock}
		setStoreFault(t, fixture.Store, fault)
		_, err := awaitProvision(ctx, fixture.Store, fixture.Resource, genericDescribedHelperID)
		if !fault.waited || !fault.fired || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("provision observation = %v (owner waited %t, release fault %t); want its deadline joined with the release fault", err, fault.waited, fault.fired)
		}
		requireProvisionFault(t, "faulted observation deadline", err, errOwnerUnlock)
	})
}
