package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// ValidateInventory checks this canonical request against the existing root
// registry winner. It reads/verifies only; LoadResource intentionally keeps its
// separate contract. Caller owns Resource, its control and the borrowed Store.
// Missing or mismatched inventory is a fault, never permission or repair.
func (r *Resource) ValidateInventory(ctx context.Context) (resultErr error) {
	if r == nil || r.store == nil || r.control == nil || ctx == nil {
		return os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateResourceRequest(r.request, r.store.RootID); err != nil {
		return classifyStoredResourceRecordError(err)
	}
	registry, err := r.store.OpenWorkspaceRegistry(ctx, false)
	if err != nil {
		return requireResourceRecord(err)
	}
	// Match the existing canonical registry reader's terminal Close contract.
	// A cleanup fault refuses the gate and names its exact unresolved resource;
	// ControlDir.Close is sticky, so this does not promise an unavailable retry.
	defer func() {
		if closeErr := registry.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close existing workspace registry %s: %w", filepath.Join(r.store.Root, "workspace-registry"), closeErr))
		}
	}()
	return registry.WithLock(ctx, func(tx *taskdir.ControlTransaction) error { return validateResourceInventoryTx(tx, r.request) })
}

// validateResourceInventoryTx reuses the inventory owner's canonical pairing
// and request comparison. It reads/verifies existing records, never repairs them.
func validateResourceInventoryTx(tx *taskdir.ControlTransaction, request contract.ResourceRequest) error {
	winner, err := readCanonicalWorkspaceInventoryTx(tx, request.WorkspaceID, request.RootID)
	if err != nil {
		return err
	}
	if !inventoryMatchesRequest(winner, request) {
		return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	return nil
}
