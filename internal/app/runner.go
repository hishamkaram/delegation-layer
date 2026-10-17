package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

type runnerArguments struct {
	Root               string
	TaskID             string
	JSON               bool
	Inspection         bool
	WorkspaceOperation bool
	Supervised         bool
	Guardian           bool
	QueueMaintenance   bool
	LifetimeChild      bool
	LifetimeFD         int
	TerminalEvent      bool
	PueueID            int64
	ExitCode           string
}

// RunRunner executes one saved task from the supervisor's exact root/task
// command. It shares the public runtime and never accepts a provider argv.
func RunRunner(args []string, stdout, stderr io.Writer, deps Dependencies) int {
	if code, handled := pueue.RunControlOwnerMode(args); handled {
		return code
	}
	if code, handled := lifetime.RunProcessGroupPinMode(args); handled {
		return code
	}
	if code, handled := runLifetimeLaunchGateMode(args, stderr); handled {
		return code
	}
	parsed, err := parseRunnerArguments(args)
	if err != nil {
		return writeCLIError(stderr, err, 2)
	}
	if code, handled := runRunnerControlMode(parsed, stderr, deps); handled {
		return code
	}
	return runRunnerOutput(parsed, stdout, stderr, deps.normalized())
}

func runRunnerControlMode(parsed runnerArguments, stderr io.Writer, deps Dependencies) (int, bool) {
	switch {
	case parsed.QueueMaintenance:
		if err := runQueueMaintenance(parsed, deps.normalized()); err != nil {
			return writeCLIError(stderr, errors.New("supervisor maintenance unavailable"), 1), true
		}
		return 0, true
	case parsed.TerminalEvent:
		if err := runTerminalEvent(parsed, deps.normalized()); err != nil {
			return writeCLIError(stderr, errors.New("terminal event unavailable"), 1), true
		}
		return 0, true
	case parsed.Guardian:
		if err := runLifetimeGuardianMode(parsed, deps.normalized()); err != nil {
			return writeCLIError(stderr, errors.New("task lifetime unavailable"), 1), true
		}
		return 0, true
	case parsed.Inspection:
		if err := runInspectionRunnerMode(parsed, deps.normalized()); err != nil {
			return writeCLIError(stderr, inspectionWorkerPublicError(err), 1), true
		}
		return 0, true
	case parsed.WorkspaceOperation:
		if err := runWorkspaceOperationWorker(parsed, deps.normalized()); err != nil {
			return writeCLIError(stderr, err, 1), true
		}
		return 0, true
	default:
		return 0, false
	}
}

func runInspectionRunnerMode(parsed runnerArguments, deps Dependencies) error {
	if parsed.LifetimeChild {
		return runInspectionLifetimeChildMode(parsed, deps)
	}
	return runInspectionWorkerMode(parsed.Root, parsed.TaskID, parsed.Supervised, deps)
}

func runRunnerOutput(parsed runnerArguments, stdout, stderr io.Writer, deps Dependencies) int {
	result := runRunner(parsed, deps)
	if parsed.JSON {
		result.response.setError(result.err)
		if err := writeJSON(stdout, result.response); err != nil {
			return 1
		}
		return result.code
	}
	if err := writeHumanResponse(stdout, result.response); err != nil {
		return 1
	}
	if result.err != nil {
		if _, err := fmt.Fprintf(stderr, "error: %v\n", result.err); err != nil {
			return 1
		}
	}
	return result.code
}

func parseRunnerArguments(args []string) (runnerArguments, error) {
	parsed := runnerArguments{LifetimeFD: -1}
	fs := newRunnerFlagSet(&parsed)
	ordered, err := orderRunnerFlags(fs, args)
	if err != nil {
		return runnerArguments{}, err
	}
	if err = fs.Parse(ordered); err != nil {
		return runnerArguments{}, err
	}
	return validateRunnerArguments(parsed, fs.Args())
}

