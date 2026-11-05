package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const instanceLeaseName = ".infrastructure-owner.lock"

// intentJournal owns only its ControlDir and closure state. The caller owns
// the borrowed Store and closes it afterward. RootPin is an observation; this
// journal provides neither election policy nor permission to start a process.
type intentJournal struct {
	mu      sync.RWMutex
	closed  bool
	store   *taskdir.Store
	control *taskdir.ControlDir
	pin     taskdir.RootPin
	bootID  string
}

func openIntentJournal(ctx context.Context, store *taskdir.Store, create bool) (*intentJournal, error) {
	if store == nil {
		return nil, os.ErrInvalid
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return nil, err
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		return nil, err
	}
	if lifetime.CompareBootIDs(bootID, bootID) != lifetime.BootSame {
		return nil, fmt.Errorf("%w: journal requires a known stable boot identity", ErrIdentity)
	}
	control, err := store.OpenInfrastructureContext(ctx, create)
	if err != nil {
		return nil, err
	}
	current, err := store.PinRootContext(ctx)
	if err == nil && !pin.Equal(current) {
		err = task.ErrIdentityMismatch
	}
	if err != nil {
		return nil, errors.Join(err, control.Close())
	}
	return &intentJournal{store: store, control: control, pin: pin, bootID: bootID}, nil
}

func (j *intentJournal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.closed = true
	if j.control == nil {
		return os.ErrClosed
	}
	return j.control.Close()
}

