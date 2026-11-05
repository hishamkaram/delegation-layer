package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// ownedResidentEntry is the sole owner of closure and resident resources. Any
// nonnil result, including on error, transfers cleanup ownership to the caller.
// The Store outlives its journal/endpoint borrowers. No same-UID adversarial
// pathname atomicity or inherited-flock proof is supplied by these checks.
type ownedResidentEntry struct {
	mu        sync.Mutex
	closed    bool
	closeErr  error
	reader    *taskdir.ReadOnlyStore
	store     *taskdir.Store
	journal   *intentJournal
	lease     *Lease
	endpoint  *ownedEndpoint
	operation Operation
	grant     *residentGrant
}

// consumeOwnerInvocation is only for the fixed trusted child birth path. FD 3
// must already be the parent's held instance lease; adoption does not prove
// that precondition. CLOEXEC is set before callbacks, probes or any child fork.
func consumeOwnerInvocation(ctx context.Context, invocation ownerInvocation, host EntryHost) (*ownedResidentEntry, error) {
	// Observe raw identity before taking ownership: a foreign runtime descriptor
	// must keep its flags and lifetime. Composition calls this before opening
	// other resources. Matching identity still does not attest a held flock.
	if err := matchOwnerInheritedDescriptor(invocation.root); err != nil {
		return nil, err
	}
	entry := &ownedResidentEntry{}
	file := os.NewFile(3, "infrastructure-instance-lease")
	lease, err := AdoptInheritedLease(file, filepath.Join(invocation.root, instanceLeaseName))
	if err != nil {
		return nil, err
	} // Adopt consumes the descriptor even on failure.
	entry.lease = lease
	// Invalid input still consumes FD3. No path-dependent borrowers exist yet.
	if ctx == nil {
		entry.closed = true
		if err = lease.Close(); err != nil {
			entry.closeErr = err
			return entry, errors.Join(os.ErrInvalid, err)
		}
		return nil, os.ErrInvalid
	}
	if err = entry.prepare(ctx, invocation, host); err != nil {
		return finishFailedEntry(ctx, entry, err)
	}
	return entry, nil
}

func matchOwnerInheritedDescriptor(root string) error {
	var descriptor, visible unix.Stat_t
	if err := unix.Fstat(3, &descriptor); err != nil {
		return err
	}
	if uint32(descriptor.Mode)&unix.S_IFMT != unix.S_IFREG {
		return ErrIdentity
	}
	if err := unix.Lstat(filepath.Join(root, instanceLeaseName), &visible); err != nil {
		return err
	}
	if taskdir.CheckPrivateStat(&visible, os.Geteuid(), false) != taskdir.PrivateStatAccepted || uint32(visible.Mode)&unix.S_IFMT != unix.S_IFREG || descriptor.Dev != visible.Dev || descriptor.Ino != visible.Ino {
		return ErrIdentity
	}
	return nil
}

func finishFailedEntry(ctx context.Context, entry *ownedResidentEntry, birthErr error) (*ownedResidentEntry, error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endpointCleanupTimeout)
	defer cancel()
	if cleanupErr := entry.closeContext(cleanupCtx); cleanupErr != nil {
		return entry, errors.Join(birthErr, cleanupErr)
	}
	return nil, birthErr
}

func (e *ownedResidentEntry) prepare(ctx context.Context, invocation ownerInvocation, host EntryHost) error {
	if err := e.prepareRoot(ctx, invocation, host); err != nil {
		return err
	}
	// Upgrade birth needs its lifecycle coordinator; refusing here must precede
	// any new assumption, endpoint or publication.
	if e.operation.Intent().Inputs.Kind == StartupUpgrade {
		return ErrIncompatible
	}
	return e.assumeAndPublish(ctx, invocation.startup)
}

