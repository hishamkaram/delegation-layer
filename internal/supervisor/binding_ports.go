package supervisor

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// StoreOpener supplies the application-composed record interpreters when a
// saved endpoint requires journal reconciliation. Supervisor owns when to open
// and close the store; the opener owns only its immutable configuration.
type StoreOpener func(context.Context, string) (*taskdir.Store, error)
