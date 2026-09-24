package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func dispatch(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("dispatch")
	response.TaskRecord = initialDispatchTaskRecord(a.TaskID)
	response.TaskID = a.TaskID
	root, cwd, brief, err := dispatchInputs(a)
	if err != nil {
		return failed(response, withDispatchStage("validate-request", err), classifyCode(err, 2))
	}
	store, err := openStore(root, deps.storeDependencies(), true)
	if err != nil {
		return failed(response, withDispatchStage("open-state", err), classifyCode(err, 1))
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	id := a.TaskID
	if id == "" {
		id, err = task.NewTaskID()
		if err != nil {
			return failed(response, withDispatchStage("generate-task-id", err), 1)
		}
	}
	response.TaskID = id
	if a.Auto {
		persistedProvider, exists, providerErr := persistedProviderForTask(store, id)
		response.TaskRecord = dispatchTaskRecordLookup(a.TaskID, exists, providerErr)
		if providerErr != nil {
			return failed(response, withDispatchStage("select-provider", providerErr), classifyCode(providerErr, 1))
		}
		if exists {
			a.Provider = persistedProvider
		} else {
			a.Provider, err = selectAutoProvider(deps, store.Root, store.RootID, id, cwd, a.Config, brief)
			if err != nil {
				return failed(response, withDispatchStage("select-provider", err), classifyCode(err, 2))
			}
		}
	}
	prior := (*task.PriorSession)(nil)
	base, err := buildRequest(store.RootID, id, a.Provider, cwd, a.Config, brief, nil)
	if err != nil {
		return failed(response, withDispatchStage("validate-request", err), classifyCode(err, 2))
	}
	if a.ResumeTask != "" {
		prior, err = resolvePredecessor(store, base, a.ResumeTask, deps.SupervisorOptions)
		if err != nil {
			return failed(response, withDispatchStage("resolve-predecessor", err), classifyCode(err, 1))
		}
		base.PriorSession = prior
	}
	req := base
	response.TaskID = req.TaskID

	td, openErr := store.OpenTask(id)
	if openErr == nil {
		response.TaskRecord = "created"
		return dispatchExisting(a, deps, store, td, req, response)
	}
	if !errors.Is(openErr, os.ErrNotExist) {
		response.TaskRecord = "unknown"
		return failed(response, withDispatchStage("open-task-record", openErr), classifyCode(openErr, 1))
	}
	response.TaskRecord = dispatchTaskRecordLookup(a.TaskID, false, nil)
	return dispatchNew(a, deps, store, req, brief, response)
}

func initialDispatchTaskRecord(taskID string) string {
	if taskID == "" {
		return "not_created"
	}
	return "unknown"
}

func dispatchTaskRecordLookup(requestedTaskID string, exists bool, lookupErr error) string {
	if exists {
		return "created"
	}
	if lookupErr != nil || requestedTaskID != "" {
		return "unknown"
	}
	return "not_created"
}

func persistedProviderForTask(store *taskdir.Store, taskID string) (provider string, exists bool, resultErr error) {
	td, err := store.OpenTask(taskID)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	req, _, err := td.PreparedRecords()
	if err != nil {
		return "", true, fmt.Errorf("existing task cannot be reused for automatic provider selection: %w", err)
	}
	if req == nil || req.Provider == "" {
		return "", true, errors.New("existing task has no persisted provider")
	}
	return req.Provider, true, nil
}

func dispatchInputs(a Arguments) (root, cwd string, brief []byte, err error) {
	root, err = resolveRoot(a.Root)
	if err != nil {
		return "", "", nil, err
	}
	_, cwd, err = validateRootAndCwd(root, a.Cwd)
	if err != nil {
		return "", "", nil, err
	}
	if a.BriefBytes != nil {
		if len(a.BriefBytes) == 0 {
			return "", "", nil, errors.New("brief must be nonempty")
		}
		brief = append([]byte(nil), a.BriefBytes...)
		if len(brief) > task.MaxBriefSize {
			return "", "", nil, config.ErrBriefTooLarge
		}
		return root, cwd, brief, nil
	}
	briefPath, pathErr := filepath.Abs(a.Brief)
	if pathErr != nil {
		return "", "", nil, pathErr
	}
	brief, err = readBrief(briefPath)
	return root, cwd, brief, err
}

func continueTask(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("continue")
	response.TaskRecord = "not_created"
	root, err := resolveRoot(a.Root)
	if err != nil {
		return failed(response, withDispatchStage("resolve-state-root", err), 1)
	}
	predecessor, predecessorMeta, brief, err := loadContinuationInput(root, a, deps)
	if err != nil {
		return failed(response, withDispatchStage("load-predecessor", err), 1)
	}
	registration, lookupErr := deps.normalized().Catalog.Lookup(predecessor.Provider)
	if lookupErr != nil {
		return failed(response, withDispatchStage("resolve-continuation-provider", lookupErr), 2)
	}
	if registration.Description.Continuation != commonprovider.ContinuationNative {
		return failed(response, withDispatchStage("check-continuation-capability", fmt.Errorf("native continuation unsupported for %s", predecessor.Provider)), 2)
	}
	if err = validateContinuationMetadata(root, a, predecessorMeta); err != nil {
		return failed(response, withDispatchStage("validate-continuation", err), 2)
	}
	continuationArgs := buildContinuationArguments(a, root, predecessor, predecessorMeta, brief)
	supervisorOptions := supervisorOptionsForCurrentEnvironment(deps.SupervisorOptions)
	restoreEnvironment, restoreErr := applyContinuationEnvironment(root, predecessorMeta)
	if restoreErr != nil {
		return failed(response, withDispatchStage("restore-continuation-environment", restoreErr), 1)
	}
	defer restoreRunnerEnvironment(&result, restoreEnvironment)
	deps.SupervisorOptions = supervisorOptions
	result = dispatch(continuationArgs, deps)
	var supervisor *task.SupervisorRef
	if continuationArgs.savedSupervisor != nil {
		saved := *continuationArgs.savedSupervisor
		supervisor = &saved
	} else if continuationArgs.PueueConfig != "" {
		supervisor = &task.SupervisorRef{ConfigPath: continuationArgs.PueueConfig}
	}
	result = decorateContinuationResult(result, root, predecessor.TaskID, registration.Description.Continuation, continuationArgs.Runner, supervisor)
	return result
}

func loadContinuationInput(root string, a Arguments, deps Dependencies) (req *task.TaskRecord, meta *task.MetaRecord, brief []byte, resultErr error) {
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		return nil, nil, nil, err
	}
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return nil, nil, nil, errors.Join(err, store.Close())
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close(), store.Close()) }()
	req, meta, err = td.PreparedRecords()
	if err != nil {
		return nil, nil, nil, err
	}
	brief, err = td.ReadBrief()
	if err == nil && a.Brief != "" {
		brief, err = readBrief(a.Brief)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	return req, meta, brief, nil
}

