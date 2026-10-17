package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// supervisorOptionsForCurrentEnvironment snapshots the bounded supervisor
// control environment before runner reconstruction applies a task's provider
// environment to the process. This preserves Linux XDG socket/configuration
// selectors for external supervisors while keeping task credentials out of
// the supervisor context.
func OptionsForCurrentEnvironment(base pueue.Options) pueue.Options {
	if base.Environment != nil {
		base.Environment = append([]string(nil), base.Environment...)
		return base
	}
	base.Environment = pueue.DefaultEnvironment()
	return base
}
