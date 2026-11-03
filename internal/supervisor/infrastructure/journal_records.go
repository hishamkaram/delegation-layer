package infrastructure

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The journal owns create-once assumption/publication records. The fixed entry
// is the intended writer; the runtime coordinator and observers read evidence.
// A false created result, including an equal winner, grants no new authority.
func (j *intentJournal) putAssumption(ctx context.Context, lease *Lease, assumption StartupAssumption) (bool, error) {
	return j.putAssumptionWithClock(ctx, lease, assumption, time.Now)
}

// clock is invoked inside the serialized transaction, never before lock wait.
func (j *intentJournal) putAssumptionWithClock(ctx context.Context, lease *Lease, assumption StartupAssumption, clock func() time.Time) (created bool, resultErr error) {
	if clock == nil {
		return false, os.ErrInvalid
	}
	resultErr = j.withLeaseTransaction(ctx, lease, func(tx *taskdir.ControlTransaction) error {
		intent, err := j.currentReferencedIntent(tx, assumption.Startup)
		if err != nil {
			return err
		}
		if _, err = assumption.Digest(intent); err != nil {
			return err
		}
		if err = validateSelfAssumption(assumption); err != nil {
			return err
		}
		expired, err := intent.Expired(clock())
		if err != nil {
			return err
		}
		if expired {
			return fmt.Errorf("%w: original startup deadline expired before assumption", ErrIdentity)
		}
		created, err = tx.Put(assumptionName(intent.Sequence), assumption)
		return err
	})
	return created, resultErr
}

// Called only after the referenced intent establishes the current boot/root.
// The canonical observer compares native incarnations; record tokens alone are
// never interpreted as proof that a different process may assume this attempt.
func validateSelfAssumption(assumption StartupAssumption) error {
	if assumption.Process.PID != os.Getpid() || assumption.UID != os.Geteuid() {
		return ErrIdentity
	}
	reference, err := assumption.Process.Reference()
	if err != nil {
		return err
	}
	present, err := lifetime.ProcessPresent(reference)
	if err != nil {
		return fmt.Errorf("%w: observe assuming process: %w", ErrIdentity, err)
	}
	if !present {
		return fmt.Errorf("%w: assuming process incarnation does not match", ErrIdentity)
	}
	return nil
}

func (j *intentJournal) putPublication(ctx context.Context, lease *Lease, publication OwnerPublication) (created bool, resultErr error) {
	resultErr = j.withLeaseTransaction(ctx, lease, func(tx *taskdir.ControlTransaction) error {
		intent, err := j.currentReferencedIntent(tx, publication.Startup)
		if err != nil {
			return err
		}
		var assumption StartupAssumption
		if err = tx.Read(assumptionName(intent.Sequence), &assumption); err != nil {
			return err
		}
		if _, err = publication.Digest(intent, assumption); err != nil {
			return err
		}
		created, err = tx.Put(publicationName(intent.Sequence), publication)
		return err
	})
	return created, resultErr
}

func (j *intentJournal) currentReferencedIntent(tx *taskdir.ControlTransaction, reference StartupReference) (StartupIntent, error) {
	if err := reference.Validate(); err != nil {
		return StartupIntent{}, err
	}
	var intent StartupIntent
	if err := tx.Read(intentName(reference.Sequence), &intent); err != nil {
		return StartupIntent{}, err
	}
	if err := validateCurrentIntent(intent, j.pin, j.bootID); err != nil {
		return StartupIntent{}, err
	}
	if err := reference.validateIntent(intent); err != nil {
		return StartupIntent{}, err
	}
	return intent, nil
}

func assumptionName(sequence uint64) string { return fmt.Sprintf("assumption-%020d.json", sequence) }

func publicationName(sequence uint64) string { return fmt.Sprintf("publication-%020d.json", sequence) }

type ownerRecordReader interface {
	intentRecordReader
	assumption(context.Context, string) (StartupAssumption, error)
	publication(context.Context, string) (OwnerPublication, error)
}

type controlOwnerReader struct{ controlIntentReader }

func (r controlOwnerReader) assumption(ctx context.Context, name string) (StartupAssumption, error) {
	if err := ctx.Err(); err != nil {
		return StartupAssumption{}, err
	}
	var record StartupAssumption
	if err := r.control.Read(name, &record); err != nil {
		return StartupAssumption{}, err
	}
	return record, ctx.Err()
}

func (r controlOwnerReader) publication(ctx context.Context, name string) (OwnerPublication, error) {
	if err := ctx.Err(); err != nil {
		return OwnerPublication{}, err
	}
	var record OwnerPublication
	if err := r.control.Read(name, &record); err != nil {
		return OwnerPublication{}, err
	}
	return record, ctx.Err()
}

type passiveOwnerReader struct{ passiveIntentReader }