func buildContinuationArguments(a Arguments, root string, predecessor *task.TaskRecord, meta *task.MetaRecord, brief []byte) Arguments {
	requested := predecessor.RequestedConfig
	if a.Config.Budget != "" {
		requested.Budget = a.Config.Budget
	}
	if a.Config.Model != "" {
		requested.Model = a.Config.Model
	}
	if a.Config.Effort != "" {
		requested.Effort = a.Config.Effort
	}
	if a.Config.Budget != "" && requested.NativeTimeout != "" {
		if budget, budgetErr := config.ParseBudget(requested.Budget); budgetErr == nil {
			if nativeTimeout, timeoutErr := time.ParseDuration(requested.NativeTimeout); timeoutErr == nil && nativeTimeout > budget {
				requested.NativeTimeout = budget.String()
			}
		}
	}
	continuationArgs := a
	continuationArgs.Command = "dispatch"
	continuationArgs.Root = root
	continuationArgs.Auto = false
	continuationArgs.TaskID = ""
	continuationArgs.ResumeTask = predecessor.TaskID
	continuationArgs.Provider = predecessor.Provider
	continuationArgs.Brief = ""
	continuationArgs.BriefBytes = brief
	continuationArgs.Cwd = predecessor.CanonicalCwd
	continuationArgs.Config = requested
	if meta != nil {
		saved := meta.SupervisorConfig
		continuationArgs.savedSupervisor = &saved
	}
	if continuationArgs.Runner == "" && meta != nil {
		continuationArgs.Runner = meta.RunnerExecutable
	}
	if continuationArgs.PueueConfig == "" && meta != nil && !pueue.IsPrivateConfig(root, meta.SupervisorConfig.ConfigPath) {
		continuationArgs.PueueConfig = meta.SupervisorConfig.ConfigPath
	}
	return continuationArgs
}

func validateContinuationMetadata(root string, a Arguments, meta *task.MetaRecord) error {
	if meta == nil || len(meta.Environment) == 0 {
		return errors.New("continuation unavailable: predecessor launch environment is not recorded")
	}
	if meta.RunnerExecutable == "" && a.Runner == "" {
		return errors.New("continuation unavailable: predecessor runner executable is not recorded")
	}
	if meta.RunnerExecutable != "" && a.Runner != "" && a.Runner != meta.RunnerExecutable {
		return fmt.Errorf("%w: continuation runner %q does not match predecessor runner %q", task.ErrIdentityMismatch, a.Runner, meta.RunnerExecutable)
	}
	if meta.SupervisorConfig.ConfigPath == "" {
		return errors.New("continuation unavailable: predecessor supervisor binding is not recorded")
	}
	private := pueue.IsPrivateConfig(root, meta.SupervisorConfig.ConfigPath)
	if a.PueueConfig != "" && (private || a.PueueConfig != meta.SupervisorConfig.ConfigPath) {
		return fmt.Errorf("%w: continuation must use the predecessor supervisor binding", task.ErrRequestConflict)
	}
	return nil
}

func applyContinuationEnvironment(root string, meta *task.MetaRecord) (func() error, error) {
	restore, err := applySavedEnvironment(meta.Environment)
	if err != nil {
		return nil, err
	}
	if !pueue.IsPrivateConfig(root, meta.SupervisorConfig.ConfigPath) {
		return restore, nil
	}
	if err = os.Unsetenv("DELEGATE_PUEUE_CONFIG"); err != nil {
		return nil, errors.Join(err, restore())
	}
	return restore, nil
}

func decorateContinuationResult(result commandResult, root, predecessorID string, mode commonprovider.ContinuationMode, runner string, supervisor *task.SupervisorRef) commandResult {
	result.response.Command = "continue"
	result.response.ParentTaskID = predecessorID
	result.response.Continuation = &ContinuationResponse{Resumable: result.err == nil, Mode: string(mode), PredecessorID: predecessorID}
	if result.err == nil {
		result.response.Continuation.ContinueCommand = continuationCommand(root, result.response.TaskID, runner, supervisor)
	}
	return result
}

