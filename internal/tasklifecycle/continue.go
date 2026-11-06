package tasklifecycle

import (
	"context"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

func Continue(request DispatchRequest, options ContinuationOptions) (result ContinuationResult) {
	return ContinueContext(context.Background(), request, options)
}

func ContinueContext(ctx context.Context, request DispatchRequest, options ContinuationOptions) (result ContinuationResult) {
	root, err := ResolveRoot(request.Root)
	if err != nil {
		return continuationFailure(result, ContinueResolveRoot, "resolve-state-root", err)
	}
	result.Root = root
	predecessor, meta, brief, err := LoadContinuationInputContext(ctx, root, request, options.Dispatch)
	if err != nil {
		return continuationFailure(result, ContinueLoadPredecessor, "load-predecessor", err)
	}
	result.RootID = predecessor.RootID
	result.Resources, err = checkContinuationResources(ctx, request, root, options.Dispatch)
	if err != nil {
		return continuationFailure(result, ContinueResources, "check-resources", err)
	}
	options.Dispatch.resourceAssessment = result.Resources
	result.Workspace, err = PrepareContinuationWorkspaceContext(ctx, root, options.Dispatch, *predecessor)
	if err != nil {
		return continuationFailure(result, ContinueWorkspace, "prepare-workspace", err)
	}
	successor, prepared := prepareContinuationSuccessor(request, options, predecessor, meta, brief, result)
	result = prepared
	if result.Failure != nil {
		return result
	}
	if options.Dispatch.ContinuationProfiles == nil {
		return continuationFailure(result, ContinueValidation, "validate-continuation", task.ErrEvidenceFault)
	}
	options.Dispatch.Creation.Admission.Profiles = options.Dispatch.ContinuationProfiles(*meta, options.Dispatch.Creation.Admission.Profiles.Environment())
	prior, source, err := loadContinuationPrior(root, predecessor.TaskID, options.Dispatch)
	if err != nil {
		return continuationFailure(result, ContinueSession, "load-continuation-session", err)
	}
	result.SessionSource = source
	successorID, err := randomid.NewTaskID()
	if err != nil {
		return continuationFailure(result, ContinueGenerateID, "generate-task-id", err)
	}
	successorRecord, err := admissioncore.BuildRequest(predecessor.RootID, successorID, successor.Provider, successor.Cwd, successor.Config, successor.BriefBytes, prior)
	if err != nil {
		return continuationFailure(result, ContinueValidation, "validate-continuation", err)
	}
	if _, err = admissioncore.ValidateContinuation(options.Dispatch.staticAdmission(), root, *predecessor, *meta, prior, successorRecord); err != nil {
		return continuationFailure(result, ContinueValidation, "validate-continuation", err)
	}
	successor.GeneratedTaskID = successorID
	return continueAdmittedContext(ctx, successor, options, predecessor, meta, result)
}

// continueAdmitted dispatches the successor in the caller's captured launch
// scope, which owns the new task's provider profile, supervisor control, and
// maintenance. The predecessor's records decide only compatibility.
func continueAdmittedContext(ctx context.Context, successor DispatchRequest, options ContinuationOptions, predecessor *task.TaskRecord, meta *task.MetaRecord, result ContinuationResult) ContinuationResult {
	options.Dispatch.Creation, successor.Selection = newSuccessorCreation(options.Dispatch.Creation, successor.Selection, result.Root, meta)
	options.Dispatch.Workspace.Runtime = admissioncore.NewWorkspaceRuntime(options.Dispatch.Creation.Admission.Supervisor)
	dispatched := DispatchContext(ctx, successor, options.Dispatch)
	result.Dispatch = &dispatched
	result.PredecessorID, result.Runner = predecessor.TaskID, successor.Selection.Runner
	if dispatched.Creation != nil && dispatched.Creation.Supervisor != nil {
		saved := *dispatched.Creation.Supervisor
		result.Supervisor = &saved
	} else if successor.Selection.SavedSupervisor != nil {
		saved := *successor.Selection.SavedSupervisor
		result.Supervisor = &saved
	} else if successor.Selection.ConfigPath != "" {
		result.Supervisor = &task.SupervisorRef{ConfigPath: successor.Selection.ConfigPath}
	}
	return result
}

func continuationFailure(result ContinuationResult, phase ContinuationPhase, stage string, err error) ContinuationResult {
	result.Phase, result.Failure = phase, admissioncore.WithStage(stage, err)
	return result
}

func prepareContinuationSuccessor(request DispatchRequest, options ContinuationOptions, predecessor *task.TaskRecord, meta *task.MetaRecord, brief []byte, result ContinuationResult) (DispatchRequest, ContinuationResult) {
	var err error
	result.Mode, err = admissioncore.ContinuationProviderMode(options.Dispatch.Catalog, predecessor.Provider)
	if err != nil {
		return DispatchRequest{}, continuationFailure(result, ContinueProvider, "", err)
	}
	if err = ValidateContinuationMetadata(result.Root, request.Selection, meta); err != nil {
		return DispatchRequest{}, continuationFailure(result, ContinueValidation, "validate-continuation", err)
	}
	successor, err := BuildContinuationRequest(request, result.Root, predecessor, meta, brief)
	if err != nil {
		return DispatchRequest{}, continuationFailure(result, ContinueRunner, "resolve-continuation-runner", err)
	}
	return successor, result
}
