package infrastructure

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// EstablishmentHost supplies artifacts through fixed trusted app composition.
// It is separate from the resident EntryHost. The caller must classify outside
// placement from the original captured/native observations before invoking it,
// and retain the target pin and instance boundary. This port grants no placement
// or daemon Start authority and accepts no executable paths from a request.
type EstablishmentHost interface {
	PrepareOutsideRunner(context.Context, *taskdir.Store) (string, error)
}
