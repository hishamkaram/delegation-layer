package journal

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// operationRequestName is the canonical request record every operation
// journal opens and loads.
const operationRequestName = "request.json"

// OperationRequestPresent reports whether the existing operation directory id
// holds its request record, without opening its lock, creating anything or
// decoding the bytes. present=false with a nil error means only that this
// leaf entry is absent; every other outcome, including a missing directory, is
// an error. A present request still needs LoadOperation for validation.
func OperationRequestPresent(ctx context.Context, store *taskdir.Store, id string) (bool, error) {
	_, present, err := store.ReadWorkspaceOperationRecord(ctx, id, operationRequestName)
	if err != nil {
		return false, err
	}
	return present, nil
}