func newRunnerFlagSet(parsed *runnerArguments) *flag.FlagSet {
	fs := flag.NewFlagSet("delegate-run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&parsed.Root, "root", "", "absolute state root")
	fs.BoolVar(&parsed.JSON, "json", false, "emit versioned JSON")
	fs.BoolVar(&parsed.Inspection, "inspection", false, "run the saved internal inspection operation")
	fs.BoolVar(&parsed.WorkspaceOperation, "workspace-operation", false, "run the saved internal workspace operation")
	fs.BoolVar(&parsed.Supervised, "supervised", false, "consume a saved supervisor receipt")
	fs.BoolVar(&parsed.Guardian, "guardian", false, "own the saved task process lifetime")
	fs.BoolVar(&parsed.QueueMaintenance, "queue-maintenance", false, "own requested private queue maintenance")
	fs.BoolVar(&parsed.LifetimeChild, "lifetime-child", false, "run behind the saved task guardian")
	fs.IntVar(&parsed.LifetimeFD, "lifetime-fd", -1, "private guardian control descriptor")
	fs.BoolVar(&parsed.TerminalEvent, "terminal-event", false, "record one private supervisor terminal event")
	fs.Int64Var(&parsed.PueueID, "pueue-id", -1, "private supervisor numeric task ID")
	fs.StringVar(&parsed.ExitCode, "exit-code", "", "private supervisor terminal exit code")
	return fs
}

func validateRunnerArguments(parsed runnerArguments, positional []string) (runnerArguments, error) {
	if parsed.Root == "" || !filepath.IsAbs(parsed.Root) || filepath.Clean(parsed.Root) != parsed.Root {
		return runnerArguments{}, errors.New("delegate-run requires a clean absolute --root")
	}
	if parsed.TerminalEvent {
		if runnerTerminalEventHasUnexpectedArguments(parsed, positional) {
			return runnerArguments{}, errors.New("delegate-run terminal event requires --root, --pueue-id and --exit-code only")
		}
		return parsed, nil
	}
	if parsed.QueueMaintenance {
		if runnerQueueMaintenanceHasUnexpectedArguments(parsed, positional) {
			return runnerArguments{}, errors.New("delegate-run queue maintenance requires --root only")
		}
		return parsed, nil
	}
	if len(positional) != 1 {
		return runnerArguments{}, errors.New("delegate-run requires exactly one task ID")
	}
	parsed.TaskID = positional[0]
	if err := task.ValidateTaskID(parsed.TaskID); err != nil {
		return runnerArguments{}, err
	}
	if err := validateRunnerModes(parsed); err != nil {
		return runnerArguments{}, err
	}
	return parsed, nil
}

func runnerTerminalEventHasUnexpectedArguments(parsed runnerArguments, positional []string) bool {
	if parsed.Inspection || parsed.WorkspaceOperation || parsed.Supervised || parsed.Guardian || parsed.QueueMaintenance || parsed.LifetimeChild {
		return true
	}
	if parsed.LifetimeFD != -1 || parsed.JSON || len(positional) != 0 {
		return true
	}
	return parsed.PueueID < 0 || parsed.ExitCode == ""
}

func runnerQueueMaintenanceHasUnexpectedArguments(parsed runnerArguments, positional []string) bool {
	if parsed.Inspection || parsed.WorkspaceOperation || parsed.Supervised || parsed.Guardian || parsed.TerminalEvent || parsed.LifetimeChild {
		return true
	}
	return parsed.LifetimeFD != -1 || parsed.JSON || parsed.PueueID >= 0 || parsed.ExitCode != "" || len(positional) != 0
}

func validateRunnerModes(parsed runnerArguments) error {
	if parsed.WorkspaceOperation && (parsed.Inspection || !parsed.Supervised) {
		return errors.New("delegate-run workspace operation requires its own supervised mode")
	}
	if parsed.Guardian && parsed.LifetimeChild {
		return errors.New("delegate-run guardian and lifetime child modes are mutually exclusive")
	}
	if (parsed.Guardian || parsed.LifetimeChild) && !parsed.Supervised {
		return errors.New("delegate-run lifetime modes require --supervised")
	}
	if parsed.LifetimeChild != (parsed.LifetimeFD >= 3) {
		return errors.New("delegate-run lifetime child requires one private control descriptor")
	}
	return nil
}

func orderRunnerFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	options, positional := make([]string, 0, len(args)), make([]string, 0, 1)
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, _, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		field := fs.Lookup(name)
		if field == nil {
			return nil, fmt.Errorf("unknown flag %q", arg)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate flag --%s", name)
		}
		seen[name] = true
		options = append(options, arg)
		boolean, isBoolean := field.Value.(interface{ IsBoolFlag() bool })
		if assigned || (isBoolean && boolean.IsBoolFlag()) {
			continue
		}
		i++
		if i == len(args) {
			return nil, fmt.Errorf("missing value for --%s", name)
		}
		options = append(options, args[i])
	}
	options = append(options, "--")
	return append(options, positional...), nil
}

