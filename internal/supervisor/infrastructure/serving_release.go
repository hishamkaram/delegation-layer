package infrastructure

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// releaseServingContext is called only by the runtime finalizer after actual
// drain and retained-cleanup completion, while an owned daemon Wait is pending.
// The entry owns serving authority; its immutable settlement owns release
// evidence. The same process remains the sole waiter after serving closes.
func (e *ownedResidentEntry) releaseServingContext(ctx context.Context) (bool, error) {
	e.mu.Lock()
	if e.closeErr != nil {
		e.mu.Unlock()
		return false, e.closeErr
	}
	if !e.closed {
		if err := e.validateResident(ctx); err != nil {
			e.mu.Unlock()
			return false, err
		}
	}
	e.closed = true // Revoke every retained grant before endpoint/lease release.
	if e.endpoint != nil {
		if err := e.endpoint.closeContext(ctx); err != nil {
			e.mu.Unlock()
			return false, err
		}
		e.endpoint = nil
	}
	canRelease, recordErr := e.recordServingRelease(ctx)
	e.mu.Unlock()
	if !canRelease {
		return false, recordErr
	}
	closeErr := e.closeContext(ctx)
	return closeErr == nil, errors.Join(recordErr, closeErr)
}

// No outside settler can author this positive serving-release reason. The
// fixed resident binds its current native self, publication and held lease.
func (e *ownedResidentEntry) recordServingRelease(ctx context.Context) (bool, error) {
	if e.journal == nil || e.lease == nil || e.endpoint != nil {
		return false, ErrIdentity
	}
	reference, err := ReferenceStartup(e.operation.Intent())
	if err != nil {
		return false, err
	}
	j := e.journal
	var putFailure *servingReleasePutError
	transactionErr := j.withLeaseTransaction(ctx, e.lease, func(tx *taskdir.ControlTransaction) error {
		reader := transactionStartupReader{transactionIntentReader{j.control, tx}}
		evidence, boot, listed, readErr := j.currentSettlementEvidence(ctx, reader, reference)
		if readErr != nil {
			return readErr
		}
		if evidence.assumption == nil || evidence.publication == nil || lifetime.CompareBootIDs(evidence.intent.Inputs.BootID, boot) != lifetime.BootSame {
			return ErrIdentity
		}
		if readErr = validateSelfAssumption(*evidence.assumption); readErr != nil {
			return readErr
		}
		attempted, writeErr := writeServingRelease(tx, evidence, reference, boot, listed)
		if attempted && writeErr != nil {
			putFailure = &servingReleasePutError{cause: writeErr}
			return putFailure
		}
		return writeErr
	})
	// Only the actual Put failure permits unfenced serving-resource release.
	// Identity/read/verification or independent transaction-cleanup faults keep
	// the lease. Without a fence, the ordinary live-PID settler still refuses a
	// successor until this same process has joined its daemon Wait and exited.
	canRelease := transactionErr == nil || putFailure != nil && onlyServingReleasePutFailure(transactionErr, putFailure)
	return canRelease, transactionErr
}

// The exact callback boundary preserves provenance when Put and transaction
// cleanup report the same errno. Equal leaf errors do not grant release.
type servingReleasePutError struct{ cause error }

func (e *servingReleasePutError) Error() string { return e.cause.Error() }
func (e *servingReleasePutError) Unwrap() error { return e.cause }
func (e *servingReleasePutError) servingReleasePutBoundary() *servingReleasePutError {
	return e
}

func onlyServingReleasePutFailure(cause error, expected *servingReleasePutError) bool {
	// Inspect this node's provenance before unwrapping; As/Is can match a different inner boundary.
	if boundary, ok := cause.(interface {
		servingReleasePutBoundary() *servingReleasePutError
	}); ok {
		return boundary.servingReleasePutBoundary() == expected
	}
	if joined, ok := cause.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyServingReleasePutFailure(child, expected) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := cause.(interface{ Unwrap() error }); ok {
		return onlyServingReleasePutFailure(wrapped.Unwrap(), expected)
	}
	return false
}

func writeServingRelease(tx *taskdir.ControlTransaction, evidence settlementEvidence, reference StartupReference, boot string, listed bool) (bool, error) {
	var record startupSettlement
	readErr := tx.Read(settlementName(reference.Sequence), &record)
	if readErr == nil {
		if record.Reason != settlementServingReleased {
			return false, ErrIdentity
		}
		if readErr = evidence.validate(record); readErr != nil {
			return false, readErr
		}
		return false, tx.Verify(settlementName(reference.Sequence), record)
	}
	if !task.RecordAbsent(readErr) {
		return false, readErr
	}
	if listed {
		return false, errors.Join(task.ErrEvidenceFault, readErr)
	}
	record = startupSettlement{Protocol: RecordProtocolVersion, Startup: reference, ObservedBootID: boot, SettledAt: time.Now().UTC().Format(time.RFC3339Nano), Reason: settlementServingReleased}
	if readErr = evidence.bind(&record); readErr != nil {
		return false, readErr
	}
	if readErr = record.Validate(evidence.intent); readErr != nil {
		return false, readErr
	}
	_, readErr = tx.Put(settlementName(reference.Sequence), record)
	return true, readErr
}
