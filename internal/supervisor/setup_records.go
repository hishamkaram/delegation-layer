package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// AdmissionSelection contains immutable caller choices, never runtime authority.
// ConfigPath is the explicit --pueue-config choice. EnvironmentConfigPath is
// the DELEGATE_PUEUE_CONFIG value from the launch scope in effect, captured by
// composition; the explicit choice takes precedence over it.
type AdmissionSelection struct {
	Runner                string
	RunnerOwnership       string
	ConfigPath            string
	EnvironmentConfigPath string
	SavedSupervisor       *task.SupervisorRef
}

// AdmissionOptions is the executable/control configuration for supervisor setup.
// Environment is the launch scope whose PATH serves the documented supervisor
// executable fallback; nil is the standalone process environment.
type AdmissionOptions struct {
	RunnerExecutable  string
	InitialExecutable string
	Environment       []string
	Control           pueue.Options
	OpenStore         StoreOpener
}

// Lookup returns the supervisor executable lookup this configuration selects.
func (options AdmissionOptions) Lookup() ExecutableLookup {
	return ExecutableLookup{InitialExecutable: options.InitialExecutable, Environment: options.Environment}
}

// RunnerPreparation retains the failing setup phase without wrapping its error.
// The admission workflow owns the shared stage-error annotation.
type RunnerPreparation struct {
	Executable   string
	FailureStage string
}
