package infrastructure

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func contextName(reference LaunchContextReference) (string, error) {
	if err := reference.Validate(); err != nil {
		return "", err
	}
	// ControlDir limits names to 64 bytes. The complete digest remains the
	// reference authority; create-once comparison refuses a prefix collision.
	return "context-" + reference.SHA256[:32] + ".json", nil
}

// stageContext owns immutable input content, not lifecycle state. Trusted
// composition may stage it while B's resident holds the instance lease; the
// canonical record transaction and fresh target pin protect this publication.
// No staged record alone grants execution authority. Acceptance must bind its
// exact digest into the operation under the resident's actual instance lease.
func (j *intentJournal) stageContext(ctx context.Context, record LaunchContext) (reference LaunchContextReference, created bool, resultErr error) {
	record = record.Clone()
	resultErr = j.withOpen(ctx, func() error {
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		digest, err := record.Digest()
		if err != nil {
			return err
		}
		reference = LaunchContextReference{SHA256: digest}
		if err = reference.ValidateContext(record, j.pin.Root(), j.pin.RootID()); err != nil {
			return err
		}
		name, err := contextName(reference)
		if err != nil {
			return err
		}
		if err = j.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
			if rootErr := j.checkRoot(ctx); rootErr != nil {
				return rootErr
			}
			var putErr error
			created, putErr = tx.Put(name, record)
			return putErr
		}); err != nil {
			return err
		}
		return j.checkRoot(ctx)
	})
	return reference, created, resultErr
}

func (j *intentJournal) readContext(ctx context.Context, reference LaunchContextReference) (record LaunchContext, resultErr error) {
	resultErr = j.withOpen(ctx, func() error {
		name, err := contextName(reference)
		if err != nil {
			return err
		}
		if err = j.checkRoot(ctx); err != nil {
			return err
		}
		if err = j.control.Read(name, &record); err != nil {
			return redactedContextReadError{cause: err}
		}
		if err := reference.ValidateContext(record, j.pin.Root(), j.pin.RootID()); err != nil {
			return err
		}
		return j.checkRoot(ctx)
	})
	return record, resultErr
}

// Passive content reads share the history boundary: no boot/process probe,
// instance election, repair or substitution of an observer's environment.
func readLaunchContext(ctx context.Context, store *taskdir.ReadOnlyStore, reference LaunchContextReference) (record LaunchContext, resultErr error) {
	name, err := contextName(reference)
	if err != nil {
		return LaunchContext{}, err
	}
	resultErr = withPassiveOwnerRecords(ctx, store, func(_ ownerRecordReader, pin taskdir.RootPin) error {
		data, err := store.ReadInfrastructureRecordContext(ctx, name)
		if err != nil {
			return err
		}
		if err = task.DecodeStrict(data, &record); err != nil {
			// The decoder can quote an input token. Environment values must not
			// escape through malformed-record diagnostics.
			return fmt.Errorf("%w: %w", task.ErrEvidenceFault, redactedContextReadError{cause: err})
		}
		return reference.ValidateContext(record, pin.Root(), pin.RootID())
	})
	return record, resultErr
}

// Decoder errors may quote input tokens. Preserve classification and the cause
// for errors.Is/As without rendering unrestricted environment record contents.
type redactedContextReadError struct{ cause error }

func (redactedContextReadError) Error() string {
	return "infrastructure launch context unavailable or invalid"
}
func (e redactedContextReadError) Unwrap() error { return e.cause }
