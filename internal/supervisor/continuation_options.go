package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// ContinuationOptions supplies immutable endpoint configuration and the
// existing application-composed store opener. Supervisor owns all transitions.
type ContinuationOptions struct {
	Options           pueue.Options
	InitialExecutable string
	Environment       []string
	OpenStore         StoreOpener
}

// Lookup returns the supervisor executable lookup this configuration selects.
func (options ContinuationOptions) Lookup() ExecutableLookup {
	return ExecutableLookup{InitialExecutable: options.InitialExecutable, Environment: options.Environment}
}
