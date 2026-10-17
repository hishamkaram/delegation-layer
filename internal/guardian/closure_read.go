package guardian

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ClosureRecords supplies validated lifetime records from any journal family.
type ClosureRecords interface {
	taskdir.LifetimeRecordReader
}

// AuxiliaryClosureRecords also supplies the create-once unlaunched fence.
type AuxiliaryClosureRecords interface {
	ClosureRecords
	ReadLifetimeUnlaunchedContext(context.Context) (*task.LifetimeUnlaunchedRecord, error)
}

// ReadClosureProof observes existing records without acquiring recovery authority.
func ReadClosureProof(ctx context.Context, records ClosureRecords) (ClosureProof, error) {
	evidence, err := taskdir.LoadLifetimeEvidenceContext(ctx, records)
	if err != nil {
		return ClosureProof{}, err
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil {
		return ClosureProof{}, err
	}
	if !closure.Closed() {
		return ClosureProof{}, ErrProofPending
	}
	proof := ClosureProof{closureKind: closure.Kind}
	if evidence.Termination != nil {
		data, marshalErr := task.MarshalCanonical(evidence.Termination)
		if marshalErr != nil {
			return proof, marshalErr
		}
		proof.terminationSHA256 = task.ComputeSHA256(data)
	}
	if evidence.WorkerWait != nil {
		data, marshalErr := task.MarshalCanonical(evidence.WorkerWait)
		if marshalErr != nil {
			return proof, marshalErr
		}
		proof.workerWaitSHA256 = task.ComputeSHA256(data)
	}
	return proof, nil
}

// ReadAuxiliaryClosureProof also accepts an existing unlaunched fence. It never
// creates that fence or changes the operation's lifetime records.
func ReadAuxiliaryClosureProof(ctx context.Context, operation AuxiliaryClosureRecords) (ClosureProof, error) {
	proof, err := ReadClosureProof(ctx, operation)
	if !errors.Is(err, ErrProofPending) {
		return proof, err
	}
	fence, err := operation.ReadLifetimeUnlaunchedContext(ctx)
	if task.RecordAbsent(err) {
		return proof, ErrProofPending
	}
	if err != nil {
		return proof, err
	}
	data, err := task.MarshalCanonical(fence)
	if err != nil {
		return proof, err
	}
	return ClosureProof{unlaunchedSHA256: task.ComputeSHA256(data)}, nil
}