// prepareRoot establishes the executing image and actual rooted borrowers before
// any assumption. Endpoint birth and publication remain separate owned phases.
func (e *ownedResidentEntry) prepareRoot(ctx context.Context, invocation ownerInvocation, host EntryHost) error {
	if ctx == nil || host == nil {
		return os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Inspect the actual birth environment before any filtering or mutable
	// store probe. A nil observation does not attest independence.
	environment := os.Environ()
	if err := lifetime.CheckCurrentTaskContainment(ctx, environment); err != nil {
		return err
	}
	if err := invocation.validate(); err != nil {
		return err
	}
	operation, pin, err := e.readOperation(ctx, invocation)
	if err != nil {
		return err
	}
	e.operation = operation
	if err = validateExecutingArtifacts(ctx, operation, host); err != nil {
		return err
	}
	e.store, err = taskdir.OpenStoreWithPredicatesContext(ctx, invocation.root, predicate.Default())
	if err != nil {
		return err
	}
	current, err := e.store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !pin.Equal(current) {
		return ErrIdentity
	}
	if err = e.reader.Close(); err != nil {
		e.closeErr = err
		return err
	}
	e.reader = nil
	e.journal, err = openIntentJournal(ctx, e.store, false)
	if err != nil {
		return err
	}
	return nil
}

func (e *ownedResidentEntry) readOperation(ctx context.Context, invocation ownerInvocation) (Operation, taskdir.RootPin, error) {
	var empty taskdir.RootPin
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, invocation.root, predicate.Default())
	if err != nil {
		return Operation{}, empty, err
	}
	e.reader = reader
	pin, err := reader.PinRootContext(ctx)
	if err != nil {
		return Operation{}, empty, err
	}
	if pin.Root() != invocation.root || pin.RootID() != invocation.rootID {
		return Operation{}, empty, ErrIdentity
	}
	intent, present, err := latestPinnedIntent(ctx, passiveIntentReader{reader}, pin)
	if err != nil {
		return Operation{}, empty, err
	}
	if !present {
		return Operation{}, empty, ErrIdentity
	}
	if err = invocation.startup.validateIntent(intent); err != nil {
		return Operation{}, empty, err
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		return Operation{}, empty, err
	}
	if err = validateCurrentIntent(intent, pin, bootID); err != nil {
		return Operation{}, empty, err
	}
	expired, err := intent.Expired(time.Now())
	if err != nil {
		return Operation{}, empty, err
	}
	if expired {
		return Operation{}, empty, ErrIdentity
	}
	operation, err := readEntryOperation(ctx, reader, pin, intent)
	return operation, pin, err
}

func validateExecutingArtifacts(ctx context.Context, operation Operation, host EntryHost) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if executable != operation.Artifacts().Runner {
		return ErrIdentity
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ErrIdentity
	}
	return host.ValidateArtifacts(ctx, operation.Intent().Inputs.Root, operation.Artifacts(), info)
}

// assumeAndBind creates the exact self assumption and its sequence endpoint.
// It mints no grant: publication and resident validation must still succeed.
func (e *ownedResidentEntry) assumeAndBind(ctx context.Context, reference StartupReference) (StartupAssumption, error) {
	if err := e.withCurrentIntent(ctx, reference); err != nil {
		return StartupAssumption{}, err
	}
	process, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return StartupAssumption{}, err
	}
	assumption := StartupAssumption{Startup: reference, Process: ProcessIdentity{PID: process.PID(), StartToken: process.StartToken()}, UID: os.Geteuid(), AssumedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	created, err := e.journal.putAssumption(ctx, e.lease, assumption)
	if err != nil {
		return StartupAssumption{}, err
	}
	if !created {
		return StartupAssumption{}, ErrIdentity
	}
	if err = e.withCurrentIntent(ctx, reference); err != nil {
		return StartupAssumption{}, err
	}
	e.endpoint, err = e.journal.listenEndpoint(ctx, e.lease, reference)
	if err != nil {
		return StartupAssumption{}, err
	}
	return assumption, nil
}

func (e *ownedResidentEntry) assumeAndPublish(ctx context.Context, reference StartupReference) error {
	assumption, err := e.assumeAndBind(ctx, reference)
	if err != nil {
		return err
	}
	digest, err := assumption.Digest(e.operation.Intent())
	if err != nil {
		return err
	}
	publication := OwnerPublication{Protocol: ProtocolVersion, Startup: reference, AssumptionSHA256: digest, RuntimeSHA256: e.operation.Artifacts().RunnerSHA256, Endpoint: e.endpoint.Identity()}
	created, err := e.journal.putPublication(ctx, e.lease, publication)
	if err != nil {
		return err
	}
	if !created {
		return ErrIdentity
	}
	// The grant is private and minted only after the complete successful birth.
	if err = e.validateResident(ctx); err != nil {
		return err
	}
	e.grant = &residentGrant{entry: e}
	return nil
}

// Close invalidates starts before closure and waits for any active callback.
// Endpoint cleanup may be retried with its original facts and dependencies.
// Other native close failures are retained as unproved closure, not converted
// to success by a second Close on an already-invalid descriptor wrapper.
func (e *ownedResidentEntry) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), endpointCleanupTimeout)
	defer cancel()
	return e.closeContext(ctx)
}

func (e *ownedResidentEntry) closeContext(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	if e.closeErr != nil {
		return e.closeErr
	}
	if e.endpoint != nil {
		if err := e.endpoint.closeContext(ctx); err != nil {
			return err
		}
		e.endpoint = nil
	}
	if e.lease != nil {
		if err := e.lease.Close(); err != nil {
			e.closeErr = err
			return err
		}
		e.lease = nil
	}
	if e.journal != nil {
		if err := e.journal.Close(); err != nil {
			e.closeErr = err
			return err
		}
		e.journal = nil
	}
	if e.reader != nil {
		if err := e.reader.Close(); err != nil {
			e.closeErr = err
			return err
		}
		e.reader = nil
	}
	if e.store != nil {
		if err := e.store.Close(); err != nil {
			e.closeErr = err
			return err
		}
		e.store = nil
	}
	return nil
}