func (j *intentJournal) withOpen(ctx context.Context, callback func() error) error {
	if j == nil || ctx == nil {
		return os.ErrInvalid
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	if j.closed || j.store == nil || j.control == nil {
		return os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return callback()
}

func (j *intentJournal) checkRoot(ctx context.Context) error {
	current, err := j.store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !j.pin.Equal(current) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (j *intentJournal) tryAcquire(ctx context.Context) (lease *Lease, acquired bool, resultErr error) {
	resultErr = j.withOpen(ctx, func() error {
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		var err error
		lease, acquired, err = TryAcquireLease(ctx, filepath.Join(j.pin.Root(), instanceLeaseName))
		if err != nil {
			return err
		}
		if err = j.checkRoot(ctx); err != nil {
			if lease != nil {
				err = errors.Join(err, lease.Close())
			}
			lease, acquired = nil, false
			return err
		}
		return nil
	})
	return lease, acquired, resultErr
}

// putIntent performs only a short canonical record transaction. created=false
// is never permission to replace an intent or perform a fresh native Start.
func (j *intentJournal) putIntent(ctx context.Context, lease *Lease, intent StartupIntent) (created bool, resultErr error) {
	resultErr = j.withLeaseTransaction(ctx, lease, func(tx *taskdir.ControlTransaction) error {
		if err := validateCurrentIntent(intent, j.pin, j.bootID); err != nil {
			return err
		}
		reader := transactionLifecycleReader{transactionStartupReader{transactionIntentReader{j.control, tx}}}
		if err := requireLifecycleStartup(ctx, reader, j.pin, intent); err != nil {
			return err
		}
		if err := validateIntentAppend(ctx, controlIntentReader{j.control}, j.pin, intent); err != nil {
			return err
		}
		_, selected, selectionErr := lifecycleStartupSelection(ctx, reader, j.pin, intent)
		if selectionErr != nil {
			return selectionErr
		}
		if _, err := readContextRecords(ctx, transactionContextReader{tx}, selected, j.pin); err != nil {
			return err
		}
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		var err error
		created, err = tx.Put(intentName(intent.Sequence), intent)
		return err
	})
	return created, resultErr
}

// Sequence and epoch facts are derived from immutable records while the same
// canonical transaction serializes writers. Existing equal records remain
// retries; neither historical replay nor gap filling elects a fresh attempt.
func validateIntentAppend(ctx context.Context, reader intentRecordReader, pin taskdir.RootPin, intent StartupIntent) error {
	latest, found, err := latestPinnedIntent(ctx, reader, pin)
	if err != nil {
		return err
	}
	names, err := reader.names(ctx)
	if err != nil {
		return err
	}
	existing := false
	for _, name := range names {
		if !strings.HasPrefix(name, "intent-") {
			continue
		}
		sequence, parseErr := intentSequence(name)
		if parseErr != nil {
			return parseErr
		}
		saved, readErr := readPinnedIntent(ctx, reader, pin, sequence)
		if readErr != nil {
			return errors.Join(task.ErrEvidenceFault, readErr)
		}
		if sequence == intent.Sequence {
			existing = true
			continue
		}
		if saved.Epoch == intent.Epoch {
			return fmt.Errorf("%w: startup epoch already belongs to another sequence", ErrIdentity)
		}
	}
	if existing {
		return nil
	}
	var last uint64
	if found {
		last = latest.Sequence
	}
	if last == ^uint64(0) {
		return fmt.Errorf("%w: startup sequence exhausted", ErrIdentity)
	}
	if intent.Sequence != last+1 {
		return fmt.Errorf("%w: new startup intent must append the next sequence", ErrIdentity)
	}
	return nil
}

// Lifecycle record mutators borrow the same actual instance lease and current root
// inside a short record transaction. Nothing in this closure may start a process
// or wait for readiness; the runtime coordinator owns those decisions.
func (j *intentJournal) withLeaseTransaction(ctx context.Context, lease *Lease, callback func(*taskdir.ControlTransaction) error) error {
	return j.withOpen(ctx, func() error {
		if lease == nil || lease.path != filepath.Join(j.pin.Root(), instanceLeaseName) {
			return ErrIdentity
		}
		return lease.WithHeld(ctx, func() error {
			if err := j.checkRoot(ctx); err != nil {
				return err
			}
			return j.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
				if err := j.checkRoot(ctx); err != nil {
					return err
				}
				return callback(tx)
			})
		})
	})
}

func (j *intentJournal) readIntent(ctx context.Context, sequence uint64) (intent StartupIntent, resultErr error) {
	resultErr = j.withOpen(ctx, func() error {
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		var err error
		intent, err = readPinnedIntent(ctx, controlIntentReader{j.control}, j.pin, sequence)
		return err
	})
	return intent, resultErr
}

func (j *intentJournal) latestIntent(ctx context.Context) (intent StartupIntent, found bool, resultErr error) {
	resultErr = j.withOpen(ctx, func() error {
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		var err error
		intent, found, err = latestPinnedIntent(ctx, controlIntentReader{j.control}, j.pin)
		return err
	})
	return intent, found, resultErr
}

// readLatestIntent is passive. Missing journal storage is absence; a listed
// record that disappears while being read is instead an evidence fault.
func readLatestIntent(ctx context.Context, store *taskdir.ReadOnlyStore) (StartupIntent, bool, error) {
	if store == nil {
		return StartupIntent{}, false, os.ErrInvalid
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return StartupIntent{}, false, err
	}
	intent, found, err := latestPinnedIntent(ctx, passiveIntentReader{store}, pin)
	if err != nil {
		return StartupIntent{}, false, err
	}
	current, err := store.PinRootContext(ctx)
	if err == nil && !pin.Equal(current) {
		err = task.ErrIdentityMismatch
	}
	if err != nil {
		return StartupIntent{}, false, err
	}
	return intent, found, nil
}

type intentRecordReader interface {
	names(context.Context) ([]string, error)
	read(context.Context, string) (StartupIntent, error)
}

type controlIntentReader struct{ control *taskdir.ControlDir }

func (r controlIntentReader) names(ctx context.Context) ([]string, error) {
	return r.control.RecordNamesContext(ctx)
}

func (r controlIntentReader) read(ctx context.Context, name string) (StartupIntent, error) {
	if err := ctx.Err(); err != nil {
		return StartupIntent{}, err
	}
	var intent StartupIntent
	if err := r.control.Read(name, &intent); err != nil {
		return StartupIntent{}, err
	}
	return intent, ctx.Err()
}

type passiveIntentReader struct{ store *taskdir.ReadOnlyStore }

func (r passiveIntentReader) names(ctx context.Context) ([]string, error) {
	return r.store.InfrastructureRecordNamesContext(ctx)
}

func (r passiveIntentReader) read(ctx context.Context, name string) (StartupIntent, error) {
	data, err := r.store.ReadInfrastructureRecordContext(ctx, name)
	if err != nil {
		return StartupIntent{}, err
	}
	var intent StartupIntent
	if err = task.DecodeStrict(data, &intent); err != nil {
		return StartupIntent{}, err
	}
	return intent, nil
}

func intentName(sequence uint64) string { return fmt.Sprintf("intent-%020d.json", sequence) }

func intentSequence(name string) (uint64, error) {
	raw := strings.TrimSuffix(strings.TrimPrefix(name, "intent-"), ".json")
	sequence, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || sequence == 0 || name != intentName(sequence) {
		return 0, errors.Join(task.ErrEvidenceFault, err)
	}
	return sequence, nil
}

func validateHistoricalIntent(intent StartupIntent, pin taskdir.RootPin) error {
	if _, err := intent.Digest(); err != nil {
		return err
	}
	inputs := intent.Inputs
	if inputs.Root != pin.Root() || inputs.RootID != pin.RootID() {
		return fmt.Errorf("%w: startup intent does not match journal root", ErrIdentity)
	}
	return nil
}

// Historical records belong to the namespace even after a reboot changes native
// coordinates. Only a current writer additionally binds the live boot and pin.
func validateCurrentIntent(intent StartupIntent, pin taskdir.RootPin, bootID string) error {
	if err := validateHistoricalIntent(intent, pin); err != nil {
		return err
	}
	inputs := intent.Inputs
	if lifetime.CompareBootIDs(inputs.BootID, bootID) != lifetime.BootSame || inputs.RootDevice != pin.Device() || inputs.RootInode != pin.Inode() {
		return fmt.Errorf("%w: startup intent does not match current boot and native root", ErrIdentity)
	}
	return nil
}

func readPinnedIntent(ctx context.Context, reader intentRecordReader, pin taskdir.RootPin, sequence uint64) (StartupIntent, error) {
	if sequence == 0 {
		return StartupIntent{}, ErrIdentity
	}
	intent, err := reader.read(ctx, intentName(sequence))
	if err != nil {
		return StartupIntent{}, err
	}
	if err = validateHistoricalIntent(intent, pin); err != nil {
		return StartupIntent{}, err
	}
	if intent.Sequence != sequence {
		return StartupIntent{}, task.ErrEvidenceFault
	}
	return intent, nil
}

func latestPinnedIntent(ctx context.Context, reader intentRecordReader, pin taskdir.RootPin) (StartupIntent, bool, error) {
	names, err := reader.names(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return StartupIntent{}, false, nil
	}
	if err != nil {
		return StartupIntent{}, false, err
	}
	var latest uint64
	for _, name := range names {
		if !strings.HasPrefix(name, "intent-") {
			continue
		}
		sequence, parseErr := intentSequence(name)
		if parseErr != nil {
			return StartupIntent{}, false, parseErr
		}
		latest = max(latest, sequence)
	}
	if latest == 0 {
		return StartupIntent{}, false, nil
	}
	intent, err := readPinnedIntent(ctx, reader, pin, latest)
	if err != nil {
		return StartupIntent{}, false, errors.Join(task.ErrEvidenceFault, err)
	}
	return intent, true, nil
}
