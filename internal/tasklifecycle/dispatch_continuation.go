package tasklifecycle

import (
	"errors"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func resolveDispatchProvider(request DispatchRequest, options DispatchOptions, store *taskdir.Store, cwd string, brief []byte, result DispatchResult) (DispatchRequest, DispatchResult, error) {
	if !request.Auto {
		return request, result, nil
	}
	provider, exists, err := persistedProviderForTask(store, result.TaskID)
	result.Record = DispatchRecordLookup(request.TaskID, exists, err)
	if err != nil {
		return request, dispatchFailure(result, DispatchProviderLookup, "select-provider", err), err
	}
	if !exists {
		provider, err = admissioncore.SelectAutoProvider(options.staticAdmission(), store.Root, store.RootID, result.TaskID, cwd, request.Config, brief)
	}
	request.Provider = provider
	if err != nil {
		result = dispatchFailure(result, DispatchProviderSelection, "select-provider", err)
	}
	return request, result, err
}

func prepareDispatchContinuation(store *taskdir.Store, options DispatchOptions, request *task.TaskRecord, predecessorID string) (DispatchOptions, *task.MetaRecord, error) {
	if predecessorID == "" {
		return options, nil, nil
	}
	continuationOptions, meta, err := ContinuationOptionsForTask(store, options, predecessorID)
	if err != nil {
		return options, nil, err
	}
	if err = ValidateContinuationEnvironment(meta); err != nil {
		return options, nil, err
	}
	prior, err := ResolvePredecessor(store, PredecessorOptions{Admission: continuationOptions.staticAdmission(), Supervisor: continuationOptions.Creation.Admission.Supervisor}, *request, predecessorID)
	if err != nil {
		return options, nil, err
	}
	request.PriorSession = prior
	return continuationOptions, meta, nil
}

func ContinuationOptionsForTask(store *taskdir.Store, options DispatchOptions, id string) (result DispatchOptions, predecessorMeta *task.MetaRecord, resultErr error) {
	result = options
	td, err := store.OpenTask(id)
	if err != nil {
		return options, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return options, nil, err
	}
	if meta == nil {
		return options, nil, errors.New("continuation unavailable: predecessor metadata is not recorded")
	}
	if options.ContinuationProfiles == nil {
		return options, nil, task.ErrEvidenceFault
	}
	result.Creation.Admission.Profiles = options.ContinuationProfiles(*meta, options.Creation.Admission.Profiles.Environment())
	return result, meta, nil
}

// newSuccessorCreation binds a new continuation task's creation to the
// caller's captured launch scope, which Profiles already owns: that scope
// selects the provider profile, supervisor control, and detached maintenance.
// The predecessor's record only suppresses a scope DELEGATE_PUEUE_CONFIG that
// would retarget its private supervisor; an explicit --pueue-config and the
// saved supervisor binding are kept. A dispatch without a predecessor is
// returned unchanged.
func newSuccessorCreation(options CreationOptions, selection supervisorcore.AdmissionSelection, root string, meta *task.MetaRecord) (CreationOptions, supervisorcore.AdmissionSelection) {
	if meta == nil {
		return options, selection
	}
	scope := options.Admission.Profiles.Environment()
	options.Admission.Supervisor = supervisorcore.AdmissionOptionsForScope(options.Admission.Supervisor, scope, scope)
	selection.EnvironmentConfigPath = supervisorcore.ContinuationConfigEnvironment(root, *meta, scope)
	return options, selection
}
