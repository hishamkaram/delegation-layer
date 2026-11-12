package taskdir

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ControlLifetimeRecords is a read-only view of one validated control lifetime.
// Its methods are valid only inside the transaction callback that created it.
type ControlLifetimeRecords interface {
	LifetimeRecordReader
	ReadLifetimeUnlaunchedContext(context.Context) (*task.LifetimeUnlaunchedRecord, error)
}

// ControlLifetimeObservation contains scoped read facts, not lifecycle
// authority. PreparationOnly means the family owner verified its exact
// pre-submission inventory; it never means a lifetime was closed.
type ControlLifetimeObservation struct {
	Records         ControlLifetimeRecords
	OwnerPresent    bool
	PreparationOnly bool
}

// TransactionReader reuses canonical lifetime readers under an already held
// transaction. It grants no transition or write methods and never reacquires
// the operation lock. An escaped reader refuses after its callback ends.
func (l *ControlLifetime) TransactionReader(tx *ControlTransaction) (ControlLifetimeRecords, error) {
	if err := l.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	if tx == nil || tx.d != l.control {
		return nil, task.ErrInvalidPermit
	}
	if err := tx.enter(); err != nil {
		return nil, err
	}
	tx.leave()
	return controlLifetimeTransactionReader{tx: tx, lifetime: l}, nil
}

func (r controlLifetimeTransactionReader) ReadLifetimeUnlaunchedContext(context.Context) (*task.LifetimeUnlaunchedRecord, error) {
	return readControlLifetimeUnlaunchedTx(r.tx, r.lifetime)
}

var _ ControlLifetimeRecords = controlLifetimeTransactionReader{}
