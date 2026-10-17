package workspaces

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// ReadOperationProof observes existing proof only. Workspace recovery retains
// its separate lifetime-owner lease and never runs under a queue identity lease.
func ReadOperationProof(ctx context.Context, operation *workspace.Operation) (guardian.ClosureProof, error) {
	return ReadAuxiliaryProof(ctx, operation)
}