func (r passiveOwnerReader) assumption(ctx context.Context, name string) (StartupAssumption, error) {
	data, err := r.store.ReadInfrastructureRecordContext(ctx, name)
	if err != nil {
		return StartupAssumption{}, err
	}
	var record StartupAssumption
	if err = task.DecodeStrict(data, &record); err != nil {
		return StartupAssumption{}, err
	}
	return record, nil
}

func (r passiveOwnerReader) publication(ctx context.Context, name string) (OwnerPublication, error) {
	data, err := r.store.ReadInfrastructureRecordContext(ctx, name)
	if err != nil {
		return OwnerPublication{}, err
	}
	var record OwnerPublication
	if err = task.DecodeStrict(data, &record); err != nil {
		return OwnerPublication{}, err
	}
	return record, nil
}

func readReferencedIntent(ctx context.Context, reader intentRecordReader, pin taskdir.RootPin, reference StartupReference) (StartupIntent, error) {
	if err := reference.Validate(); err != nil {
		return StartupIntent{}, err
	}
	intent, err := readPinnedIntent(ctx, reader, pin, reference.Sequence)
	if err != nil {
		return StartupIntent{}, err
	}
	if err = reference.validateIntent(intent); err != nil {
		return StartupIntent{}, err
	}
	return intent, nil
}

func readAssumptionRecord(ctx context.Context, reader ownerRecordReader, pin taskdir.RootPin, reference StartupReference) (StartupAssumption, error) {
	intent, err := readReferencedIntent(ctx, reader, pin, reference)
	if err != nil {
		return StartupAssumption{}, err
	}
	record, err := reader.assumption(ctx, assumptionName(reference.Sequence))
	if err != nil {
		return StartupAssumption{}, err
	}
	if _, err = record.Digest(intent); err != nil {
		return StartupAssumption{}, err
	}
	return record, nil
}

func readPublicationRecord(ctx context.Context, reader ownerRecordReader, pin taskdir.RootPin, reference StartupReference) (OwnerPublication, error) {
	intent, err := readReferencedIntent(ctx, reader, pin, reference)
	if err != nil {
		return OwnerPublication{}, err
	}
	assumption, err := readAssumptionRecord(ctx, reader, pin, reference)
	if err != nil {
		return OwnerPublication{}, err
	}
	record, err := reader.publication(ctx, publicationName(reference.Sequence))
	if err != nil {
		return OwnerPublication{}, err
	}
	if _, err = record.Digest(intent, assumption); err != nil {
		return OwnerPublication{}, err
	}
	return record, nil
}

func (j *intentJournal) withHistoricalRecords(ctx context.Context, callback func(ownerRecordReader, taskdir.RootPin) error) error {
	return j.withOpen(ctx, func() error {
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		if err := callback(controlOwnerReader{controlIntentReader{j.control}}, j.pin); err != nil {
			return err
		}
		return j.checkRoot(ctx)
	})
}

func (j *intentJournal) readAssumption(ctx context.Context, reference StartupReference) (record StartupAssumption, resultErr error) {
	resultErr = j.withHistoricalRecords(ctx, func(reader ownerRecordReader, pin taskdir.RootPin) error {
		var err error
		record, err = readAssumptionRecord(ctx, reader, pin, reference)
		return err
	})
	return record, resultErr
}

func (j *intentJournal) readPublication(ctx context.Context, reference StartupReference) (record OwnerPublication, resultErr error) {
	resultErr = j.withHistoricalRecords(ctx, func(reader ownerRecordReader, pin taskdir.RootPin) error {
		var err error
		record, err = readPublicationRecord(ctx, reader, pin, reference)
		return err
	})
	return record, resultErr
}

// Passive history reads use current namespace pins, but never probe boot or
// process presence, acquire an instance lease, or reinterpret old native pins.
func withPassiveOwnerRecords(ctx context.Context, store *taskdir.ReadOnlyStore, callback func(ownerRecordReader, taskdir.RootPin) error) error {
	if store == nil {
		return os.ErrInvalid
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if err = callback(passiveOwnerReader{passiveIntentReader{store}}, pin); err != nil {
		return err
	}
	current, err := store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !pin.Equal(current) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func readStartupAssumption(ctx context.Context, store *taskdir.ReadOnlyStore, reference StartupReference) (record StartupAssumption, resultErr error) {
	resultErr = withPassiveOwnerRecords(ctx, store, func(reader ownerRecordReader, pin taskdir.RootPin) error {
		var err error
		record, err = readAssumptionRecord(ctx, reader, pin, reference)
		return err
	})
	return record, resultErr
}

func readOwnerPublication(ctx context.Context, store *taskdir.ReadOnlyStore, reference StartupReference) (record OwnerPublication, resultErr error) {
	resultErr = withPassiveOwnerRecords(ctx, store, func(reader ownerRecordReader, pin taskdir.RootPin) error {
		var err error
		record, err = readPublicationRecord(ctx, reader, pin, reference)
		return err
	})
	return record, resultErr
}
