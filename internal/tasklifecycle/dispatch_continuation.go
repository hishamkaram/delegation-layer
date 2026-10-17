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
	prior, err := ResolvePredecessor(store, PredecessorOptions{Admission: continuationOptions.staticAdmission(), Supervisor: continuationOptions.Creation.Admission.Supervisor, ApplyEnvironment: options.ApplyEnvironment}, *request, predecessorID)
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
	result.Creation.Admission.Profiles = options.ContinuationProfiles(*meta)
	return result, meta, nil
}

func dispatchContinuationEnvironment(apply EnvironmentApplicator, root string, meta *task.MetaRecord, interactive bool) (func() error, error) {
	if meta == nil || !interactive {
		return nil, nil
	}
	return supervisorcore.ApplyContinuationEnvironment(root, meta, apply)
}
