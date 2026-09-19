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
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type runnerArguments struct {
	Root       string
	TaskID     string
	JSON       bool
	Inspection bool
}

// RunRunner executes one saved task from the supervisor's exact root/task
// command. It shares the public runtime and never accepts a provider argv.
func RunRunner(args []string, stdout, stderr io.Writer, deps Dependencies) int {
	parsed, err := parseRunnerArguments(args)
	if err != nil {
		return writeCLIError(stderr, err, 2)
	}
	if parsed.Inspection {
		if err = runInspectionWorker(parsed.Root, parsed.TaskID, deps.normalized()); err != nil {
			return writeCLIError(stderr, errors.New("native inspection unavailable"), 1)
		}
		return 0
	}
	result := runRunner(parsed, deps.normalized())
	if parsed.JSON {
		result.response.setError(result.err)
		if err = writeJSON(stdout, result.response); err != nil {
			return 1
		}
	} else {
		if err = writeHumanResponse(stdout, result.response); err != nil {
			return 1
		}
		if result.err != nil {
			if _, err = fmt.Fprintf(stderr, "error: %v\n", result.err); err != nil {
				return 1
			}
		}
	}
	return result.code
}

func parseRunnerArguments(args []string) (runnerArguments, error) {
	var parsed runnerArguments
	fs := flag.NewFlagSet("delegate-run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&parsed.Root, "root", "", "absolute state root")
	fs.BoolVar(&parsed.JSON, "json", false, "emit versioned JSON")
	fs.BoolVar(&parsed.Inspection, "inspection", false, "run the saved internal inspection operation")
	ordered, err := orderRunnerFlags(fs, args)
	if err != nil {
		return runnerArguments{}, err
	}
	if err = fs.Parse(ordered); err != nil {
		return runnerArguments{}, err
	}
	positional := fs.Args()
	if len(positional) != 1 {
		return runnerArguments{}, errors.New("delegate-run requires exactly one task ID")
	}
	parsed.TaskID = positional[0]
	if err = task.ValidateTaskID(parsed.TaskID); err != nil {
		return runnerArguments{}, err
	}
	if parsed.Root == "" || !filepath.IsAbs(parsed.Root) || filepath.Clean(parsed.Root) != parsed.Root {
		return runnerArguments{}, errors.New("delegate-run requires a clean absolute --root")
	}
	return parsed, nil
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
	releaseErr := releaseContinuationSession(td, req, outcome)
	if err := errors.Join(cleanupErr, collectErr, releaseErr); err != nil {
		return failed(response, err, 1), true
	}
	return commandResult{response: response, code: 0}, true
}

// releaseContinuationSession rotates a predecessor session only after the
// current task has a validated terminal winner and its runner lease is gone.
// A nil outcome deliberately retains the claim because there is no durable
// evidence that permits a successor to continue the conversation.
func releaseContinuationSession(td *taskdir.TaskDir, req *task.TaskRecord, outcome *task.OutcomeRecord) error {
	if td == nil || req == nil || req.PriorSession == nil || outcome == nil {
		return nil
	}
	return td.ReleaseSession(req.Provider, req.PriorSession.ConversationID, outcome.EvidenceSHA256)
}

func reconcileRunner(root string, response *Response, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisorOptions pueue.Options) (*pueue.Client, pueue.Observation, error) {
	submit, err := td.ReadSubmission()
	if err != nil {
		return nil, pueue.Observation{}, err
	}
	client, err := newSupervisorClient(root, submit.Supervisor, supervisorOptions, true)
	if err != nil {
		return nil, pueue.Observation{}, err
	}
	spec, metaHash, err := preparedHashes(td)
	if err != nil {
		return nil, pueue.Observation{}, err
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return nil, pueue.Observation{}, receiptErr
	}
	observation, reconcileErr := client.Reconcile(context.Background(), taskIdentity(req, meta, spec, metaHash, receipt))
	response.Supervisor = supervisorResponse(observation)
	response.Liveness = mapPueueState(observation.State)
	response.Pending = pendingResponse(observation)
	if !observation.Matched {
		response.Admission = task.AdmissionUnknown.String()
		if reconcileErr == nil {
			reconcileErr = pueue.ErrUnknown
		}
		return client, observation, reconcileErr
	}
	response.Admission = task.AdmissionAdmitted.String()
	positive, err := observation.Receipt()
	if err != nil {
		return client, observation, err
	}
	reconcileErr = errors.Join(reconcileErr, td.RecordSupervisorReceipt(*positive))
	return client, observation, reconcileErr
}

func runProvider(response Response, td *taskdir.TaskDir, req *task.TaskRecord, profile PreparedProfile, client *pueue.Client, deps Dependencies) commandResult {
	stopper := &budgetStopper{client: client, taskDir: td}
	identityObserver, err := makeIdentityObserver(profile, *req, td)
	if err != nil {
		return failed(response, err, 1)
	}
	admittedReq, admittedMeta, err := td.PreparedRecords()
	if err != nil {
		return failed(response, err, 1)
	}
	preflight := func(scope execution.PreflightScope) error {
		root := filepath.Dir(filepath.Dir(td.Dir))
		return freshPreflightProfile(deps, root, *admittedReq, *admittedMeta, scope)
	}
	permit, err := td.PrepareStart(0)
	if err != nil {
		return failed(response, err, 1)
	}
	runResult := execution.Run(td, permit, profile.Plan, execution.Options{Preflight: preflight, Stopper: stopper, Identity: identityObserver, Hooks: deps.ExecutionHooks})
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
	releaseErr := releaseContinuationSession(td, req, runResult.Outcome)
	combinedErr := errors.Join(combinedExecutionErrorValue(runResult), releaseErr)
	if combinedErr != nil {
		return failed(response, combinedErr, 1)
	}
	return commandResult{response: response, code: 0}
}

func runRunner(parsed runnerArguments, deps Dependencies) (result commandResult) {
	response := newResponse("delegate-run")
	root, err := resolveRoot(parsed.Root)
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
	supervisorOptions := supervisorOptionsForCurrentEnvironment(deps.SupervisorOptions)
	restoreEnvironment, err := applySavedEnvironment(meta.Environment)
	if err != nil {
		return failed(response, err, 1)
	}
	defer restoreRunnerEnvironment(&result, restoreEnvironment)
	if isTerminal(inspection) {
		releaseErr := releaseContinuationSession(td, req, inspection.Outcome)
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
	_, err = prepareMatchedProfile(deps, root, *req, *meta)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	client, observation, reconcileErr := reconcileRunner(root, &response, td, req, meta, supervisorOptionsForMeta(supervisorOptions, *meta))
	if reconcileErr != nil {
		return failed(response, reconcileErr, 1)
	}
	if stateErr := runnerStartStateError(observation.State); stateErr != nil {
		return failed(response, stateErr, 1)
	}
	profile, err := prepareMatchedProfile(deps, root, *req, *meta)
	if err != nil {
		return failed(response, err, classifyCode(err, 1))
	}
	return runProvider(response, td, req, profile, client, deps)
}

func applySavedEnvironment(values []string) (func() error, error) {
	if len(values) == 0 {
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
	if restoreErr := restore(); restoreErr != nil {
		result.err = errors.Join(result.err, restoreErr)
		if result.code == 0 {
			result.code = 1
		}
		result.response.setError(restoreErr)
	}
}

// prepareMatchedProfile refreshes the provider's compiled launch profile and
// checks it against the immutable admission record. Callers use it immediately
// before any fresh submission and again after supervisor reconciliation, so a
// policy or placement change cannot cross the next authority boundary.
func prepareMatchedProfile(deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord) (PreparedProfile, error) {
	candidate, facts, err := prepareExistingCandidate(deps, root, req, meta)
	if err != nil {
		return PreparedProfile{}, err
	}
	profile, err := finalizeCandidate(candidate, req, facts)
	if err != nil {
		return PreparedProfile{}, err
	}
	if err = profile.Matches(req, meta); err != nil {
		return PreparedProfile{}, err
	}
	if err = profile.ValidateStatePlacement(root); err != nil {
		return PreparedProfile{}, err
	}
	return profile, nil
}

func runnerStartStateError(state pueue.State) error {
	switch state {
	case pueue.StateQueued, pueue.StateRunning:
		return nil
	case pueue.StateEnded:
		return errors.New("supervisor task ended before runner start")
	case pueue.StateUnknown:
		return fmt.Errorf("%w: supervisor task state unknown before runner start", pueue.ErrUnknown)
	default:
		return fmt.Errorf("%w: unsupported supervisor task state %q before runner start", pueue.ErrUnknown, state)
	}
}

func preparedHashes(td *taskdir.TaskDir) (string, string, error) {
	_, _, spec, metaHash, err := requestHashes(td)
	return spec, metaHash, err
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
	client  *pueue.Client
	taskDir *taskdir.TaskDir
}

func (s *budgetStopper) PrepareBudget(deadline time.Time) (execution.BudgetRequest, error) {
	permit, request, err := s.prepareStop(deadline)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		requestCtx := execution.DetachBudgetRequestContext(ctx)
		result, stopErr := s.client.Stop(requestCtx, permit)
		releaseErr := permit.Release()
		return errors.Join(stopErr, releaseErr, s.recordStopResult(requestCtx, request, result))
	}, nil
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

func combinedExecutionError(result execution.Result) string {
	err := combinedExecutionErrorValue(result)
	if err == nil {
		return ""
	}
	return err.Error()
}

func combinedExecutionErrorValue(result execution.Result) error {
	return errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError)
}
