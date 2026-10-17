package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// AdmissionSelection contains immutable caller choices, never runtime authority.
type AdmissionSelection struct {
	Runner          string
	RunnerOwnership string
	ConfigPath      string
	SavedSupervisor *task.SupervisorRef
}

// AdmissionOptions is the executable/control configuration for supervisor setup.
type AdmissionOptions struct {
	RunnerExecutable  string
	InitialExecutable string
	Control           pueue.Options
	OpenStore         StoreOpener
}

// RunnerPreparation retains the failing setup phase without wrapping its error.
// The admission workflow owns the shared stage-error annotation.
type RunnerPreparation struct {
	Executable   string
	FailureStage string
}