func dispatchExisting(a Arguments, deps Dependencies, store *taskdir.Store, td *taskdir.TaskDir, req task.TaskRecord, response Response) (result commandResult) {
	defer func() {
		if result.err != nil {
			result.err = withDispatchStage("existing-task", result.err)
			result.response.setError(result.err)
		}
	}()
	defer func() { mergeCommandClose(&result, td.Close) }()
	oldReq, oldMeta, err := td.PreparedRecords()
	if err != nil {
		return dispatchNew(a, deps, store, req, nil, response)
	}

	if !sameRequest(&req, oldReq) {
		return failed(response, task.ErrRequestConflict, 2)
	}
	if a.PueueConfig != "" && a.PueueConfig != oldMeta.SupervisorConfig.ConfigPath {
		return failed(response, task.ErrRequestConflict, 2)
	}
	response.RootID, response.TaskID = oldReq.RootID, oldReq.TaskID
	inspection, inspectErr := td.Inspect()
	fillInspection(&response, inspection)
	if inspectErr != nil {
		return failed(response, inspectErr, classifyCode(inspectErr, 1))
	}
	if isTerminal(inspection) {
		if releaseErr := releaseContinuationSession(td, oldReq, inspection.Outcome); releaseErr != nil {
			return failed(response, releaseErr, 1)
		}
		return commandResult{response: response, code: 0}
	}
	submit, submitErr := td.ReadSubmission()
	if submitErr == nil {
		return reconcileExisting(store.Root, td, oldReq, oldMeta, submit, deps.SupervisorOptions, response)
	}
	if !errors.Is(submitErr, os.ErrNotExist) {
		return failed(response, submitErr, classifyCode(submitErr, 1))
	}
	if inspection.StartExists {
		return failed(response, task.ErrAlreadyStarted, 1)
	}
	supervisorOptions := supervisorOptionsForCurrentEnvironment(deps.SupervisorOptions)
	restoreEnvironment, restoreErr := applySavedEnvironment(oldMeta.Environment)
	if restoreErr != nil {
		return failed(response, restoreErr, 1)
	}
	defer restoreRunnerEnvironment(&result, restoreEnvironment)
	profile, profileErr := prepareMatchedProfile(deps, store.Root, *oldReq, *oldMeta)
	if profileErr != nil {
		return failed(response, profileErr, classifyCode(profileErr, 2))
	}
	return markDispatchSubmissionFailure(submitPreparedWithOptions(a, deps, td, oldReq, oldMeta, oldMeta.SupervisorConfig,
		supervisorOptionsForProfile(supervisorOptions, profile), store.Root, response))
}

func dispatchNew(a Arguments, deps Dependencies, store *taskdir.Store, req task.TaskRecord, brief []byte, response Response) commandResult {
	prepared, err := prepareAdmission(a, deps, store, req)
	if err != nil {
		err = withDispatchStage("prepare-admission", annotateMissingDispatchStage("preparing inspection admission", err))
		return failed(response, err, classifyCode(err, 2))
	}
	response.Capability = &prepared.Capability
	profile, supervisor := prepared.Profile, prepared.Supervisor
	if err = profile.Validate(req); err != nil {
		return failed(response, withDispatchStage("validate-profile", err), classifyCode(err, 2))
	}
	runner, err := resolveRunner(a.Runner, deps)
	if err != nil {
		return failed(response, withDispatchStage("resolve-runner", err), classifyCode(err, 2))
	}
	meta := newMeta(req, profile, supervisor.Binding(), runner, deps.PublisherVersion)
	if brief == nil {
		brief, err = readBriefFileForRequest(a.Brief, req)
		if err != nil {
			err = annotateMissingDispatchStage("reading brief", err)
			return failed(response, withDispatchStage("read-brief", err), classifyCode(err, 1))
		}
	}
	if !prepared.InspectionDeadline.IsZero() && !time.Now().Before(prepared.InspectionDeadline) {
		return failed(response, withDispatchStage("admission-deadline", inspection.ErrAdmissionExpired), 1)
	}
	td, err := store.CreateTask(req.TaskID, &req, brief, &meta)
	if err != nil {
		response.TaskRecord = "unknown"
		err = annotateMissingDispatchStage("creating ordinary task", err)
		return failed(response, withDispatchStage("create-task-record", err), classifyCode(err, 1))
	}
	response.TaskRecord = "created"
	result := submitPreparedWithProfile(a, deps, td, &req, &meta, supervisor, profile, response)
	if result.err != nil {
		result.err = annotateMissingDispatchStage("submitting ordinary task", result.err)
	}
	return markDispatchSubmissionFailure(result)
}

func markDispatchSubmissionFailure(result commandResult) commandResult {
	if result.err != nil {
		stage := "prepare-task-submission"
		switch {
		case errors.Is(result.err, pueue.ErrSubmissionUncertain):
			stage = "submit-task"
		case result.response.Admission == task.AdmissionAdmitted.String():
			stage = "record-submission-evidence"
		}
		result.err = withDispatchStage(stage, result.err)
		result.response.setError(result.err)
	}
	return result
}

// annotateMissingDispatchStage adds only a fixed operation label to an
// otherwise opaque missing-file error. This keeps errors.Is(os.ErrNotExist)
// behavior intact while making the failing admission boundary diagnosable;
// no path, record bytes, or native diagnostic is included.
func annotateMissingDispatchStage(stage string, err error) error {
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fmt.Errorf("dispatch %s: %w", stage, err)
}

func readBriefFileForRequest(path string, req task.TaskRecord) ([]byte, error) {
	briefPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	brief, err := readBrief(briefPath)
	if err != nil {
		return nil, err
	}
	if int64(len(brief)) != req.BriefLength || task.ComputeSHA256(brief) != req.BriefSHA256 {
		return nil, errors.New("brief changed after request preparation")
	}
	return brief, nil
}

func submitPrepared(a Arguments, deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisor task.SupervisorRef, response Response) (result commandResult) {
	return submitPreparedWithOptions(a, deps, td, req, meta, supervisor, deps.SupervisorOptions, "", response)
}

func submitPreparedWithOptions(a Arguments, deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisor task.SupervisorRef, supervisorOptions pueue.Options, recoveryRoot string, response Response) (result commandResult) {
	if meta != nil {
		supervisorOptions = supervisorOptionsForMeta(supervisorOptions, *meta)
	}
	client, err := newSupervisorClient(context.Background(), recoveryRoot, supervisor, supervisorOptions, recoveryRoot != "")
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	runner, err := resolveTaskRunner(a.Runner, deps, meta)
	if err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	if err = validateSubmissionRunner(deps, td, req, meta, runner); err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	if req.PriorSession != nil {
		if err = td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
			return failed(response, err, classifyCode(err, 1))
		}
	}
	permit, err := td.PrepareSubmission(supervisor)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	defer func() { mergeCommandClose(&result, permit.Release) }()
	return submitWithClient(td, req, meta, client, runner, permit, response)
}

