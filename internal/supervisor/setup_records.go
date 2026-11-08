package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
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
	SavedSource           infrastructure.SavedBindingSource
	// ContinuationSource names the original predecessor metadata. A derived
	// saved binding never changes this locator or grants recovery authority.
	ContinuationSource infrastructure.SavedBindingSource
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
	Routing           OwnerRouting
}

// Lookup returns the supervisor executable lookup this configuration selects.
func (options AdmissionOptions) Lookup() installation.ExecutableLookup {
	return installation.ExecutableLookup{InitialExecutable: options.InitialExecutable, Environment: options.Environment}
}

// RunnerPreparation retains the failing setup phase without wrapping its error.
// The admission workflow owns the shared stage-error annotation.
type RunnerPreparation struct {
	Executable   string
	FailureStage string
}
