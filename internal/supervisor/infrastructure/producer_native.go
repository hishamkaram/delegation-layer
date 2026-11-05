package infrastructure

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

// Only tests replace these native operations. The public producer always uses
// the accepted custody image and fixed argv; no RPC or host selects a command.
type (
	producerNative struct {
		start   func(*exec.Cmd) error
		wait    func(*exec.Cmd) error
		observe func(int) (lifetime.ProcessRef, error)
	}
	producerChild struct {
		done     chan struct{}
		mu       sync.Mutex
		err      error
		pid      int
		identity ProcessIdentity
	}
)

func (c *producerChild) waitError() (error, bool) {
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.err, true
	default:
		return nil, false
	}
}

func (a *StartupAttempt) acceptAndStart(ctx context.Context, intent StartupIntent, originalEnvironment []string, native producerNative) error {
	expired, err := intent.Expired(time.Now())
	if err != nil {
		return err
	}
	if expired {
		return context.DeadlineExceeded
	}
	created, err := a.journal.putIntent(ctx, a.lease, intent)
	if created {
		a.intent = intent
	}
	if err != nil {
		return err
	}
	if !created {
		return ErrIdentity
	}
	deadline, err := canonicalStartupTime(intent.Deadline)
	if err != nil {
		return err
	}
	// Accepted responsibility survives observer cancellation without renewing the
	// immutable startup deadline. No command is tied to CommandContext cancellation.
	operation, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	command, err := newProducerCommand(intent, originalEnvironment)
	if err != nil {
		return err
	}
	startErr := a.lease.WithHeldFile(operation, func(file *os.File) error {
		if err := a.validateBirth(operation, intent, originalEnvironment); err != nil {
			return err
		}
		command.ExtraFiles = []*os.File{file}
		defer func() { command.ExtraFiles = nil }()
		if native.start != nil {
			return native.start(command)
		}
		return command.Start()
	})
	a.registerNativeChild(command, native)
	closeErr := a.releaseElection()
	observeErr := a.observeNativeChild(native)
	if startErr == nil && a.child == nil {
		startErr = ErrIdentity
	}
	return errors.Join(startErr, closeErr, observeErr, a.checkRoot(operation))
}

func newProducerCommand(intent StartupIntent, environment []string) (*exec.Cmd, error) {
	reference, err := ReferenceStartup(intent)
	if err != nil {
		return nil, err
	}
	arguments, err := ownerArguments(intent.Inputs.Root, intent.Inputs.RootID, reference)
	if err != nil {
		return nil, err
	}
	command := exec.Command(intent.Inputs.Artifacts.Runner, arguments...)
	command.Dir = intent.Inputs.Root
	command.Env = slices.Clone(environment)
	if err = lifetime.ConfigureIndependentSession(command); err != nil {
		return nil, err
	}
	return command, nil
}

func (a *StartupAttempt) validateBirth(ctx context.Context, intent StartupIntent, environment []string) error {
	if err := a.checkRoot(ctx); err != nil {
		return err
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return err
	}
	if err = validateCurrentIntent(intent, a.pin, boot); err != nil {
		return err
	}
	if err = requireLifecycleStartup(ctx, a.currentLifecycleReader(), a.pin, intent); err != nil {
		return err
	}
	if err = lifetime.CheckCurrentTaskContainment(ctx, environment); err != nil {
		return err
	}
	expired, err := intent.Expired(time.Now())
	if err != nil {
		return err
	}
	if expired {
		return context.DeadlineExceeded
	}
	return nil
}

func (a *StartupAttempt) registerNativeChild(command *exec.Cmd, native producerNative) {
	if command.Process == nil {
		return
	}
	child := &producerChild{done: make(chan struct{}), pid: command.Process.Pid}
	a.child = child
	// Registration precedes every post-Start observation, including failure.
	go waitProducerChild(child, command, native)
}

// The parent's duplicate is closed before probing the post-exec child. Its
// inherited descriptor alone must keep the native lease held across handoff.
func (a *StartupAttempt) observeNativeChild(native producerNative) error {
	if a.child == nil {
		return nil
	}
	observe := lifetime.ObserveStartedProcess
	if native.observe != nil {
		observe = native.observe
	}
	reference, err := observe(a.child.pid)
	if err == nil {
		a.child.identity = ProcessIdentity{PID: reference.PID(), StartToken: reference.StartToken()}
	}
	return err
}