func submitPreparedWithProfile(a Arguments, deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, client *pueue.Client, profile PreparedProfile, response Response) (result commandResult) {
	defer func() { mergeCommandClose(&result, td.Close) }()
	if err := profile.Validate(*req); err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	runner, err := resolveTaskRunner(a.Runner, deps, meta)
	if err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	if err = validateSubmissionRunner(deps, td, req, meta, runner); err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	if req.PriorSession != nil {
		if err = td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
			return failed(response, err, classifyCode(err, 1))
		}
	}
	permit, err := td.PrepareSubmission(client.Binding())
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	defer func() { mergeCommandClose(&result, permit.Release) }()
	return submitWithClient(td, req, meta, client, runner, permit, response)
}

// validateSubmissionRunner binds every ordinary retry to the worker executable
// recorded in immutable task metadata. The inspection journal is optional for
// ordinary profiles; when present, its immutable binding adds a launch-time
// fingerprint check.
func validateSubmissionRunner(deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner string) (resultErr error) {
	if td == nil || req == nil {
		return task.ErrEvidenceFault
	}
	if meta != nil && meta.RunnerExecutable != "" && runner != meta.RunnerExecutable {
		return task.ErrIdentityMismatch
	}
	root := filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))
	store, err := openStore(root, deps.normalized().storeDependencies(), false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	inspectionDir := filepath.Join(root, "inspections", req.TaskID)
	if _, statErr := os.Lstat(inspectionDir); errors.Is(statErr, os.ErrNotExist) {
		return nil
	} else if statErr != nil {
		return statErr
	}
	operation, err := inspection.LoadOperation(store, req.TaskID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	binding := operation.Request().Binding
	if runner != binding.WorkerExecutable {
		return task.ErrIdentityMismatch
	}
	digest, err := commonprovider.FingerprintExecutable(runner)
	if err != nil || digest != binding.WorkerSHA256 {
		return task.ErrEvidenceFault
	}
	return nil
}

func submitWithClient(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, client *pueue.Client, runner string, permit *taskdir.SubmissionPermit, response Response) commandResult {
	observation, err := client.Submit(context.Background(), permit, pueue.Launch{RunnerExecutable: runner, RootPath: filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))})
	response.Supervisor = supervisorResponse(observation)
	response.Liveness = mapPueueState(observation.State)
	response.Pending = pendingResponse(observation)
	if observation.Matched {
		response.Admission = task.AdmissionAdmitted.String()
		if receipt, receiptErr := observation.Receipt(); receiptErr != nil {
			err = errors.Join(err, receiptErr)
		} else {
			if recordErr := td.RecordSupervisorReceipt(*receipt); recordErr != nil {
				err = errors.Join(err, recordErr)
			}
		}
	} else {
		response.Admission = task.AdmissionUnknown.String()
	}
	if req != nil && meta != nil {
		inspection, inspectErr := td.Inspect()
		fillInspection(&response, inspection)
		err = errors.Join(err, inspectErr)
	}
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	return commandResult{response: response, code: 0}
}

func reconcileExisting(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, submit *task.SubmitRecord, supervisorOptions pueue.Options, response Response) commandResult {
	supervisorOptions = supervisorOptionsForMeta(supervisorOptions, *meta)
	client, err := newSupervisorClient(context.Background(), root, submit.Supervisor, supervisorOptions, true)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	_, _, spec, metaHash, hashErr := requestHashes(td)
	if hashErr != nil {
		return failed(response, hashErr, 1)
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return failed(response, receiptErr, 1)
	}
	observation, reconcileErr := client.Reconcile(context.Background(), taskIdentity(req, meta, spec, metaHash, receipt))
	response.Supervisor = supervisorResponse(observation)
	response.Liveness = mapPueueState(observation.State)
	response.Pending = pendingResponse(observation)
	if observation.Matched {
		response.Admission = task.AdmissionAdmitted.String()
		if positive, receiptErr := observation.Receipt(); receiptErr != nil {
			reconcileErr = errors.Join(reconcileErr, receiptErr)
		} else {
			recordErr := td.RecordSupervisorReceipt(*positive)
			reconcileErr = errors.Join(reconcileErr, recordErr)
		}
	} else {
		response.Admission = task.AdmissionUnknown.String()
	}
	if reconcileErr != nil {
		return failed(response, reconcileErr, classifyCode(reconcileErr, 1))
	}
	return commandResult{response: response, code: 0}
}

func reconcileStatus(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, supervisorOptions pueue.Options, response *Response) (pueue.Observation, error) {
	return reconcileStatusContext(context.Background(), root, td, req, meta, spec, metaHash, supervisorOptions, response)
}

func reconcileStatusContext(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, supervisorOptions pueue.Options, response *Response) (pueue.Observation, error) {
	if ctx == nil {
		return pueue.Observation{}, context.Canceled
	}
	supervisorOptions = supervisorOptionsForMeta(supervisorOptions, *meta)
	submit, err := td.ReadSubmission()
	if err != nil {
		return pueue.Observation{}, err
	}
	client, err := newSupervisorClient(ctx, root, submit.Supervisor, supervisorOptions, true)
	if err != nil {
		return pueue.Observation{}, err
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return pueue.Observation{}, receiptErr
	}
	observation, reconcileErr := client.Reconcile(ctx, taskIdentity(req, meta, spec, metaHash, receipt))
	response.Supervisor = supervisorResponse(observation)
	response.Liveness = mapPueueState(observation.State)
	response.Pending = pendingResponse(observation)
	if observation.Matched {
		response.Admission = task.AdmissionAdmitted.String()
		if positive, positiveErr := observation.Receipt(); positiveErr != nil {
			reconcileErr = errors.Join(reconcileErr, positiveErr)
		} else {
			reconcileErr = errors.Join(reconcileErr, td.RecordSupervisorReceiptContext(ctx, *positive))
		}
	} else {
		response.Admission = task.AdmissionUnknown.String()
	}
	return observation, reconcileErr
}

