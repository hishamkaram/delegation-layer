package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// ContinuationOptions supplies immutable endpoint configuration and the
// existing application-composed store opener. Supervisor owns all transitions.
type ContinuationOptions struct {
	Options           pueue.Options
	InitialExecutable string
	OpenStore         StoreOpener
}
