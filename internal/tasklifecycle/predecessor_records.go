package tasklifecycle

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
)

type PredecessorOptions struct {
	RootAdmission *taskdir.RootAdmissionLease
	Admission     admissioncore.StaticOptions
	Supervisor    supervisorcore.AdmissionOptions
}