func updateStatusStops(td *taskdir.TaskDir, response *Response, observation pueue.Observation) error {
	return updateStatusStopsContext(context.Background(), td, response, observation)
}

func updateStatusStopsContext(ctx context.Context, td *taskdir.TaskDir, response *Response, observation pueue.Observation) error {
	if ctx == nil {
		return context.Canceled
	}
	records, err := td.ReadStopRecords()
	if err != nil {
		return err
	}
	if observation.State != pueue.StateEnded || !observation.Matched {
		response.Stops = stopResponses(records)
		return nil
	}
	var recordErr error
	for _, record := range records {
		if record.Request == nil || record.Request.NumericTaskID == nil || *record.Request.NumericTaskID != observation.NumericTaskID {
			continue
		}
		recordErr = errors.Join(recordErr, td.RecordStopObservationContext(ctx, record.Request.RequestID, task.StopObservationFacts{NumericTaskID: observation.NumericTaskID, State: "ended", Terminated: true}))
	}
	refreshed, refreshErr := td.ReadStopRecords()
	if refreshErr == nil {
		response.Stops = stopResponses(refreshed)
	}
	return errors.Join(recordErr, refreshErr)
}

func status(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("status")
	root, err := resolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	inspection, err := td.Inspect()
	fillInspection(&response, inspection)
	if err != nil {
		return failed(response, err, 1)
	}
	if isTerminal(inspection) {
		req, meta, _, _, reqErr := requestHashes(td)
		if reqErr != nil {
			return failed(response, reqErr, 1)
		}
		if records, recordsErr := readTimeoutRecords(td); recordsErr == nil {
			// The outcome is authoritative. Timeout metadata only decorates a
			// terminal response, so malformed or stale optional records must not
			// hide the committed or rejected result.
			applyTimeoutContinuationBestEffortWithRecords(&response, root, td, req, meta, records, deps.normalized().Catalog)
		}
		return commandResult{response: response, code: 0}
	}
	req, meta, spec, metaHash, err := requestHashes(td)
	if err != nil {
		return failed(response, err, 1)
	}
	_, err = td.ReadSubmission()
	if errors.Is(err, os.ErrNotExist) {
		response.Admission = task.AdmissionNotAdmitted.String()
		return commandResult{response: response, code: 0}
	}
	if err != nil {
		return failed(response, err, 1)
	}
	observation, reconcileErr := reconcileStatus(root, td, req, meta, spec, metaHash, deps.SupervisorOptions, &response)
	reconcileErr = errors.Join(reconcileErr, updateStatusStops(td, &response, observation))
	if records, recordsErr := readTimeoutRecords(td); recordsErr == nil {
		reconcileErr = errors.Join(reconcileErr, applyTimeoutContinuationWithRecords(&response, root, td, req, meta, records, deps.normalized().Catalog))
	} else {
		reconcileErr = errors.Join(reconcileErr, recordsErr)
	}
	if reconcileErr != nil {
		if isPureSupervisorUncertaintyError(reconcileErr) {
			return commandResult{response: response, code: 0, err: reconcileErr}
		}
		return failed(response, reconcileErr, 1)
	}
	return commandResult{response: response, code: 0}
}

func collectionResult(response Response, outcome *task.OutcomeRecord, collectErr error) commandResult {
	response.Outcome = outcome
	response.Payload = &outcome.Payload
	response.EvidenceSHA256 = outcome.EvidenceSHA256
	if outcome.Verdict == task.VerdictCommitted {
		response.Publication = task.PublicationCommitted.String()
		if collectErr != nil {
			return failed(response, collectErr, 1)
		}
		return commandResult{response: response, code: 0}
	}
	response.Publication = task.PublicationRejected.String()
	if collectErr != nil {
		return failed(response, collectErr, 1)
	}
	return commandResult{response: response, code: 4}
}

func collectionInspectionResult(response Response, inspection *taskdir.TaskInspection, collectErr, inspectErr, descriptorErr error) (commandResult, bool) {
	if inspection == nil || !isTerminal(inspection) || inspection.Outcome == nil {
		return commandResult{}, false
	}
	if isPureCollectionPendingError(collectErr) {
		collectErr = nil
	}
	return collectionResult(response, inspection.Outcome, errors.Join(collectErr, inspectErr, descriptorErr)), true
}

func releaseCollectedContinuation(result commandResult, td *taskdir.TaskDir, req *task.TaskRecord, outcome *task.OutcomeRecord) commandResult {
	if releaseErr := releaseContinuationSession(td, req, outcome); releaseErr != nil {
		result.err = errors.Join(result.err, releaseErr)
		result.code = 1
		result.response.setError(result.err)
	}
	return result
}

func decorateCollectedTerminal(result commandResult, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) commandResult {
	// Timeout records are optional metadata. A committed or rejected outcome
	// remains authoritative when the metadata is absent or malformed.
	applyTimeoutContinuationBestEffort(&result.response, root, td, req, meta, catalog)
	return result
}

func applyTimeoutContinuationBestEffort(response *Response, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) {
	if err := applyTimeoutContinuationFromTask(response, root, td, req, meta, catalog); err != nil {
		// A committed or rejected outcome remains authoritative when optional
		// timeout metadata is missing or malformed.
		return
	}
}

func applyTimeoutContinuationBestEffortWithRecords(response *Response, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog) {
	if err := applyTimeoutContinuationWithRecords(response, root, td, req, meta, records, catalog); err != nil {
		// A committed or rejected outcome remains authoritative when optional
		// timeout metadata is missing or malformed.
		return
	}
}