func waitProducerChild(child *producerChild, command *exec.Cmd, native producerNative) {
	var err error
	if native.wait != nil {
		err = native.wait(command)
	} else {
		err = command.Wait()
	}
	child.mu.Lock()
	child.err = err
	child.mu.Unlock()
	close(child.done)
}

// Observe authenticates only the current resident instance. It does not send
// an operation or infer daemon readiness. The observer supplies a finite context;
// a resident remains valid after the original startup deadline.
func (a *StartupAttempt) Observe(ctx context.Context) (publication OwnerPublication, ready bool, resultErr error) {
	if a == nil || ctx == nil {
		return publication, false, os.ErrInvalid
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.reader == nil {
		return publication, false, os.ErrClosed
	}
	if _, finite := ctx.Deadline(); !finite {
		return publication, false, os.ErrInvalid
	}
	intent, evidence, err := a.observationRecords(ctx)
	if err != nil {
		return publication, false, err
	}
	pending, err := pendingOwnerPublication(intent, evidence)
	if err != nil || pending {
		return publication, false, err
	}
	publication = *evidence.publication
	if err = a.matchObservedChild(*evidence.assumption); err != nil {
		return publication, false, err
	}
	if err = checkOwnerContactEndpoint(ctx, a.reader, a.pin, publication.Endpoint); err != nil {
		return publication, false, err
	}
	connection, err := dialOwnerContact(ctx, publication.Endpoint.Path)
	if err != nil {
		return publication, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	if err = authenticateOwnerPeer(ctx, connection, intent, *evidence.assumption, publication); err != nil {
		return publication, false, err
	}
	if err = checkOwnerContactEndpoint(ctx, a.reader, a.pin, publication.Endpoint); err != nil {
		return publication, false, err
	}
	if err = a.checkRoot(ctx); err != nil {
		return publication, false, err
	}
	return publication, true, nil
}

func (a *StartupAttempt) releaseElection() error {
	if a.lease == nil {
		return nil
	}
	err := a.lease.Close()
	if err != nil {
		a.closeErr = errors.Join(a.closeErr, err)
	} else {
		a.lease = nil
	}
	return err
}

func (a *StartupAttempt) observationRecords(ctx context.Context) (StartupIntent, settlementEvidence, error) {
	if err := a.checkRoot(ctx); err != nil {
		return StartupIntent{}, settlementEvidence{}, err
	}
	reference, err := ReferenceStartup(a.intent)
	if err != nil {
		return StartupIntent{}, settlementEvidence{}, err
	}
	intent, err := readOwnerContactIntent(ctx, a.reader, a.pin, reference)
	if err != nil {
		return StartupIntent{}, settlementEvidence{}, err
	}
	evidence, err := loadSettlementEvidence(ctx, passiveOwnerReader{passiveIntentReader{a.reader}}, a.pin, reference)
	return intent, evidence, err
}

func (a *StartupAttempt) matchObservedChild(assumption StartupAssumption) error {
	if a.child == nil {
		return nil
	}
	if assumption.Process.PID != a.child.pid {
		return ErrIdentity
	}
	if a.child.identity != (ProcessIdentity{}) {
		observed, err := a.child.identity.Reference()
		if err != nil {
			return err
		}
		persisted, err := assumption.Process.Reference()
		if err != nil {
			return err
		}
		match, err := lifetime.SameProcess(observed, persisted)
		if err != nil {
			return err
		}
		if !match {
			return ErrIdentity
		}
	}
	return nil
}

// Missing publication is pending only while an unassumed attempt retains its
// original deadline. An assumed resident is not expired by that startup clock.
func pendingOwnerPublication(intent StartupIntent, evidence settlementEvidence) (bool, error) {
	if evidence.assumption != nil {
		return evidence.publication == nil, nil
	}
	expired, err := intent.Expired(time.Now())
	if err != nil {
		return false, err
	}
	if expired {
		return false, context.DeadlineExceeded
	}
	return true, nil
}