func recoverSealedTask(response Response, td *taskdir.TaskDir, inspection *taskdir.TaskInspection, req *task.TaskRecord, meta *task.MetaRecord) (commandResult, bool) {
	if !inspection.SealExists {
		return commandResult{}, false
	}
	// A valid seal is recoverable without another provider Start. Collection
	// resolves the exact registered predicate and never needs the profile.
	outcome, cleanupErr, collectErr := td.Collect(meta.Predicate)
	if outcome == nil {
		return failed(response, errors.Join(cleanupErr, collectErr, task.ErrNoSeal), 1), true
	}
	response.Outcome = outcome
	response.Payload = &outcome.Payload
	response.EvidenceSHA256 = outcome.EvidenceSHA256
	if outcome.Verdict == task.VerdictCommitted {
		response.Publication = task.PublicationCommitted.String()
	} else {
		response.Publication = task.PublicationRejected.String()
	}
	releaseErr := tasklifecycle.ReleaseContinuationSession(td, req, outcome)
	if err := errors.Join(cleanupErr, collectErr, releaseErr); err != nil {
		return failed(response, err, 1), true
	}
	return commandResult{response: response, code: 0}, true
}

func runProvider(response Response, td *taskdir.TaskDir, req *task.TaskRecord, profile PreparedProfile, client *pueue.Client, lifetimeChild *openedLifetimeChild, deps Dependencies) commandResult {
	stopper := &budgetStopper{client: client, taskDir: td, lifetime: lifetimeChild.Lifetime()}
	identityObserver, err := makeIdentityObserver(profile, *req, td)
	if err != nil {
		return failed(response, err, 1)
	}
	admittedReq, admittedMeta, err := td.PreparedRecords()
	if err != nil {
		return failed(response, err, 1)
	}
	preflight := func(scope execution.PreflightScope, _ execution.Plan) (execution.Plan, error) {
		root := filepath.Dir(filepath.Dir(td.Dir))
		return freshPreflightProfile(deps, root, *admittedReq, *admittedMeta, scope)
	}
	permit, err := td.PrepareStart(0)
	if err != nil {
		return failed(response, err, 1)
	}
	var interaction execution.InteractionProtocol
	if profile.Interaction != nil {
		interaction, err = profile.Interaction(continuationExpectation(req), func(identity task.SessionIdentity) error { return td.RecordProviderIdentity(identity) })
		if err != nil {
			return failed(response, errors.Join(err, permit.Release()), 1)
		}
	}
	runResult := execution.Run(td, permit, profile.Plan, execution.Options{
		Interaction: interaction, PreflightPlan: preflight, Stopper: stopper,
		Lifetime: lifetimeChild.Lifetime(), Deadline: lifetimeChild.Deadline(),
		Identity: identityObserver, Hooks: deps.ExecutionHooks,
	})
	if runResult.Outcome != nil {
		response.Outcome = runResult.Outcome
		response.Payload = &runResult.Outcome.Payload
		response.EvidenceSHA256 = runResult.Outcome.EvidenceSHA256
		if runResult.Outcome.Verdict == task.VerdictCommitted {
			response.Publication = task.PublicationCommitted.String()
		} else {
			response.Publication = task.PublicationRejected.String()
		}
	}
	releaseErr := tasklifecycle.ReleaseContinuationSession(td, req, runResult.Outcome)
	combinedErr := errors.Join(combinedExecutionErrorValue(runResult), releaseErr)
	if combinedErr != nil {
		return failed(response, combinedErr, 1)
	}
	return commandResult{response: response, code: 0}
}