func collectAttempt(td *taskdir.TaskDir, predicate task.PredicateRef) (*task.OutcomeRecord, error) {
	outcome, cleanupErr, collectErr := td.Collect(predicate)
	return outcome, errors.Join(cleanupErr, collectErr)
}

func reconcileCollectionTimeout(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, deps storeDependencies, response *Response) error {
	if ctx == nil {
		return context.Canceled
	}
	records, err := td.ReadStopRecords()
	if err != nil || !budgetStopNeedsObservation(records) || !deps.observeSupervisor {
		return err
	}
	_, _, spec, metaHash, err := requestHashes(td)
	if err != nil {
		return err
	}
	observation, reconcileErr := reconcileStatusContext(ctx, root, td, req, meta, spec, metaHash, deps.supervisorOptions, response)
	if reconcileErr != nil {
		if isPureSupervisorUncertaintyError(reconcileErr) {
			return reapSupervisorObservation(ctx, reconcileErr)
		}
		if errors.Is(reconcileErr, context.Canceled) || errors.Is(reconcileErr, context.DeadlineExceeded) {
			return nil
		}
		return reconcileErr
	}
	if err := updateStatusStopsContext(ctx, td, response, observation); err != nil {
		// A bounded watch may expire while the durable stop annotation is being
		// written. The next collection pass can retry that annotation; the
		// current pass must remain a pending observation rather than an error.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return err
	}
	return nil
}

func reapSupervisorObservation(ctx context.Context, err error) error {
	var inFlight *pueue.InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending == nil {
		return nil
	}
	if ctx == nil {
		return context.Canceled
	}
	select {
	case <-inFlight.Pending.Done():
		return nil
	case <-ctx.Done():
		// The pending supervisor command retains its own process ownership. The
		// collection caller has reached its finite watch boundary, so leave the
		// observation unresolved and let the caller return code 3.
		return nil
	}
}

func budgetStopNeedsObservation(records []taskdir.StopRecord) bool {
	for _, record := range records {
		if record.Request != nil && record.Request.Cause == "budget" && (record.Observation == nil || !record.Observation.Terminated) {
			return true
		}
	}
	return false
}

func waitForCollection(deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	if remaining > 20*time.Millisecond {
		remaining = 20 * time.Millisecond
	}
	timer := time.NewTimer(remaining)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	<-timer.C
	return time.Now().Before(deadline)
}

func applyTimeoutContinuationFromTask(response *Response, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) error {
	records, err := readTimeoutRecords(td)
	if err != nil {
		return err
	}
	return applyTimeoutContinuationWithRecords(response, root, td, req, meta, records, catalog)
}

func applyTimeoutContinuationOnce(annotated *bool, response *Response, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) error {
	if *annotated {
		return nil
	}
	err := applyTimeoutContinuationFromTask(response, root, td, req, meta, catalog)
	if err == nil && timeoutContinuationIsFinal(response.Continuation) {
		*annotated = true
	}
	return err
}

func timeoutContinuationIsFinal(continuation *ContinuationResponse) bool {
	if continuation == nil {
		return false
	}
	return continuation.Resumable || continuation.Mode == string(commonprovider.ContinuationUnsupported) || continuation.Mode == string(commonprovider.ContinuationCheckpoint)
}

func applyTimeoutContinuationWithRecords(response *Response, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog) error {
	runnerReleased, err := td.RunnerLeaseReleased()
	if err != nil {
		return err
	}
	return applyTimeoutContinuationWithRecordsAndRunner(response, root, td, req, meta, records, catalog, runnerReleased)
}

func applyTimeoutContinuationWithRecordsAndRunner(response *Response, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, runnerReleased bool) error {
	if !hasTerminatedBudgetStop(records) {
		return nil
	}
	session, err := readProviderSession(td)
	if err != nil {
		return err
	}
	runner := ""
	if meta != nil {
		runner = meta.RunnerExecutable
	}
	launchStateRecorded := meta != nil && len(meta.Environment) > 0
	applyTimeoutContinuationWithRunnerState(response, root, req, records, catalog, session, runner, savedSupervisor(meta), runnerReleased, launchStateRecorded)
	return nil
}

func savedSupervisor(meta *task.MetaRecord) *task.SupervisorRef {
	if meta == nil {
		return nil
	}
	supervisor := meta.SupervisorConfig
	return &supervisor
}

func readProviderSession(td *taskdir.TaskDir) (*task.ProviderRefRecord, error) {
	if td == nil {
		return nil, errors.New("nil task directory")
	}
	session, err := td.ReadProviderIdentity()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return session, err
}

func applyTimeoutContinuationToResult(result commandResult, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) commandResult {
	if err := applyTimeoutContinuationFromTask(&result.response, root, td, req, meta, catalog); err != nil {
		result.err = errors.Join(result.err, err)
		result.response.setError(result.err)
		result.code = 1
	}
	return result
}

func terminalInspectionWithoutError(inspection *taskdir.TaskInspection, inspectErr error) bool {
	return inspectErr == nil && isTerminal(inspection)
}

const collectionSupervisorReconcileBackoff = 100 * time.Millisecond

func reconcileCollectionTimeoutIfActive(watch time.Duration, deadline, nextAttempt time.Time, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, deps storeDependencies, response *Response) (time.Time, error) {
	now := time.Now()
	if watch <= 0 || !now.Before(deadline) || now.Before(nextAttempt) {
		return nextAttempt, nil
	}
	watchContext, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	err := reconcileCollectionTimeout(watchContext, root, td, req, meta, deps, response)
	return time.Now().Add(collectionSupervisorReconcileBackoff), err
}

func collectionWatchExpired(watch time.Duration, deadline time.Time) bool {
	return watch <= 0 || time.Now().After(deadline)
}

