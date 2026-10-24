package supervisor

import (
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// OptionsForEnvironment snapshots the bounded supervisor control environment
// from one captured launch scope instead of the process environment. An
// explicit control environment, including an empty one, is copied unchanged.
func OptionsForEnvironment(base pueue.Options, environment []string) pueue.Options {
	if base.Environment != nil {
		base.Environment = slices.Clone(base.Environment)
		return base
	}
	base.Environment = pueue.DefaultEnvironmentFrom(environment)
	return base
}

// OptionsWithMaintenanceScope binds the bounded launch environment of a
// detached maintenance owner to the scope that schedules it. It leaves the
// client's control environment unchanged.
func OptionsWithMaintenanceScope(base pueue.Options, environment []string) pueue.Options {
	base.PrivateMaintenanceEnvironment = pueue.DefaultEnvironmentFrom(environment)
	return base
}

// AdmissionOptionsForScope binds supervisor setup to one selected launch
// scope. control is the environment a default control configuration reads at
// this point; scope is the selected environment, which also owns detached
// maintenance launches and supervisor executable lookup while the scoped work
// runs.
func AdmissionOptionsForScope(runtime AdmissionOptions, control, scope []string) AdmissionOptions {
	runtime.Control = OptionsWithMaintenanceScope(OptionsForEnvironment(runtime.Control, control), scope)
	runtime.Environment = slices.Clone(scope)
	return runtime
}