func runRunner(parsed runnerArguments, deps Dependencies) (result commandResult) {
	response := newResponse("delegate-run")
	root, err := tasklifecycle.ResolveRoot(parsed.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(parsed.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	var lifetimeChild *openedLifetimeChild
	if parsed.LifetimeChild {
		lifetimeChild, err = openTaskLifetimeChild(parsed, td)
		if err != nil {
			return failed(response, err, 1)
		}
		defer func() { mergeCommandClose(&result, lifetimeChild.Close) }()
	}
	return runPreparedRunner(response, parsed, deps, root, store, td, lifetimeChild)
}

func runPreparedRunner(response Response, parsed runnerArguments, deps Dependencies, root string, store *taskdir.Store, td *taskdir.TaskDir, lifetimeChild *openedLifetimeChild) (result commandResult) {
	response.RootID, response.TaskID = store.RootID, parsed.TaskID
	inspection, err := td.Inspect()
	fillInspection(&response, inspection)
	if err != nil {
		return failed(response, err, 1)
	}
	req, meta, err := td.PreparedRecords()
	if err != nil {
		return failed(response, err, 1)
	}
	supervisorOptions := supervisor.OptionsForCurrentEnvironment(deps.SupervisorOptions)
	restoreEnvironment, err := applySavedEnvironment(meta.Environment, meta.EnvironmentRecorded)
	if err != nil {
		return failed(response, err, 1)
	}
	defer restoreRunnerEnvironment(&result, restoreEnvironment)
	if supervisor.TaskTerminal(inspection) {
		releaseErr := tasklifecycle.ReleaseContinuationSession(td, req, inspection.Outcome)
		if releaseErr != nil {
			return failed(response, releaseErr, 1)
		}
		return commandResult{response: response, code: 0}
	}
	if recovered, handled := recoverSealedTask(response, td, inspection, req, meta); handled {
		return recovered
	}
	if inspection.StartExists {
		return failed(response, task.ErrAlreadyStarted, 1)
	}
	if req.Workspace != nil && lifetimeChild == nil {
		return failed(response, fmt.Errorf("%w: a managed workspace requires an authenticated lifetime child", task.ErrInvalidPermit), 1)
	}
	_, err = prepareMatchedProfile(deps, root, *req, *meta)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	taskSupervisorOptions := supervisorOptionsForMeta(supervisorOptions, *meta)
	supervisorContext, cancelSupervisor := tasklifecycle.SupervisorContextForRunner(req, taskSupervisorOptions)
	defer cancelSupervisor()
	client, observation, reconcileErr := reconcileRunnerMode(supervisorContext, parsed, root, &response, td, req, meta, taskSupervisorOptions, deps.InitialSupervisorExecutable)
	defer func() { supervisor.CloseMaintenance(client, result.err) }()
	if reconcileErr != nil {
		return failed(response, reconcileErr, 1)
	}
	if stateErr := tasklifecycle.RunnerStartStateError(observation.State); stateErr != nil {
		return failed(response, stateErr, 1)
	}
	profile, err := prepareRunnerProfile(deps, root, td, store, *req, *meta)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	return runProvider(response, td, req, profile, client, lifetimeChild, deps)
}

func reconcileRunnerMode(ctx context.Context, parsed runnerArguments, root string, response *Response, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisorOptions pueue.Options, initialSupervisorExecutable string) (*pueue.Client, pueue.Observation, error) {
	if parsed.Supervised {
		return resumeSupervisedRunner(ctx, response, td, supervisorOptions)
	}
	return reconcileRunner(ctx, root, response, td, req, meta, supervisorOptions, initialSupervisorExecutable)
}

func applySavedEnvironment(values []string, recorded bool) (func() error, error) {
	if len(values) == 0 && !recorded {
		return func() error { return nil }, nil
	}
	if err := task.ValidateEnvironment(values); err != nil {
		return nil, err
	}
	original := os.Environ()
	os.Clearenv()
	for _, entry := range values {
		key, value, _ := strings.Cut(entry, "=")
		if err := os.Setenv(key, value); err != nil {
			return nil, errors.Join(err, restoreEnvironment(original))
		}
	}
	return func() error { return restoreEnvironment(original) }, nil
}

func restoreEnvironment(values []string) (resultErr error) {
	os.Clearenv()
	for _, entry := range values {
		key, value, _ := strings.Cut(entry, "=")
		resultErr = errors.Join(resultErr, os.Setenv(key, value))
	}
	return resultErr
}

func restoreRunnerEnvironment(result *commandResult, restore func() error) {
	if result == nil || restore == nil {
		return
	}
	applyEnvironmentRestoreError(result, restore())
}

func applyEnvironmentRestoreError(result *commandResult, restoreErr error) {
	if restoreErr != nil {
		result.err = errors.Join(result.err, restoreErr)
		if result.code == 0 {
			result.code = 1
		}
		result.response.setError(restoreErr)
	}
}

func prepareMatchedProfile(deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord) (PreparedProfile, error) {
	return admissionProfiles(deps).Match(root, req, meta)
}

func prepareRunnerProfile(deps Dependencies, root string, td *taskdir.TaskDir, store *taskdir.Store, req task.TaskRecord, meta task.MetaRecord) (PreparedProfile, error) {
	runnerOwnership, err := supervisor.RunnerOwnershipForTask(root, td, &meta)
	if err != nil {
		return PreparedProfile{}, err
	}
	return prepareMatchedProfileForRunner(deps, root, req, meta, store, "", runnerOwnership)
}

func makeIdentityObserver(profile PreparedProfile, req task.TaskRecord, td *taskdir.TaskDir) (execution.IdentityObserver, error) {
	if profile.Identity == nil {
		return nil, nil
	}
	return profile.Identity(continuationExpectation(&req), func(identity task.SessionIdentity) error {
		return td.RecordProviderIdentity(identity)
	})
}

type budgetStopper struct {
	client   *pueue.Client
	taskDir  *taskdir.TaskDir
	lifetime execution.Lifetime
}

func (s *budgetStopper) PrepareBudget(deadline time.Time) (execution.BudgetRequest, error) {
	permit, request, err := s.prepareStop(deadline)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		requestCtx := execution.DetachBudgetRequestContext(ctx)
		if s.lifetime != nil {
			return s.stopLifetime(requestCtx, permit, request)
		}
		result, stopErr := s.client.Stop(requestCtx, permit)
		releaseErr := permit.Release()
		return errors.Join(stopErr, releaseErr, s.recordStopResult(requestCtx, request, result))
	}, nil
}

func (s *budgetStopper) stopLifetime(ctx context.Context, permit *taskdir.StopPermit, request *task.StopRequestRecord) error {
	if permit == nil || request == nil || request.NumericTaskID == nil {
		return errors.Join(task.ErrInvalidPermit, permit.Release())
	}
	if err := permit.Consume(); err != nil {
		return errors.Join(err, permit.Release())
	}
	terminateErr := s.lifetime.Terminate(ctx)
	var recordErr error
	if terminateErr == nil {
		facts := task.StopReplyFacts{NumericTaskID: *request.NumericTaskID, Action: "terminate", Acknowledged: true}
		recordErr = errors.Join(recordErr, s.taskDir.RecordStopReplyContext(ctx, request.RequestID, facts))
	}
	return errors.Join(terminateErr, recordErr, permit.Release())
}

func (s *budgetStopper) prepareStop(deadline time.Time) (*taskdir.StopPermit, *task.StopRequestRecord, error) {
	permit, err := s.taskDir.PrepareStop("budget", "budget", deadline)
	if err != nil {
		return nil, nil, err
	}
	request, err := permit.Request()
	if err != nil {
		return permit, nil, errors.Join(err, permit.Release())
	}
	return permit, request, nil
}

func (s *budgetStopper) recordStopResult(ctx context.Context, request *task.StopRequestRecord, result pueue.StopResult) error {
	if request == nil {
		return nil
	}
	var recordErr error
	if result.Acknowledged != nil && result.NumericTaskID != nil {
		recordErr = errors.Join(recordErr, s.taskDir.RecordStopReplyContext(ctx, request.RequestID, task.StopReplyFacts{NumericTaskID: *result.NumericTaskID, Action: result.Action, Acknowledged: *result.Acknowledged, Message: result.Message}))
	}
	if result.ObservedState == pueue.StateEnded && result.NumericTaskID != nil {
		recordErr = errors.Join(recordErr, s.taskDir.RecordStopObservationContext(ctx, request.RequestID, task.StopObservationFacts{NumericTaskID: *result.NumericTaskID, State: "ended", Terminated: true}))
	}
	return recordErr
}

func combinedExecutionErrorValue(result execution.Result) error {
	return errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError)
}