func collect(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("collect")
	root, err := resolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStore(root, deps, false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	req, meta, err := td.PreparedRecords()
	if err != nil {
		return failed(response, err, 1)
	}
	timeoutAnnotated := false
	deadline := time.Now().Add(a.Watch)
	nextSupervisorReconcile := time.Time{}
	for {
		outcome, collectErr := collectAttempt(td, meta.Predicate)
		if outcome != nil {
			result := collectionResult(response, outcome, collectErr)
			result = decorateCollectedTerminal(result, root, td, req, meta, deps.providerCatalog())
			return releaseCollectedContinuation(result, td, req, outcome)
		}
		var supervisorErr error
		nextSupervisorReconcile, supervisorErr = reconcileCollectionTimeoutIfActive(a.Watch, deadline, nextSupervisorReconcile, root, td, req, meta, deps, &response)
		inspection, inspectErr := td.Inspect()
		fillInspection(&response, inspection)
		descriptors, descriptorErr := td.LogDescriptors()
		response.Raw = descriptors
		if terminal, handled := collectionInspectionResult(response, inspection, collectErr, inspectErr, descriptorErr); handled {
			terminal = decorateCollectedTerminal(terminal, root, td, req, meta, deps.providerCatalog())
			return releaseCollectedContinuation(terminal, td, req, terminal.response.Outcome)
		}
		timeoutErr := applyTimeoutContinuationOnce(&timeoutAnnotated, &response, root, td, req, meta, deps.providerCatalog())
		combinedErr := errors.Join(collectErr, inspectErr, descriptorErr, supervisorErr, timeoutErr)
		if combinedErr != nil && !isPureCollectionPendingError(combinedErr) {
			return failed(response, combinedErr, 1)
		}
		if terminalInspectionWithoutError(inspection, inspectErr) {
			return commandResult{response: response, code: 0}
		}
		if collectionWatchExpired(a.Watch, deadline) {
			response.Pending = &PendingResponse{Kind: "publication"}
			return commandResult{response: response, code: 3}
		}
		if !waitForCollection(deadline) {
			response.Pending = &PendingResponse{Kind: "publication"}
			return commandResult{response: response, code: 3}
		}
	}
}

func stopTask(root string, td *taskdir.TaskDir, requestID, cause string, supervisorOptions pueue.Options) (StopResponse, error) {
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return StopResponse{}, err
	}
	supervisorOptions = supervisorOptionsForMeta(supervisorOptions, *meta)
	permit, err := td.PrepareStop(requestID, cause, time.Time{})
	if err != nil {
		return StopResponse{}, err
	}
	request, err := permit.Request()
	if err != nil {
		return StopResponse{}, errors.Join(err, permit.Release())
	}
	client, err := newSupervisorClient(context.Background(), root, request.Supervisor, supervisorOptions, true)
	if err != nil {
		return StopResponse{}, errors.Join(err, permit.Release())
	}
	stopResult, stopErr := client.Stop(context.Background(), permit)
	stopErr = errors.Join(stopErr, permit.Release())
	if stopResult.Acknowledged != nil && stopResult.NumericTaskID != nil {
		stopErr = errors.Join(stopErr, td.RecordStopReply(requestID, task.StopReplyFacts{NumericTaskID: *stopResult.NumericTaskID, Action: stopResult.Action, Acknowledged: *stopResult.Acknowledged, Message: stopResult.Message}))
	}
	if stopResult.ObservedState == pueue.StateEnded && stopResult.NumericTaskID != nil {
		stopErr = errors.Join(stopErr, td.RecordStopObservation(requestID, task.StopObservationFacts{NumericTaskID: *stopResult.NumericTaskID, State: "ended", Terminated: true}))
	}
	return stopResponseFromResult(requestID, cause, stopResult), stopErr
}

func cancel(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("cancel")
	root, err := resolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	inspection, err := td.Inspect()
	fillInspection(&response, inspection)
	if err != nil {
		return failed(response, err, 1)
	}
	if isTerminal(inspection) {
		return commandResult{response: response, code: 0}
	}
	requestID, err := task.NewRandomID()
	if err != nil {
		return failed(response, err, 1)
	}
	stopResponse, stopErr := stopTask(root, td, requestID, "user", deps.SupervisorOptions)
	if errors.Is(stopErr, task.ErrTerminalTask) {
		return terminalAfterCancelRace(response, td)
	}
	response.Stop = &stopResponse
	if records, recordsErr := td.ReadStopRecords(); recordsErr == nil {
		response.Stops = stopResponses(records)
	} else {
		stopErr = errors.Join(stopErr, recordsErr)
	}
	finalInspection, inspectErr := td.Inspect()
	fillInspection(&response, finalInspection)
	stopErr = errors.Join(stopErr, inspectErr)
	if stopErr != nil {
		return failed(response, stopErr, 1)
	}
	return commandResult{response: response, code: 0}
}

func terminalAfterCancelRace(response Response, td *taskdir.TaskDir) commandResult {
	inspection, err := td.Inspect()
	fillInspection(&response, inspection)
	if err != nil {
		return failed(response, err, 1)
	}
	if isTerminal(inspection) {
		return commandResult{response: response, code: 0}
	}
	return failed(response, task.ErrTerminalTask, 1)
}

