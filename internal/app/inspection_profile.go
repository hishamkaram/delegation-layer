package app

import (
	"errors"

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

// prepareInspectionTaskCandidate compiles a detached worker's candidate with
// the factory its admission used, in the binding's launch scope deps carries.
func prepareInspectionTaskCandidate(deps Dependencies, store *taskdir.Store, req task.TaskRecord, meta *task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	deps, err := inspectionCandidateDependencies(deps, store, req, meta)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return prepareInspectionWorkerCandidate(deps, store.Root, req, meta)
}

// inspectionCandidateDependencies selects the production successor factory
// for a prospective noninteractive exact-session continuation. Its admission
// compiled the candidate through Catalog.SuccessorCandidate from the immediate
// predecessor's immutable records, so the worker reads those records through
// its already open store and repeats the catalog's compatibility check. No
// predecessor resolution, recovery, or session handoff happens here. An
// admitted task reconstructs from its own record, and injected factories and
// interactive continuations keep their current preparation.
func inspectionCandidateDependencies(deps Dependencies, store *taskdir.Store, req task.TaskRecord, meta *task.MetaRecord) (Dependencies, error) {
	normalized := deps.normalized()
	if !normalized.useCatalogExisting || meta != nil || req.PriorSession == nil || req.RequestedConfig.Interactive {
		return deps, nil
	}
	predecessor, predecessorMeta, err := loadInspectionPredecessor(store, req.PriorSession.PredecessorTaskID)
	if err != nil {
		return Dependencies{}, err
	}
	if err = normalized.Catalog.ValidateContinuation(*predecessor, *predecessorMeta, req); err != nil {
		return Dependencies{}, err
	}
	return continuationDependencies(deps, *predecessorMeta), nil
}

// loadInspectionPredecessor reads one predecessor's validated prepared
// records. The task store guards their task and root identities.
func loadInspectionPredecessor(store *taskdir.Store, id string) (request *task.TaskRecord, meta *task.MetaRecord, resultErr error) {
	td, err := store.OpenTask(id)
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	return td.PreparedRecords()
}

// useCatalogExistingFor reports whether an already admitted task reconstructs
// through the catalog's recorded contract. Every admitted task, including an
// interactive continuation, keeps its own record; ForTask alone handles a task
// that has no metadata yet.
func (d Dependencies) useCatalogExistingFor(request task.TaskRecord) bool {
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
