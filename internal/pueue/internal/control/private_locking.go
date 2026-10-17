package control

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// WithBootstrapLock serializes a state-root supervisor transition with normal
// private bootstrap. The callback runs while the same lock used by
// BindPrivate is held; it must perform only bounded, state-rooted recovery and
// return without retaining the lock or starting an unrelated supervisor.
func WithBootstrapLock(ctx context.Context, stateRoot string, callback func() error) error {
	if callback == nil {
		return ErrConfiguration
	}
	return WithBootstrapLockContext(ctx, stateRoot, func(context.Context) error { return callback() })
}

// WithBootstrapLockContext is WithBootstrapLock with the acquired generation
// lease attached to the callback context. Supervisor controls started with that
// context transfer a duplicate lease to their one-shot process owner.
func WithBootstrapLockContext(ctx context.Context, stateRoot string, callback func(context.Context) error) error {
	return withProvisionedPrivateLockContext(ctx, stateRoot, "bootstrap.lock", unix.LOCK_EX, callback)
}

// TryWithPrivateTransition elects one state-root transition owner without
// competing with shared supervisor leases. A false acquisition means another
// caller is already preparing this root; callers should wait for that
// transition before probing again instead of spinning through more controls.
func TryWithPrivateTransition(ctx context.Context, stateRoot string, callback func() error) (bool, error) {
	if callback == nil {
		return false, ErrConfiguration
	}
	if err := ensurePrivateTransitionElection(ctx, stateRoot); err != nil {
		return false, err
	}
	acquired := false
	err := withPrivateTransitionLock(ctx, stateRoot, unix.LOCK_EX|unix.LOCK_NB, func() error {
		acquired = true
		return callback()
	})
	return acquired, err
}

// tryWithPrivateQueueMaintenance elects one terminal-row cleanup owner for a
// private state root. Its dedicated lock keeps optional cleanup out of the
// lifecycle-transition election used by required admission and recovery work.
func tryWithPrivateQueueMaintenance(ctx context.Context, stateRoot string, callback func() error) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if callback == nil {
		return false, ErrConfiguration
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateLockFile(base, privateQueueMaintenanceLockName); err != nil {
		return false, err
	}
	acquired := false
	err := withPublishedPrivateLock(ctx, stateRoot, privateQueueMaintenanceLockName, unix.LOCK_EX|unix.LOCK_NB, func() error {
		acquired = true
		return callback()
	})
	return acquired, err
}