func logs(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("logs")
	root, err := resolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStore(root, deps, false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	inspection, err := td.Inspect()
	fillInspection(&response, inspection)
	if err != nil {
		return failed(response, err, 1)
	}
	descriptors, descriptorErr := td.LogDescriptors()
	response.Raw = descriptors
	if descriptorErr != nil {
		return failed(response, descriptorErr, 1)
	}
	return commandResult{response: response, code: 0}
}

func stopResponseFromResult(id, cause string, result pueue.StopResult) StopResponse {
	terminated := result.ObservedState == pueue.StateEnded && result.NumericTaskID != nil
	return StopResponse{RequestID: id, Cause: cause, Requested: result.Requested, Matched: result.Matched, Attempted: result.Attempted, Action: result.Action, NumericTaskID: copyTaskID(result.NumericTaskID), Acknowledged: result.Acknowledged, InFlight: result.InFlight, ObservedState: string(result.ObservedState), Terminated: terminated, Message: result.Message}
}

func stopResponses(records []taskdir.StopRecord) []StopResponse {
	responses := make([]StopResponse, 0, len(records))
	for _, record := range records {
		responses = append(responses, describeStopRecord(record))
	}
	return responses
}

func failed(response Response, err error, code int) commandResult {
	if errors.Is(err, commonprovider.ErrAuthenticationBlocked) {
		response.Status = preflightStatusBlocked
	}
	response.setError(err)
	return commandResult{response: response, code: code, err: err}
}

func isPureCollectionPendingError(err error) bool {
	return isPureErrorSet(err, func(candidate error) bool {
		return errors.Is(candidate, task.ErrNoSeal) || errors.Is(candidate, task.ErrLockBusy)
	})
}

func isPureSupervisorUncertaintyError(err error) bool {
	return isPureErrorSet(err, func(candidate error) bool {
		return errors.Is(candidate, pueue.ErrUnknown) || errors.Is(candidate, pueue.ErrInFlight)
	})
}

func isPureErrorSet(err error, leaf func(error) bool) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !isPureErrorSet(cause, leaf) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return isPureErrorSet(cause, leaf)
	}
	return leaf(err)
}

func resolvePredecessor(store *taskdir.Store, request task.TaskRecord, id string, supervisorOptions pueue.Options) (prior *task.PriorSession, resultErr error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	td, err := store.OpenTask(id)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	predecessorReq, _, err := td.PreparedRecords()
	if err != nil {
		return nil, err
	}
	if predecessorReq == nil {
		return nil, task.ErrInvariantFault
	}
	if err = validatePredecessorCompatibility(request, *predecessorReq); err != nil {
		return nil, err
	}
	identity, err := td.ReadProviderIdentity()
	if err != nil {
		return nil, fmt.Errorf("predecessor session is unavailable: %w", err)
	}
	if err = predecessorTerminated(store.Root, td, predecessorReq, supervisorOptions); err != nil {
		return nil, err
	}
	return &task.PriorSession{Provider: identity.Provider, ConversationID: identity.ConversationID, PredecessorTaskID: id}, nil
}

func validatePredecessorCompatibility(request, predecessor task.TaskRecord) error {
	if predecessor.Provider != request.Provider || predecessor.Mode != request.Mode {
		return task.ErrIdentityMismatch
	}
	if request.Provider == config.ProviderPiJSON && predecessor.CanonicalCwd != request.CanonicalCwd {
		// Pi's exact-session resume searches globally and prompts to fork when
		// the stored session belongs to another workspace. The direct JSON
		// adapter cannot answer that prompt while preserving the requested
		// session identity, so reject the continuation before admission.
		return task.ErrIdentityMismatch
	}
	return nil
}

var errPredecessorOutcomeUnavailable = errors.New("predecessor has no authoritative terminal outcome")

func predecessorTerminated(root string, td *taskdir.TaskDir, req *task.TaskRecord, supervisorOptions pueue.Options) error {
	inspection, err := td.Inspect()
	if err != nil {
		return err
	}
	if isTerminal(inspection) {
		if err = requireRunnerLeaseRelease(td); err != nil {
			return err
		}
		return releaseTerminalPredecessor(td, req, inspection)
	}
	if timedOut, timeoutErr := releaseTimedOutPredecessor(td, req); timeoutErr != nil {
		return timeoutErr
	} else if timedOut {
		return nil
	}
	inspection, err = reconcilePredecessor(root, td, req, supervisorOptions)
	if err != nil {
		return err
	}
	if isTerminal(inspection) && inspection.Outcome != nil {
		return releaseTerminalPredecessor(td, req, inspection)
	}
	if timedOut, timeoutErr := releaseTimedOutPredecessor(td, req); timeoutErr != nil {
		return timeoutErr
	} else if timedOut {
		return nil
	}
	return errPredecessorOutcomeUnavailable
}

func releaseTerminalPredecessor(td *taskdir.TaskDir, req *task.TaskRecord, inspection *taskdir.TaskInspection) error {
	if inspection == nil || !isTerminal(inspection) || inspection.Outcome == nil {
		return errPredecessorOutcomeUnavailable
	}
	return releaseContinuationSession(td, req, inspection.Outcome)
}

func reconcilePredecessor(root string, td *taskdir.TaskDir, req *task.TaskRecord, supervisorOptions pueue.Options) (*taskdir.TaskInspection, error) {
	_, meta, spec, metaHash, err := requestHashes(td)
	if err != nil {
		return nil, err
	}
	var response Response
	observation, err := reconcileStatus(root, td, req, meta, spec, metaHash, supervisorOptions, &response)
	if err != nil {
		return nil, err
	}
	if err = updateStatusStops(td, &response, observation); err != nil {
		return nil, err
	}
	if observation.State != pueue.StateEnded {
		return nil, pueue.ErrUnknown
	}
	return td.Inspect()
}

func releaseTimedOutPredecessor(td *taskdir.TaskDir, req *task.TaskRecord) (bool, error) {
	records, err := td.ReadStopRecords()
	if err != nil {
		return false, err
	}
	if !hasTerminatedBudgetStop(records) {
		return false, nil
	}
	if err = requireRunnerLeaseRelease(td); err != nil {
		return false, err
	}
	if req.PriorSession == nil {
		return true, nil
	}
	return true, td.ReleaseSessionAfterTimeout(req.Provider, req.PriorSession.ConversationID)
}

func requireRunnerLeaseRelease(td *taskdir.TaskDir) error {
	released, err := td.RunnerLeaseReleased()
	if err != nil {
		return err
	}
	if !released {
		return task.ErrSessionBusy
	}
	return nil
}
