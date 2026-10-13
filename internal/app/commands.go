package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
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
	id, err := dispatchTaskID(a)
	if err != nil {
		return failed(response, withDispatchStage("generate-task-id", err), 1)
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
	var predecessorMeta *task.MetaRecord
	base, err := buildRequest(store.RootID, id, a.Provider, cwd, a.Config, brief, nil)
	if err != nil {
		return failed(response, withDispatchStage("validate-request", err), classifyCode(err, 2))
	}
	if a.ResumeTask != "" {
		deps, prior, predecessorMeta, err = prepareDispatchContinuation(store, deps, base, a.ResumeTask)
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
	restoreEnvironment, restoreErr := dispatchContinuationEnvironment(root, predecessorMeta, req.RequestedConfig.Interactive)
	if restoreErr != nil {
		return failed(response, withDispatchStage("apply-continuation-environment", restoreErr), 1)
	}
	defer restoreRunnerEnvironment(&result, restoreEnvironment)
	return dispatchNew(a, deps, store, req, brief, response)
}

func dispatchTaskID(a Arguments) (string, error) {
	if a.TaskID != "" {
		return a.TaskID, nil
	}
	if a.generatedTaskID != "" {
		return a.generatedTaskID, nil
	}
	return task.NewTaskID()
}

func dispatchContinuationEnvironment(root string, meta *task.MetaRecord, interactive bool) (func() error, error) {
	if meta == nil || !interactive {
		return nil, nil
	}
	return applyContinuationEnvironment(root, meta)
}

func prepareDispatchContinuation(store *taskdir.Store, deps Dependencies, request task.TaskRecord, predecessorID string) (Dependencies, *task.PriorSession, *task.MetaRecord, error) {
	continuationDeps, predecessorMeta, err := continuationDependenciesForTask(store, deps, predecessorID)
	if err != nil {
		return deps, nil, nil, err
	}
	if err = validateContinuationEnvironment(predecessorMeta); err != nil {
		return deps, nil, nil, err
	}
	prior, err := resolvePredecessor(store, continuationDeps, request, predecessorID)
	if err != nil {
		return deps, nil, nil, err
	}
	return continuationDeps, prior, predecessorMeta, nil
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
	continuationArgs, err := buildContinuationArguments(a, root, predecessor, predecessorMeta, brief)
	if err != nil {
		return failed(response, withDispatchStage("resolve-continuation-runner", err), 1)
	}
	deps = continuationDependencies(deps, *predecessorMeta)
	prior, err := loadContinuationPrior(root, predecessor.TaskID, deps)
	if err != nil {
		return failed(response, withDispatchStage("load-continuation-session", err), 2)
	}
	successorID, idErr := task.NewTaskID()
	if idErr != nil {
		return failed(response, withDispatchStage("generate-task-id", idErr), 1)
	}
	successor, successorErr := buildRequest(predecessor.RootID, successorID, continuationArgs.Provider, continuationArgs.Cwd, continuationArgs.Config, continuationArgs.BriefBytes, prior)
	if successorErr != nil {
		return failed(response, withDispatchStage("validate-continuation", successorErr), 2)
	}
	if _, admissionErr := validateContinuationAdmission(deps, root, *predecessor, *predecessorMeta, prior, successor); admissionErr != nil {
		return failed(response, withDispatchStage("validate-continuation", admissionErr), 2)
	}
	continuationArgs.generatedTaskID = successorID
	supervisorOptions := supervisorOptionsForCurrentEnvironment(deps.SupervisorOptions)
	controlContext, cancelControl := supervisorControlContext(supervisorOptions)
	defer cancelControl()
	deps.SupervisorOptions = supervisorOptions
	if recoveryErr := recoverContinuationSupervisor(controlContext, root, &continuationArgs, deps); recoveryErr != nil {
		return failed(response, withDispatchStage("resolve-continuation-supervisor", recoveryErr), 1)
	}
	restoreEnvironment, restoreErr := applyContinuationEnvironment(root, predecessorMeta)
	if restoreErr != nil {
		return failed(response, withDispatchStage("restore-continuation-environment", restoreErr), 1)
	}
	defer restoreRunnerEnvironment(&result, restoreEnvironment)
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

// recoverContinuationSupervisor reopens the predecessor's recorded private
// supervisor only after static continuation admission has succeeded. It keeps
// all stateful recovery in one continuation-specific boundary.
func recoverContinuationSupervisor(ctx context.Context, root string, arguments *Arguments, deps Dependencies) error {
	if arguments == nil || arguments.savedSupervisor == nil {
		return nil
	}
	rebound, err := rebindContinuationSupervisor(ctx, root, *arguments.savedSupervisor, deps)
	if err != nil {
		return err
	}
	if pueue.IsPrivateConfig(root, rebound.ConfigPath) {
		rebound, err = prepareContinuationSuccessor(ctx, root, rebound, deps)
		if err != nil {
			return err
		}
	}
	*arguments.savedSupervisor = rebound
	return nil
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
	ownership, err := runnerOwnershipForTask(root, td, meta)
	if err != nil {
		return nil, nil, nil, err
	}
	if meta != nil && meta.RunnerOwnership == "" && ownership == task.RunnerOwnershipManaged {
		migratedMeta := *meta
		migratedMeta.RunnerOwnership = task.RunnerOwnershipManaged
		meta = &migratedMeta
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

func buildContinuationArguments(a Arguments, root string, predecessor *task.TaskRecord, meta *task.MetaRecord, brief []byte) (Arguments, error) {
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
	continuationArgs.Config = continuationConfig(predecessor.RequestedConfig, a.Config)
	if meta != nil {
		saved := meta.SupervisorConfig
		continuationArgs.savedSupervisor = &saved
	}
	if meta != nil {
		continuationArgs.runnerOwnership = persistedRunnerOwnership(root, meta)
		if continuationArgs.runnerOwnership == task.RunnerOwnershipManaged {
			continuationArgs.Runner = ""
		} else if continuationArgs.Runner == "" {
			runner, err := continuationRunner(root, meta.RunnerExecutable, continuationArgs.runnerOwnership)
			if err != nil {
				return Arguments{}, err
			}
			continuationArgs.Runner = runner
		}
	}
	if continuationArgs.PueueConfig == "" && meta != nil && !pueue.IsPrivateConfig(root, meta.SupervisorConfig.ConfigPath) {
		continuationArgs.PueueConfig = meta.SupervisorConfig.ConfigPath
	}
	return continuationArgs, nil
}

// continuationDependencies keeps the recorded provider preparation contract
// attached to every candidate compiled for this successor. The production
// catalog owns the historical interpretation; injected profile hooks retain
// their test and acceptance behavior.
func continuationDependencies(deps Dependencies, predecessorMeta task.MetaRecord) Dependencies {
	normalized := deps.normalized()
	if !normalized.useCatalogExisting {
		return normalized
	}
	normalized.PrepareCandidate = func(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		if request.RequestedConfig.Interactive && request.PriorSession != nil {
			return normalized.Catalog.Candidate(request)
		}
		return normalized.Catalog.ExistingCandidate(request, predecessorMeta)
	}
	return normalized
}

func continuationDependenciesForTask(store *taskdir.Store, deps Dependencies, id string) (result Dependencies, predecessorMeta *task.MetaRecord, resultErr error) {
	result = deps
	td, err := store.OpenTask(id)
	if err != nil {
		return deps, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return deps, nil, err
	}
	if meta == nil {
		return deps, nil, errors.New("continuation unavailable: predecessor metadata is not recorded")
	}
	return continuationDependencies(deps, *meta), meta, nil
}

// validateContinuationRequest is the single compatibility gate for every
// continuation path. The catalog owns shared and provider-specific policy;
// callers combine it with validateStaticAdmission before they can change
// supervisor state.
func validateContinuationRequest(catalog commonprovider.Catalog, predecessor task.TaskRecord, predecessorMeta task.MetaRecord, successor task.TaskRecord) error {
	return catalog.ValidateContinuation(predecessor, predecessorMeta, successor)
}

// validateContinuationAdmission constructs the exact successor identity and
// applies the shared static admission gate before any continuation recovery.
// The saved launch environment is used only while compiling the provider
// candidate; supervisor control keeps the caller's own environment.
func validateContinuationAdmission(deps Dependencies, root string, predecessor task.TaskRecord, predecessorMeta task.MetaRecord, prior *task.PriorSession, successor task.TaskRecord) (task.TaskRecord, error) {
	if prior == nil {
		return task.TaskRecord{}, errors.New("predecessor session is unavailable")
	}
	priorCopy := *prior
	successor.PriorSession = &priorCopy
	normalized := deps.normalized()
	if err := validateContinuationRequest(normalized.Catalog, predecessor, predecessorMeta, successor); err != nil {
		return task.TaskRecord{}, err
	}
	if err := validateStaticContinuationAdmission(normalized, root, predecessorMeta, successor); err != nil {
		return task.TaskRecord{}, err
	}
	return successor, nil
}

func validateStaticContinuationAdmission(deps Dependencies, root string, predecessorMeta task.MetaRecord, successor task.TaskRecord) (resultErr error) {
	restore, err := applySavedEnvironment(predecessorMeta.Environment, predecessorMeta.EnvironmentRecorded)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, restore()) }()
	return validateStaticAdmission(deps, root, successor)
}

func loadContinuationPrior(root, id string, deps Dependencies) (prior *task.PriorSession, resultErr error) {
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	td, err := store.OpenTask(id)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	predecessor, _, err := td.PreparedRecords()
	if err != nil {
		return nil, err
	}
	return continuationPrior(td, predecessor)
}

func continuationPrior(td *taskdir.TaskDir, predecessor *task.TaskRecord) (*task.PriorSession, error) {
	if predecessor == nil {
		return nil, task.ErrInvariantFault
	}
	identity, err := td.ReadProviderIdentity()
	if err != nil {
		return nil, fmt.Errorf("predecessor session is unavailable: %w", err)
	}
	return &task.PriorSession{Provider: identity.Provider, ConversationID: identity.ConversationID, PredecessorTaskID: predecessor.TaskID}, nil
}

func rebindContinuationSupervisor(ctx context.Context, root string, saved task.SupervisorRef, deps Dependencies) (task.SupervisorRef, error) {
	if ctx == nil {
		return task.SupervisorRef{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return task.SupervisorRef{}, err
	}
	// The task's supervisor reference is durable evidence. Keep it unchanged;
	// newSupervisorClient reopens the private endpoint through the current
	// state-root pair and adopts a compatible daemon without rewriting task or
	// inspection history. Explicit supervisor repair owns real migrations.
	if !pueue.IsPrivateConfig(root, saved.ConfigPath) {
		return saved, nil
	}
	if err := task.ValidateFreshSupervisorRef(saved); err != nil {
		return task.SupervisorRef{}, err
	}
	return rebindPrivateContinuationSupervisor(ctx, root, saved, deps)
}

func rebindPrivateContinuationSupervisor(ctx context.Context, root string, saved task.SupervisorRef, deps Dependencies) (task.SupervisorRef, error) {
	store, err := openStoreContext(ctx, root, deps.storeDependencies(), false)
	if errors.Is(err, os.ErrNotExist) {
		return saved, nil
	}
	if err != nil {
		return task.SupervisorRef{}, err
	}
	resolution, resolutionErr := resolvePrivateSupervisorGeneration(ctx, store, saved)
	closeErr := store.Close()
	if resolutionErr != nil {
		return task.SupervisorRef{}, errors.Join(resolutionErr, closeErr)
	}
	if closeErr != nil {
		return task.SupervisorRef{}, closeErr
	}
	if resolution.PrivateAuthority || resolution.GroupMigrated {
		return resolution.Active, nil
	}
	if resolution.GroupPresent {
		return saved, nil
	}
	current, currentErr := resolveCurrentContinuationSupervisor(ctx, root, deps)
	if currentErr != nil {
		return task.SupervisorRef{}, currentErr
	}
	return reconcilePrivateContinuationBinding(root, saved, current)
}

// reconcilePrivateContinuationBinding is the single no-journal continuation
// decision. The resolved endpoint and config path must remain the same as the
// predecessor; digest drift is accepted only when the state root has no
// conflicting persisted logical binding, allowing an installation upgrade to
// refresh stale task provenance without opening a second queue.
func reconcilePrivateContinuationBinding(root string, saved, current task.SupervisorRef) (task.SupervisorRef, error) {
	if saved.ConfigPath != current.ConfigPath || saved.Endpoint != current.Endpoint {
		return task.SupervisorRef{}, fmt.Errorf("%w: continuation predecessor uses a changed private supervisor endpoint", pueue.ErrPrivateSupervisorMigrationRequired)
	}
	drift, err := pueue.PrivateSupervisorLogicalDrift(root, current)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if drift {
		return task.SupervisorRef{}, fmt.Errorf("%w: continuation predecessor uses a changed private supervisor binding", pueue.ErrPrivateSupervisorMigrationRequired)
	}
	return current, nil
}

func resolveCurrentContinuationSupervisor(ctx context.Context, root string, deps Dependencies) (task.SupervisorRef, error) {
	clientExecutable, daemonExecutable, err := resolvePrivateSupervisorExecutables(root, deps)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	client, err := pueue.PreparePrivate(ctx, clientExecutable, daemonExecutable, root, deps.SupervisorOptions)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	binding := client.Binding()
	if err = client.CloseAfterPending(nil); err != nil {
		return task.SupervisorRef{}, err
	}
	return binding, nil
}

func prepareContinuationSuccessor(ctx context.Context, root string, successor task.SupervisorRef, deps Dependencies) (task.SupervisorRef, error) {
	if !pueue.IsPrivateConfig(root, successor.ConfigPath) {
		return successor, nil
	}
	ready := successor
	err := pueue.WithBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
		current, err := revalidateContinuationSuccessor(lockedContext, root, successor, deps.storeDependencies())
		if err != nil {
			return err
		}
		successor = current
		ready = current
		legacyBinding, present, err := pueue.ReadPrivateRecoveryBinding(root)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		// The alias is the immediate predecessor for the current successor. The
		// continuation's saved task may point to an older generation after a
		// second migration, so it must not be used to validate this cleanup.
		if task.SameSupervisorIdentity(legacyBinding, successor) {
			return nil
		}
		clientExecutable, daemonExecutable, err := resolvePrivateSupervisorExecutables(root, deps)
		if err != nil {
			return err
		}
		currentClient, err := pueue.PreparePrivateWithLock(lockedContext, clientExecutable, daemonExecutable, root, deps.SupervisorOptions)
		if err != nil {
			return err
		}
		if !task.SameSupervisorIdentity(currentClient.Binding(), successor) {
			return errors.Join(pueue.ErrBinding, currentClient.Close())
		}
		startErr := currentClient.StartPrivate(lockedContext, root)
		closeErr := currentClient.CloseAfterPending(startErr)
		if startErr != nil {
			return errors.Join(startErr, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		ready = currentClient.Binding()
		return pueue.RetirePrivateRecoveryBindingContext(lockedContext, root, ready)
	})
	return ready, err
}

func revalidateContinuationSuccessor(ctx context.Context, root string, candidate task.SupervisorRef, deps storeDependencies) (task.SupervisorRef, error) {
	store, err := openStoreContext(ctx, root, deps, false)
	if errors.Is(err, os.ErrNotExist) {
		return candidate, nil
	}
	if err != nil {
		return task.SupervisorRef{}, err
	}
	resolution, resolutionErr := resolvePrivateSupervisorGenerationWithLock(ctx, store, candidate)
	if resolutionErr == nil && resolution.PrivateAuthority {
		resolutionErr = completePendingGroupMigrationWithLock(ctx, store, resolution)
	}
	closeErr := store.Close()
	if resolutionErr != nil {
		return task.SupervisorRef{}, errors.Join(resolutionErr, closeErr)
	}
	if closeErr != nil {
		return task.SupervisorRef{}, closeErr
	}
	if resolution.PrivateAuthority || resolution.GroupMigrated {
		return resolution.Active, nil
	}
	if resolution.GroupPresent && !task.SameSupervisorIdentity(resolution.Active, candidate) {
		return task.SupervisorRef{}, task.ErrIdentityMismatch
	}
	return candidate, nil
}

func continuationConfig(requested, overrides task.TaskConfig) task.TaskConfig {
	requested.Interactive = requested.Interactive || overrides.Interactive
	if overrides.Budget != "" {
		requested.Budget = overrides.Budget
	}
	if overrides.Model != "" {
		requested.Model = overrides.Model
	}
	if overrides.Effort != "" {
		requested.Effort = overrides.Effort
	}
	if overrides.Budget == "" || requested.NativeTimeout == "" {
		return requested
	}
	budget, err := config.ParseBudget(requested.Budget)
	if err != nil {
		return requested
	}
	nativeTimeout, err := time.ParseDuration(requested.NativeTimeout)
	if err == nil && nativeTimeout > budget {
		requested.NativeTimeout = budget.String()
	}
	return requested
}

func continuationRunner(root, savedRunner, ownership string) (string, error) {
	if ownership == task.RunnerOwnershipCustom {
		if savedRunner == "" {
			return "", task.ErrEvidenceFault
		}
		if _, err := os.Stat(savedRunner); err != nil {
			return "", fmt.Errorf("predecessor custom runner %q is unavailable: %w", savedRunner, err)
		}
		return savedRunner, nil
	}
	if savedRunner == "" || isStateRunnerPath(root, filepath.Clean(savedRunner)) {
		return "", nil
	}
	if _, err := os.Stat(savedRunner); err == nil || !errors.Is(err, os.ErrNotExist) {
		return savedRunner, nil
	}
	return "", nil
}

func validateContinuationMetadata(root string, a Arguments, meta *task.MetaRecord) error {
	if err := validateContinuationEnvironment(meta); err != nil {
		return err
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

func validateContinuationEnvironment(meta *task.MetaRecord) error {
	if meta == nil || (!meta.EnvironmentRecorded && len(meta.Environment) == 0) {
		return errors.New("continuation unavailable: predecessor launch environment is not recorded")
	}
	return nil
}

func applyContinuationEnvironment(root string, meta *task.MetaRecord) (func() error, error) {
	restore, err := applySavedEnvironment(meta.Environment, meta.EnvironmentRecorded)
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

	if !sameExistingRequest(a, &req, oldReq) {
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
		if err = repairPrivateQueueForTask(context.Background(), store, oldMeta.SupervisorConfig, oldMeta, deps); err != nil {
			return failed(response, err, classifyCode(err, 1))
		}
		return reconcileExisting(store.Root, td, oldReq, oldMeta, submit, deps.SupervisorOptions, deps.InitialSupervisorExecutable, response)
	}
	if !errors.Is(submitErr, os.ErrNotExist) {
		return failed(response, submitErr, classifyCode(submitErr, 1))
	}
	if a.Runner != "" && a.Runner != oldMeta.RunnerExecutable {
		return failed(response, task.ErrIdentityMismatch, 2)
	}
	if err = repairPrivateQueueForTask(context.Background(), store, oldMeta.SupervisorConfig, oldMeta, deps); err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	return retryExistingTask(a, deps, store, td, oldReq, oldMeta, inspection.StartExists, response)
}

func retryExistingTask(a Arguments, deps Dependencies, store *taskdir.Store, td *taskdir.TaskDir, oldReq *task.TaskRecord, oldMeta *task.MetaRecord, startExists bool, response Response) (result commandResult) {
	if startExists {
		return failed(response, task.ErrAlreadyStarted, 1)
	}
	supervisorOptions := supervisorOptionsForCurrentEnvironment(deps.SupervisorOptions)
	restoreEnvironment, restoreErr := applySavedEnvironment(oldMeta.Environment, oldMeta.EnvironmentRecorded)
	if restoreErr != nil {
		return failed(response, restoreErr, 1)
	}
	defer restoreRunnerEnvironment(&result, restoreEnvironment)
	profile, profileErr := prepareMatchedProfile(deps, store.Root, *oldReq, *oldMeta)
	if profileErr != nil {
		return failed(response, profileErr, classifyCode(profileErr, 2))
	}
	a, allowManagedRunnerUpgrade, runnerErr := refreshManagedRetryRunner(a, deps, store.Root, store, td, oldMeta)
	if runnerErr != nil {
		return failed(response, runnerErr, classifyCode(runnerErr, 2))
	}
	return markDispatchSubmissionFailure(submitPreparedWithOptionsForRetry(a, deps, td, oldReq, oldMeta, oldMeta.SupervisorConfig,
		supervisorOptionsForProfile(supervisorOptions, profile), store.Root, allowManagedRunnerUpgrade, response))
}

func dispatchNew(a Arguments, deps Dependencies, store *taskdir.Store, req task.TaskRecord, brief []byte, response Response) (result commandResult) {
	prepared, err := prepareAdmission(a, deps, store, req)
	if err != nil {
		err = withDispatchStage("prepare-admission", annotateMissingDispatchStage("preparing inspection admission", err))
		return failed(response, err, classifyCode(err, 2))
	}
	response.Capability = &prepared.Capability
	profile, supervisor := prepared.Profile, prepared.Supervisor
	defer func() { closeSupervisorMaintenance(supervisor, result.err) }()
	runner := prepared.RunnerExecutable
	if err = profile.Validate(req); err != nil {
		return failed(response, withDispatchStage("validate-profile", err), classifyCode(err, 2))
	}
	runnerOwnership := prepared.RunnerOwnership
	meta := newMeta(req, profile, supervisor.Binding(), runner, runnerOwnership, deps.PublisherVersion)
	if brief == nil {
		brief, err = readBriefFileForRequest(a.Brief, req)
		if err != nil {
			err = annotateMissingDispatchStage("reading brief", err)
			return failed(response, withDispatchStage("read-brief", err), classifyCode(err, 1))
		}
	}
	td, err := store.CreateTask(req.TaskID, &req, brief, &meta)
	if err != nil {
		response.TaskRecord = "unknown"
		err = annotateMissingDispatchStage("creating ordinary task", err)
		return failed(response, withDispatchStage("create-task-record", err), classifyCode(err, 1))
	}
	response.TaskRecord = "created"
	submitArgs := a
	submitArgs.Runner = runner
	result = submitPreparedWithProfile(submitArgs, deps, td, &req, &meta, supervisor, profile, response)
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
		case errors.Is(result.err, pueue.ErrEnqueueUncertain):
			stage = "enqueue-task"
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
	return submitPreparedWithOptionsMode(a, deps, td, req, meta, supervisor, supervisorOptions, recoveryRoot, false, response)
}

func submitPreparedWithOptionsForRetry(a Arguments, deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisor task.SupervisorRef, supervisorOptions pueue.Options, recoveryRoot string, allowManagedRunnerUpgrade bool, response Response) (result commandResult) {
	return submitPreparedWithOptionsMode(a, deps, td, req, meta, supervisor, supervisorOptions, recoveryRoot, allowManagedRunnerUpgrade, response)
}

func submitPreparedWithOptionsMode(a Arguments, deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisor task.SupervisorRef, supervisorOptions pueue.Options, recoveryRoot string, allowManagedRunnerUpgrade bool, response Response) (result commandResult) {
	if meta != nil {
		supervisorOptions = supervisorOptionsForMeta(supervisorOptions, *meta)
	}
	client, err := newSupervisorClient(context.Background(), recoveryRoot, supervisor, supervisorOptions, recoveryRoot != "", deps.InitialSupervisorExecutable)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	defer func() { closeSupervisorMaintenance(client, result.err) }()
	runner, err := resolveTaskRunner(a.Runner, deps, meta)
	if err != nil {
		return failed(response, err, classifyCode(err, 2))
	}
	if allowManagedRunnerUpgrade {
		err = validateManagedRetrySubmissionRunner(deps, td, req, meta, runner)
	} else {
		err = validateSubmissionRunner(deps, td, req, meta, runner)
	}
	if err != nil {
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

// validateSubmissionRunner binds ordinary launches to immutable task metadata
// and, when present, the inspection request. A managed successor is accepted
// only after that exact request durably authorized its worker migration.
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
	return validateInspectionSubmissionRunner(operation, store, td, req, meta, runner)
}

func validateManagedRetrySubmissionRunner(deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner string) error {
	if meta == nil {
		return task.ErrEvidenceFault
	}
	if err := validateManagedRetryRunner(td, meta, runner); err != nil {
		return err
	}
	retryMeta := *meta
	// Validate the selected executable against inspection evidence while
	// retaining the task's recorded ownership classification.
	retryMeta.RunnerExecutable = runner
	return validateSubmissionRunner(deps, td, req, &retryMeta, runner)
}

func validateInspectionSubmissionRunner(operation *inspection.Operation, store *taskdir.Store, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner string) error {
	request := operation.Request()
	requestBytes, err := task.MarshalCanonical(*req)
	if err != nil || request.RootID != store.RootID || request.TaskID != req.TaskID || request.TaskSHA256 != task.ComputeSHA256(requestBytes) {
		return task.ErrEvidenceFault
	}
	binding := request.Binding
	if meta != nil && !task.SameSupervisorIdentity(binding.Supervisor, meta.SupervisorConfig) {
		return task.ErrEvidenceFault
	}
	sameWorker := runner == binding.WorkerExecutable
	if sameWorker {
		digest, fingerprintErr := commonprovider.FingerprintExecutable(runner)
		if fingerprintErr != nil {
			return task.ErrEvidenceFault
		}
		if digest != binding.WorkerSHA256 {
			return task.ErrEvidenceFault
		}
		return nil
	}
	return validateManagedInspectionSuccessor(operation, td, meta, runner, false)
}

func validateManagedInspectionSuccessor(operation *inspection.Operation, td *taskdir.TaskDir, meta *task.MetaRecord, runner string, sameWorker bool) error {
	binding := operation.Request().Binding
	if meta == nil || meta.RunnerOwnership != task.RunnerOwnershipManaged || binding.RunnerOwnership != task.RunnerOwnershipManaged {
		return inspectionWorkerMismatch(sameWorker)
	}
	if err := validateManagedRetryRunner(td, meta, runner); err != nil {
		return err
	}
	authorized, err := operation.ManagedWorkerUpgradeAuthorizedContext(context.Background())
	if err != nil {
		return err
	}
	if !authorized {
		if _, proofErr := operation.ReadProofContext(context.Background()); proofErr != nil {
			if !errors.Is(proofErr, os.ErrNotExist) {
				return proofErr
			}
			return inspectionWorkerMismatch(sameWorker)
		}
		root := filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))
		if !isStateRunnerPath(root, filepath.Clean(runner)) {
			return task.ErrIdentityMismatch
		}
	}
	return nil
}

func inspectionWorkerMismatch(sameWorker bool) error {
	if sameWorker {
		return task.ErrEvidenceFault
	}
	return task.ErrIdentityMismatch
}

func validateManagedRetryRunner(td *taskdir.TaskDir, meta *task.MetaRecord, runner string) error {
	if td == nil || meta == nil {
		return task.ErrEvidenceFault
	}
	root := filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))
	ownership, err := runnerOwnershipForTask(root, td, meta)
	if err != nil {
		return err
	}
	if ownership != task.RunnerOwnershipManaged || !currentManagedRunnerExecutable(root, runner) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func submitWithClient(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, client *pueue.Client, runner string, permit *taskdir.SubmissionPermit, response Response) commandResult {
	root := filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))
	ownership, err := runnerOwnershipForTask(root, td, meta)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	commit := func(observation pueue.Observation) error {
		receipt, receiptErr := observation.Receipt()
		if receiptErr != nil {
			return receiptErr
		}
		return td.RecordSupervisorReceipt(*receipt)
	}
	observation, err := client.SubmitStagedWithGate(context.Background(), permit, pueue.Launch{
		RunnerExecutable: runner,
		RootPath:         root,
		Supervised:       ownership == task.RunnerOwnershipManaged,
	}, privateSubmissionGate(root), commit)
	response.Supervisor = supervisorResponse(observation)
	response.Liveness = mapPueueState(observation.State)
	response.Pending = pendingResponse(observation)
	if observation.Matched {
		// A matched Stashed row proves supervisor acceptance. The staged commit
		// publishes its exact receipt before enqueue, so a commit or enqueue
		// failure leaves recoverable work that status can safely resume.
		response.Admission = task.AdmissionAdmitted.String()
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

func reconcileExisting(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, submit *task.SubmitRecord, supervisorOptions pueue.Options, initialSupervisorExecutable string, response Response) (result commandResult) {
	supervisorOptions = supervisorOptionsForMeta(supervisorOptions, *meta)
	client, err := newSupervisorClient(context.Background(), root, submit.Supervisor, supervisorOptions, true, initialSupervisorExecutable)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	defer func() { closeSupervisorMaintenance(client, result.err) }()
	_, _, spec, metaHash, hashErr := requestHashes(td)
	if hashErr != nil {
		return failed(response, hashErr, 1)
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return failed(response, receiptErr, 1)
	}
	observation, reconcileErr := client.Reconcile(context.Background(), taskIdentity(req, meta, spec, metaHash, receipt))
	if observation.Matched && observation.State == pueue.StateStashed {
		var recoverErr error
		observation, recoverErr = recoverStagedOrdinarySubmission(context.Background(), td, client, observation)
		reconcileErr = errors.Join(reconcileErr, recoverErr)
	}
	response.Supervisor = supervisorResponse(observation)
	response.Liveness = mapPueueState(observation.State)
	response.Pending = pendingResponse(observation)
	if observation.Matched {
		receiptErr := requireDurableSupervisorReceipt(td, observation)
		reconcileErr = errors.Join(reconcileErr, receiptErr)
		if receiptErr == nil {
			response.Admission = task.AdmissionAdmitted.String()
		} else {
			response.Admission = task.AdmissionUnknown.String()
		}
	} else {
		response.Admission = task.AdmissionUnknown.String()
	}
	if reconcileErr != nil {
		return failed(response, reconcileErr, classifyCode(reconcileErr, 1))
	}
	return commandResult{response: response, code: 0}
}

func reconcileStatus(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, supervisorOptions pueue.Options, initialSupervisorExecutable string, response *Response) (pueue.Observation, error) {
	return reconcileStatusContext(context.Background(), root, td, req, meta, spec, metaHash, supervisorOptions, initialSupervisorExecutable, response)
}

func reconcileStatusContext(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, supervisorOptions pueue.Options, initialSupervisorExecutable string, response *Response) (observation pueue.Observation, resultErr error) {
	if ctx == nil {
		return pueue.Observation{}, context.Canceled
	}
	supervisorOptions = supervisorOptionsForMeta(supervisorOptions, *meta)
	submit, err := td.ReadSubmission()
	if err != nil {
		return pueue.Observation{}, err
	}
	client, err := newSupervisorClient(ctx, root, submit.Supervisor, supervisorOptions, true, initialSupervisorExecutable)
	if err != nil {
		return pueue.Observation{}, err
	}
	defer func() { closeSupervisorMaintenance(client, resultErr) }()
	return reconcileStatusWithClient(ctx, root, td, req, meta, spec, metaHash, client, response)
}

func reconcileStatusWithClient(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, client *pueue.Client, response *Response) (observation pueue.Observation, resultErr error) {
	if ctx == nil {
		return pueue.Observation{}, context.Canceled
	}
	if client == nil {
		return pueue.Observation{}, task.ErrInvalidPermit
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return pueue.Observation{}, receiptErr
	}
	statusClient := client
	if pueue.IsPrivateConfig(root, client.Binding().ConfigPath) {
		statusClient, receiptErr = client.WithObservationTimeout(pueue.PrivateStatusObservationTimeout)
		if receiptErr != nil {
			return pueue.Observation{}, receiptErr
		}
	}
	observation, reconcileErr := statusClient.ReconcileStatus(ctx, taskIdentity(req, meta, spec, metaHash, receipt))
	if observation.Matched && observation.State == pueue.StateStashed {
		var recoverErr error
		observation, recoverErr = recoverStagedOrdinarySubmission(ctx, td, statusClient, observation)
		reconcileErr = errors.Join(reconcileErr, recoverErr)
	}
	response.Supervisor = supervisorResponse(observation)
	response.Pending = pendingResponse(observation)
	if observation.Matched {
		receiptErr := requireDurableSupervisorReceipt(td, observation)
		reconcileErr = errors.Join(reconcileErr, receiptErr)
		if receiptErr == nil {
			response.Admission = task.AdmissionAdmitted.String()
		} else {
			response.Admission = task.AdmissionUnknown.String()
		}
	} else {
		response.Admission = task.AdmissionUnknown.String()
	}
	if observation.State == pueue.StateEnded {
		if inspection, inspectErr := td.Inspect(); inspectErr == nil && inspection.Outcome != nil {
			cleanupCompletedTask(ctx, root, td, client)
		}
	}
	return observation, reconcileErr
}

func recoverStagedOrdinarySubmission(ctx context.Context, td *taskdir.TaskDir, client *pueue.Client, observation pueue.Observation) (pueue.Observation, error) {
	commit := func(current pueue.Observation) error {
		receipt, err := current.Receipt()
		if err != nil {
			return err
		}
		return td.RecordSupervisorReceiptContext(ctx, *receipt)
	}
	recovered, err := client.RecoverStaged(ctx, observation.Identity, observation.NumericTaskID, commit)
	if err == nil || !errors.Is(err, pueue.ErrUnknown) {
		return recovered, err
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil || receipt.NumericTaskID != observation.NumericTaskID {
		return recovered, errors.Join(err, receiptErr)
	}
	current, observeErr := client.ReconcileStatus(ctx, observation.Identity)
	if observeErr != nil || !current.Matched || current.NumericTaskID != observation.NumericTaskID {
		return current, errors.Join(err, observeErr)
	}
	switch current.State {
	case pueue.StateQueued, pueue.StateRunning, pueue.StateEnded:
		return current, requireDurableSupervisorReceipt(td, current)
	case pueue.StateUnknown, pueue.StateStashed:
		return current, err
	}
	return current, err
}

// requireDurableSupervisorReceipt validates queue state against the receipt
// created by the exact staged submission. A queue observation can confirm that
// authority, but it can never create it retroactively.
func requireDurableSupervisorReceipt(td *taskdir.TaskDir, observation pueue.Observation) error {
	if td == nil || !observation.Matched {
		return task.ErrEvidenceFault
	}
	observed, err := observation.Receipt()
	if err != nil {
		return err
	}
	recorded, err := td.ReadSupervisorReceipt()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		return err
	}
	if *recorded != *observed {
		return task.ErrIdentityMismatch
	}
	return nil
}

func updateStatusStops(td *taskdir.TaskDir, response *Response, observation pueue.Observation) error {
	return updateStatusStopsContext(context.Background(), td, response, observation)
}

func updateStatusStopsContext(ctx context.Context, td *taskdir.TaskDir, response *Response, observation pueue.Observation) error {
	if ctx == nil {
		return context.Canceled
	}
	queueEnded := observation.State == pueue.StateEnded && observation.Matched
	if queueEnded {
		response.Liveness = task.LivenessRunning.String()
	} else {
		response.Liveness = mapPueueState(observation.State)
	}
	records, err := td.ReadStopRecords()
	if err != nil {
		return err
	}
	if !queueEnded {
		response.Stops = stopResponses(records)
		return nil
	}
	if err = recoverTaskLifetime(ctx, td); errors.Is(err, errLifetimeRecoveryPending) {
		markLifetimeFinalizationPending(response)
		response.Stops = stopResponses(records)
		return nil
	} else if err != nil {
		return err
	}
	ready, err := td.StopTerminationReady()
	if err != nil {
		return err
	}
	if !ready {
		response.Stops = stopResponses(records)
		return nil
	}
	response.Liveness = task.LivenessEnded.String()
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
	err = errors.Join(err, fillInspectedInteraction(&response, td, inspection))
	fillInspection(&response, inspection)
	if err != nil {
		return failed(response, err, 1)
	}
	if isTerminal(inspection) {
		req, meta, _, _, reqErr := requestHashes(td)
		if reqErr != nil {
			return failed(response, reqErr, 1)
		}
		cleanupCompletedTaskFromSaved(root, td, meta, deps.storeDependencies())
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
	submission, err := td.ReadSubmission()
	if errors.Is(err, os.ErrNotExist) {
		response.Admission = task.AdmissionNotAdmitted.String()
		return commandResult{response: response, code: 0}
	}
	if err != nil {
		return failed(response, err, 1)
	}
	privateSupervisor, err := preparePrivateTaskSupervisor(context.Background(), store, submission.Supervisor, meta, deps)
	if err != nil {
		return statusObservationResult(response, err)
	}
	if privateSupervisor != nil {
		defer func() { closeSupervisorMaintenance(privateSupervisor, result.err) }()
	}
	var observation pueue.Observation
	var reconcileErr error
	if privateSupervisor != nil {
		observation, reconcileErr = reconcileStatusWithClient(context.Background(), root, td, req, meta, spec, metaHash, privateSupervisor, &response)
	} else {
		observation, reconcileErr = reconcileStatus(root, td, req, meta, spec, metaHash, deps.SupervisorOptions, deps.InitialSupervisorExecutable, &response)
	}
	reconcileErr = errors.Join(reconcileErr, updateStatusStops(td, &response, observation))
	if records, recordsErr := readTimeoutRecords(td); recordsErr == nil {
		reconcileErr = errors.Join(reconcileErr, applyTimeoutContinuationWithRecords(&response, root, td, req, meta, records, deps.normalized().Catalog))
	} else {
		reconcileErr = errors.Join(reconcileErr, recordsErr)
	}
	return statusObservationResult(response, reconcileErr)
}

func statusObservationResult(response Response, err error) commandResult {
	if err != nil && !isPureSupervisorUncertaintyError(err) {
		return failed(response, err, 1)
	}
	return commandResult{response: response, code: 0, err: err}
}

func recheck(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("recheck")
	root, err := resolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 2)
	}
	store, err := openStore(root, deps, false)
	if err != nil {
		code := 1
		if errors.Is(err, task.ErrUnsupportedSchema) {
			code = 2
		}
		return failed(response, err, code)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		code := 1
		if errors.Is(err, os.ErrNotExist) {
			code = 2
		}
		return failed(response, err, code)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	return recheckTask(response, td, deps.registry())
}

func recheckTask(response Response, td *taskdir.TaskDir, registry predicate.Registry) commandResult {
	inspection, err := td.Inspect()
	fillInspection(&response, inspection)
	if err != nil {
		code := 1
		if errors.Is(err, task.ErrUnsupportedSchema) {
			code = 2
		}
		return failed(response, err, code)
	}
	if inspection == nil || inspection.Outcome == nil || inspection.Publication != task.PublicationRejected || inspection.Outcome.Verdict != task.VerdictRejected {
		return failed(response, task.ErrRecheckNotRejected, 2)
	}
	interpreter, err := registry.ResolveRecovery(inspection.Outcome.Predicate)
	if err != nil {
		return failed(response, err, 2)
	}
	assessment, err := td.Recheck(interpreter)
	if err != nil {
		code := 1
		if errors.Is(err, predicate.ErrRecoveryUnsupported) || errors.Is(err, predicate.ErrRecoveryAmbiguous) || errors.Is(err, task.ErrRecheckNotRejected) || errors.Is(err, task.ErrUnsupportedSchema) {
			code = 2
		}
		return failed(response, err, code)
	}
	response.Recheck = recheckResponse(assessment, td.Dir)
	response.Error = ""
	response.Failure = nil
	response.Diagnostics = nil
	applyOutputDiagnostics(&response, assessment.Report.Diagnostics)
	if assessment.Report.Status == task.RecheckStatusRecovered {
		return commandResult{response: response, code: 0}
	}
	return commandResult{response: response, code: 4}
}

func recheckResponse(result *taskdir.RecheckResult, taskPath string) *RecheckResponse {
	if result == nil || result.Report == nil {
		return nil
	}
	report := result.Report
	response := &RecheckResponse{
		Status:               report.Status,
		AssessmentID:         report.AssessmentID,
		SourceOutcomeSHA256:  report.SourceOutcomeSHA256,
		SourceSealSHA256:     report.SourceSealSHA256,
		SourceEvidenceSHA256: report.SourceEvidenceSHA256,
		SourcePredicate:      report.SourcePredicate,
		Target:               report.TargetPredicate,
		ReportPath:           filepath.Join(taskPath, result.ReportPath),
		Diagnostics:          task.CloneOutputDiagnostics(report.Diagnostics),
	}
	if report.Result != nil {
		answer := *report.Result
		response.Answer = &answer
		response.AnswerPath = filepath.Join(taskPath, result.AnswerPath)
	}
	return response
}

func collectionResult(response Response, outcome *task.OutcomeRecord, collectErr error) commandResult {
	response.Outcome = outcome
	response.Payload = &outcome.Payload
	response.EvidenceSHA256 = outcome.EvidenceSHA256
	applyOutputDiagnostics(&response, outcome.Diagnostics)
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

// decorateRejectedRecovery makes a rejected publication actionable without
// changing its terminal authority. The offer is deliberately advisory: an
// unavailable, ambiguous, already-assessed, or corrupt recovery target falls
// back to stop_and_report and never turns collection into a re-evaluation.
func decorateRejectedRecovery(result commandResult, td *taskdir.TaskDir, registry predicate.Registry) commandResult {
	if result.response.Publication != task.PublicationRejected.String() || result.response.Outcome == nil || result.err != nil {
		return result
	}
	failure := DispatchFailureResponse{Code: "provider_output_rejected", Stage: "collect", NextAction: "stop_and_report"}
	interpreter, err := registry.ResolveRecovery(result.response.Outcome.Predicate)
	if err == nil && td != nil {
		available, availabilityErr := td.RecheckAvailable(interpreter)
		if availabilityErr == nil && available {
			failure.NextAction = "recheck"
		}
	}
	result.response.Failure = &failure
	return result
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

// cleanupCompletedTask is best effort maintenance. The task store's terminal
// outcome is authoritative, so a supervisor cleanup failure must never turn a
// committed or rejected task back into an application failure.
func cleanupCompletedTask(ctx context.Context, root string, td *taskdir.TaskDir, client *pueue.Client) {
	if client == nil || !pueue.IsPrivateConfig(root, client.Binding().ConfigPath) {
		return
	}
	receipt, err := td.ReadSupervisorReceipt()
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("delegation-layer: cannot read completed supervisor receipt for cleanup: %v", err)
		return
	}
	if !task.SameSupervisorIdentity(receipt.SupervisorRef(), client.Binding()) {
		migrated, migrationErr := pueue.PrivateSupervisorMigrationComplete(root, client.Binding())
		if migrationErr != nil {
			log.Printf("delegation-layer: cannot verify private supervisor migration for completed task cleanup: %v", migrationErr)
			return
		}
		if !migrated {
			log.Printf("delegation-layer: completed supervisor receipt no longer matches the current private supervisor")
			return
		}
	}
	numericTaskID := receipt.NumericTaskID
	identity := pueue.Identity{
		RootID:        receipt.RootID,
		TaskID:        receipt.TaskID,
		SpecSHA256:    receipt.SpecSHA256,
		MetaSHA256:    receipt.MetaSHA256,
		NumericTaskID: &numericTaskID,
	}
	if err = client.RemoveCompletedTask(ctx, identity); err != nil && !errors.Is(err, pueue.ErrCleanupDeferred) {
		log.Printf("delegation-layer: completed supervisor task %d retained for retry: %v", receipt.NumericTaskID, err)
	}
}

func cleanupCompletedTaskFromSaved(root string, td *taskdir.TaskDir, meta *task.MetaRecord, deps storeDependencies) {
	if td == nil || meta == nil {
		return
	}
	submit, err := td.ReadSubmission()
	if err != nil || !pueue.IsPrivateConfig(root, submit.Supervisor.ConfigPath) || submit.Supervisor.ConfigPath != pueue.PrivateConfigPath(root) {
		// A retained recovery alias belongs to an explicit migration and is
		// cleaned by that transition, never by ordinary task collection.
		return
	}
	if _, err = os.Stat(submit.Supervisor.ConfigPath); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pueue.PrivateControlTimeout(deps.supervisorOptions))
	defer cancel()
	clientExecutable, daemonExecutable, resolveErr := resolvePrivateSupervisorExecutables(root, Dependencies{InitialSupervisorExecutable: deps.initialSupervisorExecutable})
	if resolveErr != nil {
		log.Printf("delegation-layer: cannot resolve private supervisor for completed task cleanup: %v", resolveErr)
		return
	}
	client, ready, err := pueue.ProbePrivate(ctx, clientExecutable, daemonExecutable, root, supervisorOptionsForMeta(deps.supervisorOptions, *meta))
	if err != nil {
		log.Printf("delegation-layer: cannot bind private supervisor for completed task cleanup: %v", err)
		return
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			log.Printf("delegation-layer: private supervisor cleanup close failed: %v", closeErr)
		}
	}()
	if !ready {
		return
	}
	cleanupCompletedTask(ctx, root, td, client)
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
	observation, reconcileErr := reconcileStatusContext(ctx, root, td, req, meta, spec, metaHash, deps.supervisorOptions, deps.initialSupervisorExecutable, response)
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
	commandRunner := ""
	if meta != nil {
		runner = meta.RunnerExecutable
		ownership, ownershipErr := runnerOwnershipForTask(root, td, meta)
		if ownershipErr != nil {
			return ownershipErr
		}
		commandRunner = timeoutContinuationCommandRunner(meta, ownership)
	}
	launchStateRecorded := meta != nil && (meta.EnvironmentRecorded || len(meta.Environment) > 0)
	applyTimeoutContinuationWithRunnerStateAndCommandRunner(response, root, req, records, catalog, session, runner, commandRunner, savedSupervisor(meta), runnerReleased, launchStateRecorded)
	return nil
}

func timeoutContinuationCommandRunner(meta *task.MetaRecord, ownership string) string {
	if meta == nil || ownership == task.RunnerOwnershipManaged {
		return ""
	}
	return meta.RunnerExecutable
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
	return collectPreparedTask(a, deps, root, td, req, meta, response)
}

func collectPreparedTask(a Arguments, deps storeDependencies, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, response Response) commandResult {
	timeoutAnnotated := false
	deadline := time.Now().Add(a.Watch)
	nextSupervisorReconcile := time.Time{}
	for {
		outcome, collectErr := collectWithInteraction(td, meta.Predicate, &response)
		if outcome != nil {
			cleanupCompletedTaskFromSaved(root, td, meta, deps)
			result := collectionResult(response, outcome, collectErr)
			result = decorateCollectedTerminal(result, root, td, req, meta, deps.providerCatalog())
			result = releaseCollectedContinuation(result, td, req, outcome)
			return decorateRejectedRecovery(result, td, deps.registry())
		}
		var supervisorErr error
		nextSupervisorReconcile, supervisorErr = reconcileCollectionTimeoutIfActive(a.Watch, deadline, nextSupervisorReconcile, root, td, req, meta, deps, &response)
		inspection, inspectErr := td.Inspect()
		fillInspection(&response, inspection)
		descriptors, descriptorErr := td.LogDescriptors()
		response.Raw = descriptors
		if terminal, handled := collectionInspectionResult(response, inspection, collectErr, inspectErr, descriptorErr); handled {
			cleanupCompletedTaskFromSaved(root, td, meta, deps)
			terminal = decorateCollectedTerminal(terminal, root, td, req, meta, deps.providerCatalog())
			terminal = releaseCollectedContinuation(terminal, td, req, terminal.response.Outcome)
			return decorateRejectedRecovery(terminal, td, deps.registry())
		}
		timeoutErr := applyTimeoutContinuationOnce(&timeoutAnnotated, &response, root, td, req, meta, deps.providerCatalog())
		combinedErr := errors.Join(collectErr, inspectErr, descriptorErr, supervisorErr, timeoutErr)
		if combinedErr != nil && !isPureCollectionPendingError(combinedErr) {
			return failed(response, combinedErr, 1)
		}
		if terminalInspectionWithoutError(inspection, inspectErr) {
			return commandResult{response: response, code: 0}
		}
		if lifetimeFinalizationPending(response) {
			return commandResult{response: response, code: 3}
		}
		if interactionWaiting(response.Interaction) {
			response.Pending = &PendingResponse{Kind: "interaction"}
			return commandResult{response: response, code: 3}
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

func stopTask(root string, td *taskdir.TaskDir, requestID, cause string, supervisorOptions pueue.Options, initialSupervisorExecutable string) (response StopResponse, resultErr error) {
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
	owner, err := td.TryAcquireLifetimeOwner()
	if errors.Is(err, task.ErrLockBusy) {
		consumeErr := permit.Consume()
		releaseErr := permit.Release()
		return StopResponse{
			RequestID:     requestID,
			Cause:         cause,
			Requested:     true,
			Attempted:     consumeErr == nil,
			Action:        "terminate",
			NumericTaskID: copyTaskID(request.NumericTaskID),
			InFlight:      consumeErr == nil,
			ObservedState: string(pueue.StateUnknown),
			Message:       "durable cancellation routed to the task lifetime owner",
		}, errors.Join(consumeErr, releaseErr)
	}
	if err != nil {
		return StopResponse{}, errors.Join(err, permit.Release())
	}
	ownerHeld := true
	defer func() {
		if ownerHeld {
			// LifetimeOwnerLease release is deliberately retryable. A failed
			// unlock keeps the lease authoritative, so the deferred attempt must
			// run instead of abandoning the native-lifetime fence.
			resultErr = errors.Join(resultErr, owner.Release())
		}
	}()
	client, err := newSupervisorControlClient(context.Background(), root, request.Supervisor, supervisorOptions, initialSupervisorExecutable)
	if err != nil {
		return StopResponse{}, errors.Join(err, permit.Release())
	}
	defer func() { closeSupervisorMaintenance(client, resultErr) }()
	stopResult, stopErr := client.Stop(pueue.WithControlLease(context.Background(), owner), permit)
	stopErr = errors.Join(stopErr, permit.Release())
	if stopResult.Acknowledged != nil && stopResult.NumericTaskID != nil {
		stopErr = errors.Join(stopErr, td.RecordStopReply(requestID, task.StopReplyFacts{NumericTaskID: *stopResult.NumericTaskID, Action: stopResult.Action, Acknowledged: *stopResult.Acknowledged, Message: stopResult.Message}))
	}
	var pendingStop <-chan struct{}
	if stopResult.Pending != nil {
		pendingStop = stopResult.Pending.Done()
	}
	if releaseErr := releaseLifetimeOwnerAfterPending(pendingStop, owner); releaseErr != nil {
		stopErr = errors.Join(stopErr, releaseErr)
	} else {
		ownerHeld = false
	}
	terminated, terminationErr := recordStoppedTaskTermination(context.Background(), td, requestID, stopResult)
	stopErr = errors.Join(stopErr, terminationErr)
	response = stopResponseFromResult(requestID, cause, stopResult)
	response.Terminated = terminated
	return response, stopErr
}

func releaseLifetimeOwnerAfterPending(pending <-chan struct{}, owner lifetimeGuardianOwner) error {
	if owner == nil {
		return task.ErrEvidenceFault
	}
	if pending != nil {
		// The pending supervisor mutation can still stop the managed guardian.
		// Releasing its lifetime lease first would let a new guardian launch
		// while that older mutation remains capable of ending the queue row.
		<-pending
	}
	return owner.Release()
}

func recordStoppedTaskTermination(ctx context.Context, td *taskdir.TaskDir, requestID string, result pueue.StopResult) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if td == nil {
		return false, task.ErrEvidenceFault
	}
	if !result.Matched || result.ObservedState != pueue.StateEnded || result.NumericTaskID == nil {
		return false, nil
	}
	if err := recoverTaskLifetime(ctx, td); errors.Is(err, errLifetimeRecoveryPending) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	ready, err := td.StopTerminationReady()
	if err != nil || !ready {
		return false, err
	}
	err = td.RecordStopObservationContext(ctx, requestID, task.StopObservationFacts{NumericTaskID: *result.NumericTaskID, State: "ended", Terminated: true})
	return err == nil, err
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
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return failed(response, err, 1)
	}
	submission, err := td.ReadSubmission()
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	if !task.SameSupervisorIdentity(submission.Supervisor, meta.SupervisorConfig) {
		return failed(response, task.ErrIdentityMismatch, 1)
	}
	// Persist the stop request first. The active lifetime owner handles native
	// termination; only an idle task lets this command stop the saved queue row.
	requestID, err := task.NewRandomID()
	if err != nil {
		return failed(response, err, 1)
	}
	stopResponse, stopErr := stopTask(root, td, requestID, "user", deps.SupervisorOptions, deps.InitialSupervisorExecutable)
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
	return StopResponse{RequestID: id, Cause: cause, Requested: result.Requested, Matched: result.Matched, Attempted: result.Attempted, Action: result.Action, NumericTaskID: copyTaskID(result.NumericTaskID), Acknowledged: result.Acknowledged, InFlight: result.InFlight, ObservedState: string(result.ObservedState), Message: result.Message}
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

func preparePredecessorResolution(store *taskdir.Store, td *taskdir.TaskDir, predecessorReq *task.TaskRecord, predecessorMeta *task.MetaRecord, deps Dependencies) (timedOut, submitted bool, resultErr error) {
	inspection, err := td.Inspect()
	if err != nil {
		return false, false, err
	}
	if isTerminal(inspection) {
		return false, true, nil
	}
	timedOut, err = releaseTimedOutPredecessor(td, predecessorReq)
	if err != nil || timedOut {
		return timedOut, false, err
	}
	if _, submitErr := td.ReadSubmission(); submitErr == nil {
		if repairErr := repairPrivateQueueForTask(context.Background(), store, predecessorMeta.SupervisorConfig, predecessorMeta, deps); repairErr != nil {
			return false, false, repairErr
		}
		return false, true, nil
	} else if !errors.Is(submitErr, os.ErrNotExist) {
		return false, false, submitErr
	}
	return false, false, nil
}

func resolvePredecessor(store *taskdir.Store, deps Dependencies, request task.TaskRecord, id string) (prior *task.PriorSession, resultErr error) {
	deps = deps.normalized()
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	td, err := store.OpenTask(id)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	predecessorReq, predecessorMeta, err := td.PreparedRecords()
	if err != nil {
		return nil, err
	}
	prior, err = continuationPrior(td, predecessorReq)
	if err != nil {
		return nil, err
	}
	if _, admissionErr := validateContinuationAdmission(deps, store.Root, *predecessorReq, *predecessorMeta, prior, request); admissionErr != nil {
		return nil, admissionErr
	}
	timedOut, submitted, err := preparePredecessorResolution(store, td, predecessorReq, predecessorMeta, deps)
	if err != nil {
		return nil, err
	}
	if timedOut {
		return prior, nil
	}
	if !submitted {
		return nil, task.ErrRequestConflict
	}
	if err = predecessorTerminated(store.Root, td, predecessorReq, deps.SupervisorOptions, deps.InitialSupervisorExecutable); err != nil {
		return nil, err
	}
	return prior, nil
}

var errPredecessorOutcomeUnavailable = errors.New("predecessor has no authoritative terminal outcome")

func predecessorTerminated(root string, td *taskdir.TaskDir, req *task.TaskRecord, supervisorOptions pueue.Options, initialSupervisorExecutable string) error {
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
	inspection, err = reconcilePredecessor(root, td, req, supervisorOptions, initialSupervisorExecutable)
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

func reconcilePredecessor(root string, td *taskdir.TaskDir, req *task.TaskRecord, supervisorOptions pueue.Options, initialSupervisorExecutable string) (*taskdir.TaskInspection, error) {
	_, meta, spec, metaHash, err := requestHashes(td)
	if err != nil {
		return nil, err
	}
	var response Response
	observation, err := reconcileStatus(root, td, req, meta, spec, metaHash, supervisorOptions, initialSupervisorExecutable, &response)
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