func withPrivateQueueMaintenance(ctx context.Context, stateRoot string, callback func() error) error {
	if ctx == nil {
		return context.Canceled
	}
	if callback == nil {
		return ErrConfiguration
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateLockFile(base, privateQueueMaintenanceLockName); err != nil {
		return err
	}
	return withPublishedPrivateLock(ctx, stateRoot, privateQueueMaintenanceLockName, unix.LOCK_EX, callback)
}

// runElectedPrivateQueueMaintenance handles the release handoff for scheduled
// work. A request published while an owner is finishing either gets observed
// after that owner releases the election or elects its own owner. Durable
// generations describe pending work; only the election lock describes a live
// owner.
func runElectedPrivateQueueMaintenance(ctx context.Context, stateRoot string, maintain func() error) (bool, error) {
	owned := false
	var resultErr error
	for {
		target, acquired, selected, err := runPrivateQueueMaintenanceElectionPass(ctx, stateRoot, maintain)
		if !acquired {
			// Another owner already holds the election. The wake was persisted
			// before this attempt, and that owner rechecks the journal after
			// releasing its lease. Callback processes therefore never queue
			// behind one another merely to observe an already-consumed wake.
			return owned, errors.Join(resultErr, err)
		}
		owned = owned || selected
		resultErr = errors.Join(resultErr, err)
		if err != nil && !selected {
			return owned, resultErr
		}
		wake, wakeErr := readPrivateQueueMaintenanceWake(ctx, stateRoot)
		if wakeErr != nil {
			return owned, errors.Join(resultErr, wakeErr)
		}
		if wake.Requested <= wake.Attempted {
			return owned, resultErr
		}
		if err != nil && wake.Attempted < target {
			// The failed owner did not settle the generation it selected. Keep
			// that wake durable for another callback or lifecycle invocation,
			// but do not spin this process on the same operational failure.
			return owned, resultErr
		}
	}
}

func runPrivateQueueMaintenanceElectionPass(ctx context.Context, stateRoot string, maintain func() error) (target uint64, acquired, selected bool, resultErr error) {
	acquired, resultErr = tryWithPrivateQueueMaintenance(ctx, stateRoot, func() error {
		wake, err := readPrivateQueueMaintenanceWake(ctx, stateRoot)
		if err != nil {
			return err
		}
		if err = settlePrivateQueueMaintenanceWake(ctx, stateRoot); err != nil {
			return err
		}
		wake, err = readPrivateQueueMaintenanceWake(ctx, stateRoot)
		if err != nil {
			return err
		}
		target = wake.Requested
		needed, retryErr := PrivateQueueMaintenanceRetryNeededContext(ctx, stateRoot)
		if retryErr != nil || !needed {
			return retryErr
		}
		selected = true
		return maintain()
	})
	return target, acquired, selected, resultErr
}

// preparePrivateQueueIdentityLocks returns the private root and provisions the
// writer-intent owner, short reader gate, and identity lock on a mutation path.
// Callers already hold the generation lease, so a lifecycle transition cannot
// replace the queue underneath them.
func (c *Client) preparePrivateQueueIdentityLocks() (string, bool, error) {
	if c == nil {
		return "", false, ErrConfiguration
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return "", false, nil
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	for _, name := range []string{privateQueueWriterIntentLockName, privateQueueIdentityGateLockName, privateQueueIdentityLockName} {
		if err := ensurePrivateLockFile(base, name); err != nil {
			return "", false, err
		}
	}
	return stateRoot, true, nil
}

// withPrivateQueueIdentityLease lets concurrent submissions allocate IDs and
// publish callback tickets. The short shared gate closes before the caller's
// work starts: an elected cleanup takes that gate exclusively, blocks new
// submissions, and lets existing identity leases drain without starvation.
func (c *Client) withPrivateQueueIdentityLease(ctx context.Context, callback func(context.Context) error) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if c == nil || callback == nil {
		return ErrConfiguration
	}
	stateRoot, private, err := c.preparePrivateQueueIdentityLocks()
	if err != nil {
		return err
	}
	if !private {
		return callback(ctx)
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	for {
		if err = waitForPrivateQueueWriter(ctx, base); err != nil {
			return err
		}
		var identity *bootstrapLock
		retry := false
		err = withPublishedPrivateLockContext(ctx, stateRoot, privateQueueIdentityGateLockName, unix.LOCK_SH, func(gateContext context.Context) error {
			active, intentErr := privateQueueWriterIntentActive(gateContext, base)
			if intentErr != nil {
				return intentErr
			}
			if active {
				retry = true
				return nil
			}
			var acquireErr error
			identity, acquireErr = acquireExistingSupervisorLock(gateContext, filepath.Join(base, privateQueueIdentityLockName), unix.LOCK_SH)
			return acquireErr
		})
		if err != nil {
			return err
		}
		if retry {
			continue
		}
		defer finishPrivateBootstrap(&identity, &resultErr)
		return callback(withControlLockLease(ctx, identity))
	}
}

func (c *Client) withPrivateQueueIdentityMutation(ctx context.Context, callback func(context.Context) error) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if c == nil || callback == nil {
		return ErrConfiguration
	}
	stateRoot, private, err := c.preparePrivateQueueIdentityLocks()
	if err != nil {
		return err
	}
	if !private {
		return callback(ctx)
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	intent, err := acquireExistingSupervisorLock(ctx, filepath.Join(base, privateQueueWriterIntentLockName), unix.LOCK_EX)
	if err != nil {
		return err
	}
	if err = writePrivateQueueWriterIntent(intent, true); err != nil {
		return errors.Join(err, closeBootstrapLock(intent))
	}
	defer finishPrivateQueueWriterIntent(&intent, &resultErr)
	intentContext := withControlLockLease(ctx, intent)
	resultErr = withPublishedPrivateLockContext(intentContext, stateRoot, privateQueueIdentityGateLockName, unix.LOCK_EX, func(gateContext context.Context) error {
		return withPublishedPrivateLockContext(gateContext, stateRoot, privateQueueIdentityLockName, unix.LOCK_EX, callback)
	})
	return resultErr
}

// waitForPrivateQueueWriter blocks new shared identity leases after a writer
// has published intent. Readers that crossed before publication are finite and
// drain through the gate; later readers cannot continuously overtake the
// waiting writer.
func waitForPrivateQueueWriter(ctx context.Context, base string) error {
	for {
		active, err := privateQueueWriterIntentActive(ctx, base)
		if err != nil || !active {
			return err
		}
		timer := time.NewTimer(privateQueueSettlementPoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// privateQueueWriterIntentActive reads the durable intent bit. If a previous
// writer exited before clearing it, the absence of its exclusive lock proves
// the bit stale and authorizes repair under that same lock.
func privateQueueWriterIntentActive(ctx context.Context, base string) (active bool, resultErr error) {
	path := filepath.Join(base, privateQueueWriterIntentLockName)
	data, err := readRegular(path, 2)
	if err != nil {
		return false, err
	}
	switch string(data) {
	case "", privateQueueWriterIdleState:
		return false, nil
	case privateQueueWriterWaitingState:
	default:
		return false, fmt.Errorf("%w: invalid private queue writer intent", ErrConfiguration)
	}
	intent, err := acquireExistingSupervisorLock(ctx, path, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, errSupervisorLockBusy) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer finishPrivateBootstrap(&intent, &resultErr)
	resultErr = writePrivateQueueWriterIntent(intent, false)
	return false, resultErr
}

func writePrivateQueueWriterIntent(lock *bootstrapLock, active bool) error {
	if lock == nil {
		return ErrConfiguration
	}
	state := privateQueueWriterIdleState
	if active {
		state = privateQueueWriterWaitingState
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed || lock.file == nil {
		return os.ErrClosed
	}
	if _, err := lock.file.WriteAt([]byte(state), 0); err != nil {
		return fmt.Errorf("%w: write private queue writer intent: %w", ErrConfiguration, err)
	}
	if err := lock.file.Truncate(int64(len(state))); err != nil {
		return fmt.Errorf("%w: truncate private queue writer intent: %w", ErrConfiguration, err)
	}
	if err := lock.file.Sync(); err != nil {
		return fmt.Errorf("%w: sync private queue writer intent: %w", ErrConfiguration, err)
	}
	return nil
}

func finishPrivateQueueWriterIntent(lock **bootstrapLock, resultErr *error) {
	if lock == nil || *lock == nil || resultErr == nil {
		return
	}
	pending := pendingHandles(*resultErr)
	if len(pending) == 0 {
		*resultErr = errors.Join(*resultErr, writePrivateQueueWriterIntent(*lock, false), closeBootstrapLock(*lock))
		*lock = nil
		return
	}
	retained := *lock
	*lock = nil
	go func() {
		for _, handle := range pending {
			if handle != nil {
				<-handle.Done()
			}
		}
		if err := errors.Join(writePrivateQueueWriterIntent(retained, false), closeBootstrapLock(retained)); err != nil {
			log.Printf("delegate: retained queue writer intent cleanup failed: %v", err)
		}
	}()
}

// WaitForPrivateTransition waits for the currently elected state-root
// transition, if any, to release its election lock. It never changes the
// supervisor itself; callers re-probe their required live state afterward.
func WaitForPrivateTransition(ctx context.Context, stateRoot string) error {
	present, err := privateTransitionLockPresent(stateRoot)
	if err != nil || !present {
		return err
	}
	return withPrivateTransitionLock(ctx, stateRoot, unix.LOCK_EX, func() error { return nil })
}

// WithSupervisorLease allows independent controls to share a stable supervisor
// generation. Lifecycle transitions require WithBootstrapLock and wait for all
// leases, including leases retained by unfinished commands, to drain.
func WithSupervisorLease(ctx context.Context, stateRoot string, callback func() error) error {
	if callback == nil {
		return ErrConfiguration
	}
	return WithSupervisorLeaseContext(ctx, stateRoot, func(context.Context) error { return callback() })
}

// WithSupervisorLeaseContext is WithSupervisorLease with the acquired
// generation lease attached to the callback context.
func WithSupervisorLeaseContext(ctx context.Context, stateRoot string, callback func(context.Context) error) error {
	return withPublishedPrivateLockContext(ctx, stateRoot, "bootstrap.lock", unix.LOCK_SH, callback)
}

func withProvisionedPrivateLockContext(ctx context.Context, stateRoot, name string, mode int, callback func(context.Context) error) error {
	return withPrivateLockContext(ctx, stateRoot, name, mode, true, callback)
}

func withPublishedPrivateLock(ctx context.Context, stateRoot, name string, mode int, callback func() error) error {
	return withPrivateLock(ctx, stateRoot, name, mode, false, callback)
}

func withPublishedPrivateLockContext(ctx context.Context, stateRoot, name string, mode int, callback func(context.Context) error) error {
	return withPrivateLockContext(ctx, stateRoot, name, mode, false, callback)
}

func withPrivateTransitionLock(ctx context.Context, stateRoot string, mode int, callback func() error) error {
	return withPublishedPrivateLock(ctx, stateRoot, privateTransitionLockName, mode, callback)
}

// withPrivateLock either provisions the bootstrap boundary for an exclusive
// lifecycle transition or acquires an already-published lock. Shared leases,
// observations and transition waits must never manufacture state:
// callers first decide whether a mutation is warranted, then enter an
// exclusive bootstrap boundary to provision it.
func withPrivateLock(ctx context.Context, stateRoot, name string, mode int, provision bool, callback func() error) (resultErr error) {
	if callback == nil {
		return ErrConfiguration
	}
	return withPrivateLockContext(ctx, stateRoot, name, mode, provision, func(context.Context) error { return callback() })
}

func withPrivateLockContext(ctx context.Context, stateRoot, name string, mode int, provision bool, callback func(context.Context) error) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if callback == nil || name == "" || filepath.Base(name) != name {
		return ErrConfiguration
	}
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return fmt.Errorf("%w: private supervisor root must be canonical and absolute", ErrConfiguration)
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if provision {
		if err := ensurePrivateBase(base); err != nil {
			return err
		}
	}
	present, err := privateLockFilePresent(base, name)
	if err != nil {
		return err
	}
	if !present {
		if !provision {
			return ErrPrivateSupervisorNotInitialized
		}
		return fmt.Errorf("%w: private supervisor lock is missing", ErrConfiguration)
	}
	lock, err := acquireExistingSupervisorLock(ctx, filepath.Join(base, name), mode)
	if err != nil {
		if mode&unix.LOCK_NB != 0 && errors.Is(err, errSupervisorLockBusy) {
			return nil
		}
		return err
	}
	defer finishPrivateBootstrap(&lock, &resultErr)
	return callback(withControlLockLease(ctx, lock))
}

func privateBootstrapLockPresent(stateRoot string) (bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return false, fmt.Errorf("%w: private supervisor root must be canonical and absolute", ErrConfiguration)
	}
	return privateLockFilePresent(filepath.Join(stateRoot, privateSupervisorDirectory), "bootstrap.lock")
}

// ensurePrivateTransitionElection installs the election lock only while the
// caller owns the bootstrap lock. A shared probe may discover a repair need,
// but it must never create a state-root entry before it has become the
// exclusive transition owner. Existing roots get this one-time lock upgrade
// through the same bootstrap boundary.
func ensurePrivateTransitionElection(ctx context.Context, stateRoot string) error {
	present, err := privateTransitionLockPresent(stateRoot)
	if err != nil || present {
		return err
	}
	return WithBootstrapLock(ctx, stateRoot, func() error {
		base := filepath.Join(stateRoot, privateSupervisorDirectory)
		if err := ensurePrivateBase(base); err != nil {
			return err
		}
		return ensurePrivateLockFile(base, privateTransitionLockName)
	})
}

func privateTransitionLockPresent(stateRoot string) (bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return false, fmt.Errorf("%w: private supervisor root must be canonical and absolute", ErrConfiguration)
	}
	return privateLockFilePresent(filepath.Join(stateRoot, privateSupervisorDirectory), privateTransitionLockName)
}
