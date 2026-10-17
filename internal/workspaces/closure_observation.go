package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// Authorization reads existing proof only. Recovery and fencing remain owned
// by the workspace coordinator, outside supervisor generation/queue leases.
func readOperationProof(ctx context.Context, operation *workspace.Operation) (guardian.ClosureProof, error) {
	return ReadAuxiliaryProof(ctx, operation)
}

func ReadAuxiliaryProof(ctx context.Context, operation guardian.AuxiliaryClosureRecords) (guardian.ClosureProof, error) {
	proof, err := guardian.ReadAuxiliaryClosureProof(ctx, operation)
	if errors.Is(err, guardian.ErrProofPending) {
		err = workspace.ErrPending
	}
	return proof, err
}

func ReadLifetimeProof(ctx context.Context, records guardian.ClosureRecords) (guardian.ClosureProof, error) {
	proof, err := guardian.ReadClosureProof(ctx, records)
	if errors.Is(err, guardian.ErrProofPending) {
		err = workspace.ErrPending
	}
	return proof, err
}
