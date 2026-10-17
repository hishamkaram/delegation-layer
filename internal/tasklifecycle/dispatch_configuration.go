package tasklifecycle

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (options DispatchOptions) staticAdmission() admissioncore.StaticOptions {
	return admissioncore.StaticOptions{Catalog: options.Catalog, Profiles: options.Creation.Admission.Profiles}
}

func (options DispatchOptions) openStore(root string, create bool) (*taskdir.Store, error) {
	if options.OpenStore == nil {
		return nil, task.ErrEvidenceFault
	}
	return options.OpenStore(root, create)
}
