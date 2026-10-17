package app

import (
	"github.com/hishamkaram/delegation-layer/internal/execution"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func prepareMatchedProfileForRunner(deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord, store *taskdir.Store, currentExecutable, runnerOwnership string) (profile PreparedProfile, resultErr error) {
	return admissionProfiles(deps).ForRunner(root, req, meta, store, currentExecutable, runnerOwnership)
}

func prepareInspectionWorkerCandidate(deps Dependencies, root string, req task.TaskRecord, meta *task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	return admissionProfiles(deps).ForWorker(root, req, meta)
}

func (d Dependencies) useCatalogExistingFor(request task.TaskRecord) bool {
	if request.RequestedConfig.Interactive && request.PriorSession != nil {
		return false
	}
	if d.useCatalogExisting {
		return true
	}
	if d.PrepareProfile != nil {
		return false
	}
	registration, err := d.Catalog.Lookup(request.Provider)
	return err == nil && registration.PrepareExisting != nil
}

func freshPreflightProfile(deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord, scope execution.PreflightScope) (execution.Plan, error) {
	return admissionProfiles(deps).Fresh(root, req, meta, scope)
}
