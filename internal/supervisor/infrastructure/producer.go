package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// EstablishmentRequest is an immutable selection, never executable provenance
// or native placement authority. Deadline belongs only to a newly accepted
// attempt; joining never replaces the existing attempt's deadline.
type EstablishmentRequest struct {
	Context  LaunchContext
	Initial  InitialOperation
	Deadline time.Time
}

// StartupAttempt owns parent-side startup resources, not the resident lifetime.
// The caller retains the borrowed Store until Close. Any nonnil result, even
// with an error, transfers cleanup ownership. Close never stops the resident;
// its sole native Wait remains registered while this parent process survives.
type StartupAttempt struct {
	mu       sync.Mutex
	target   *taskdir.Store
	pin      taskdir.RootPin
	reader   *taskdir.ReadOnlyStore
	journal  *intentJournal
	lease    *Lease
	intent   StartupIntent
	child    *producerChild
	closed   bool
	closeErr error
}

// Both trusted entry paths use the same acceptance, election and native child
// owner. The captured environment is private; requests cannot replace it.
func establishCaptured(ctx context.Context, target *taskdir.Store, request EstablishmentRequest, source capturedEstablishment, native producerNative) (*StartupAttempt, error) {
	if ctx == nil || target == nil || source.prepare == nil {
		return nil, os.ErrInvalid
	}
	request.Context = request.Context.Clone()
	// Validate before normalization so an Own request cannot hide a subject.
	if err := request.Initial.Validate(request.Context.RootID); err != nil {
		return nil, err
	}
	if err := validateRuntimeRestartProducer(request.Initial, source.starter.kind); err != nil {
		return nil, err
	}
	request.Initial = request.Initial.normalized()
	attempt := &StartupAttempt{target: target}
	if err := attempt.open(ctx, request); err != nil {
		return finishProducerFailure(attempt, err)
	}
	lease, acquired, err := TryAcquireLease(ctx, filepath.Join(attempt.pin.Root(), instanceLeaseName))
	attempt.lease = lease
	if err != nil {
		return finishProducerFailure(attempt, err)
	}
	if err = attempt.checkRoot(ctx); err != nil {
		return finishProducerFailure(attempt, err)
	}
	if !acquired {
		err = attempt.joinBusy(ctx, request)
	} else {
		err = attempt.elect(ctx, request, source, native)
	}
	if err != nil {
		// Durable acceptance/native birth remain reachable even after observation or
		// cleanup faults. The caller must reconcile this same reference, never retry Start.
		if attempt.intent.Sequence != 0 || attempt.child != nil {
			return attempt, err
		}
		return finishProducerFailure(attempt, err)
	}
	return attempt, nil
}

func (a *StartupAttempt) open(ctx context.Context, request EstablishmentRequest) error {
	if err := validateProducerDeadline(request.Deadline, time.Now()); err != nil {
		return err
	}
	pin, err := a.target.PinRootContext(ctx)
	if err != nil {
		return err
	}
	a.pin = pin
	if err = request.Initial.Validate(pin.RootID()); err != nil {
		return err
	}
	if err = request.Context.Validate(); err != nil {
		return err
	}
	if request.Context.Root != pin.Root() || request.Context.RootID != pin.RootID() || request.Context.OS != runtime.GOOS {
		return ErrIdentity
	}
	a.reader, err = taskdir.OpenReadOnlyStoreContext(ctx, pin.Root(), predicate.Default())
	if err != nil {
		return err
	}
	return a.checkRoot(ctx)
}

func (a *StartupAttempt) checkRoot(ctx context.Context) error {
	current, err := a.target.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !a.pin.Equal(current) {
		return ErrIdentity
	}
	if a.reader != nil {
		current, err = a.reader.PinRootContext(ctx)
		if err != nil {
			return err
		}
		if !a.pin.Equal(current) {
			return ErrIdentity
		}
	}
	return nil
}

func finishProducerFailure(attempt *StartupAttempt, cause error) (*StartupAttempt, error) {
	if err := attempt.Close(); err != nil {
		return attempt, errors.Join(cause, err)
	}
	return nil, cause
}

// Reference identifies accepted/matched target startup data. Zero means no
// matched durable acceptance; it is never a task-created or daemon-ready claim.
func (a *StartupAttempt) Reference() StartupReference {
	if a == nil {
		return StartupReference{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	reference, err := ReferenceStartup(a.intent)
	if err != nil {
		return StartupReference{}
	}
	return reference
}

// NativeCompletion is nil for joined attempts: this caller has no child Wait
// for them. A closed channel names only the exact locally started child's Wait.
func (a *StartupAttempt) NativeCompletion() <-chan struct{} {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.child == nil {
		return nil
	}
	return a.child.done
}

func (a *StartupAttempt) NativeWaitError() (error, bool) {
	if a == nil {
		return os.ErrInvalid, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.child == nil {
		return nil, false
	}
	return a.child.waitError()
}

// Close drops only parent references. It cannot certify resident closure or
// authorize root removal. Native Close faults remain sticky, never retried as
// false cleanup success after a terminal descriptor close.
func (a *StartupAttempt) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return a.closeErr
	}
	a.closed = true
	if a.lease != nil {
		a.closeErr = errors.Join(a.closeErr, a.lease.Close())
	}
	if a.journal != nil {
		a.closeErr = errors.Join(a.closeErr, a.journal.Close())
	}
	if a.reader != nil {
		a.closeErr = errors.Join(a.closeErr, a.reader.Close())
	}
	return a.closeErr
}

// New requests require a future deadline. Accepted records retain their own
// deadlines; joins never rewrite them. Wall-clock changes are not a hard
// elapsed-time guarantee.
func validateProducerDeadline(deadline, now time.Time) error {
	if _, err := canonicalStartupTime(deadline.UTC().Format(time.RFC3339Nano)); err != nil || deadline.IsZero() {
		return errors.Join(ErrIdentity, err)
	}
	if !deadline.After(now) {
		return context.DeadlineExceeded
	}
	return nil
}
