package tasklifecycle

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
)

type PredecessorOptions struct {
	Admission        admissioncore.StaticOptions
	Supervisor       supervisorcore.AdmissionOptions
	ApplyEnvironment EnvironmentApplicator
}
