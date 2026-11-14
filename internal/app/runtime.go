package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/app/infrastructurehost"
	"github.com/hishamkaram/delegation-layer/internal/gatewaycommand"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/execution"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// Dependencies is the explicit composition boundary for production and the
// hermetic acceptance harness. Production leaves hooks and profile selection at
// their safe defaults; acceptance supplies a compiled finite profile directly.
type Dependencies struct {
	Gateway *gatewaycommand.Dependencies
	// ReportDiagnostic is invocation-scoped; nil preserves CLI logging.
	ReportDiagnostic func(error)
	remoteRootPin    *taskdir.RootPin
	// TeamCatalogPath and Input are explicit command-edge dependencies. An empty
	// catalog path is resolved from the captured user environment only on demand.
	TeamCatalogPath string
	Input           io.Reader
	// Catalog is the immutable provider composition for this command. When it
	// is empty, normalized constructs the production catalog once.
	Catalog                     commonprovider.Catalog
	PrepareProfile              PrepareProfile
	PrepareCandidate            commonprovider.PrepareCandidate
	PredicateRegistry           func() predicate.Registry
	SupervisorOptions           pueue.Options
	InitialSupervisorExecutable string
	RunnerExecutable            string
	PublisherVersion            string
	ExecutionHooks              execution.Hooks
	// ResourceChecker checks fresh dispatch and continue admissions. Nil
	// selects the native checker; fixtures inject a deterministic one.
	ResourceChecker tasklifecycle.ResourceChecker
	// Environment is the caller's launch-context snapshot. Nil means it was
	// not captured yet: the first command entrypoint captures os.Environ once
	// and every later consumer reads that snapshot. A nonnil empty slice is an
	// explicit empty environment and is kept.
	Environment      []string
	invocationSource *infrastructurehost.Invocation
	// ownerRouting is an immutable command composition choice. Nil selects
	// native routing; component tests can lend their existing lifecycle owner.
	ownerRouting       supervisorcore.OwnerRouting
	useCatalogExisting bool
}

// withCapturedEnvironment binds actual invocation provenance before selecting
// the launch environment. Dependency copies retain that immutable observation,
// including a lazy capture error; launch scopes never replace its authority.
func (d Dependencies) withCapturedEnvironment() Dependencies {
	return d.withCapturedEnvironmentContext(context.Background())
}

func (d Dependencies) withCapturedEnvironmentContext(ctx context.Context) Dependencies {
	if d.invocationSource == nil {
		d.invocationSource = infrastructurehost.CaptureInvocation(ctx)
	}
	if d.Environment == nil {
		d.Environment = os.Environ()
	}
	return d
}

// storeDependencies is the narrow composition passed to read-only task-store
// commands. Profile selection, runner resolution, and supervisor mutation
// options remain outside this boundary. environment is a read-only copy of the
// command's captured launch scope, with the same nil and empty meaning.
type storeDependencies struct {
	reportDiagnostic  func(error)
	remoteRootPin     *taskdir.RootPin
	predicateRegistry func() predicate.Registry
	catalog           commonprovider.Catalog
	supervisorOptions pueue.Options
	supervisorLookup  installation.ExecutableLookup
	runnerExecutable  string
	environment       []string
	invocationSource  *infrastructurehost.Invocation
	ownerRouting      supervisorcore.OwnerRouting
	observeSupervisor bool
}

func (d Dependencies) storeDependencies() storeDependencies {
	registry := d.PredicateRegistry
	if registry == nil && !d.Catalog.IsZero() {
		registry = d.Catalog.Registry
	}
	return storeDependencies{
		reportDiagnostic:  d.ReportDiagnostic,
		predicateRegistry: registry,
		remoteRootPin:     d.remoteRootPin,
		catalog:           d.Catalog,
		supervisorOptions: d.supervisorControlScope(),
		supervisorLookup:  d.supervisorLookup(),
		runnerExecutable:  d.RunnerExecutable,
		environment:       slices.Clone(d.Environment),
		invocationSource:  d.invocationSource,
		ownerRouting:      d.ownerRouting,
		observeSupervisor: true,
	}
}

// supervisorControlScope binds supervisor control for observation and stop
// commands to this composition's captured launch scope. An uncaptured
// composition keeps pueue's standalone per-invocation default.
func (d Dependencies) supervisorControlScope() pueue.Options {
	if d.ReportDiagnostic != nil {
		d.SupervisorOptions.ReportDiagnostic = d.ReportDiagnostic
	}
	if d.Environment == nil {
		return d.SupervisorOptions
	}
	return supervisorcore.OptionsForEnvironment(d.SupervisorOptions, d.Environment)
}

// supervisorLookup locates an unbound supervisor pair in this composition's
// launch scope.
func (d Dependencies) supervisorLookup() installation.ExecutableLookup {
	return installation.ExecutableLookup{InitialExecutable: d.InitialSupervisorExecutable, Environment: slices.Clone(d.Environment)}
}

func (d Dependencies) normalized() Dependencies {
	if d.Catalog.IsZero() {
		d.Catalog = NativeCatalog()
	}
	if d.PrepareCandidate == nil {
		if d.PrepareProfile != nil {
			d.PrepareCandidate = commonprovider.ReadyCandidate(d.PrepareProfile)
		} else {
			d.PrepareCandidate = d.Catalog.Candidate
			d.useCatalogExisting = true
		}
	}
	if d.PredicateRegistry == nil {
		d.PredicateRegistry = d.Catalog.Registry
	}
	if d.PublisherVersion == "" {
		d.PublisherVersion = defaultPublisher
	}
	if d.SupervisorOptions.ControlOwnerExecutable == "" {
		d.SupervisorOptions.ControlOwnerExecutable = installation.DiscoverControlOwnerExecutable(d.RunnerExecutable)
	}
	if d.ReportDiagnostic != nil {
		d.SupervisorOptions.ReportDiagnostic = d.ReportDiagnostic
	}
	d.SupervisorOptions = nativeInfrastructureCallbacksForReporter(d.ReportDiagnostic).ApplyTo(d.SupervisorOptions)
	return d
}

// Run parses and executes one public delegate command.
func Run(args []string, stdout, stderr io.Writer, deps Dependencies) int {
	if code, handled := execution.RunCommandOwnerMode(args); handled {
		return code
	}
	deps = deps.withCapturedEnvironment()
	parsed, err := ParseArguments(args)
	if err != nil {
		return argumentUsageErrorFor(args, stderr, err)
	}
	if parsed.Command == "help" {
		return presentHelp(parsed, stdout, stderr)
	}
	if parsed.Command == "version" {
		if _, err = fmt.Fprintf(stdout, "delegate %s\n", deps.normalized().PublisherVersion); err != nil {
			return 1
		}
		return 0
	}
	if parsed.Command == "gateway" {
		return runGatewayCLI(context.Background(), parsed, deps, stdout, stderr)
	}
	if isRemoteCommand(parsed) {
		return runRemoteCLI(parsed, deps, stdout, stderr)
	}
	if parsed.Command == "teams" {
		return runTeams(parsed, deps, stdout, stderr)
	}

	normalized := deps
	if parsed.Command != "schedule" {
		normalized = deps.normalized()
	}
	if parsed.Command == "doctor" {
		return runDoctor(parsed, normalized, stdout, stderr)
	}
	if parsed.Command == "providers" {
		return runProviders(parsed.JSON, stdout, stderr, normalized.Catalog)
	}
	if parsed.Command == "capabilities" {
		return runCapabilities(parsed.JSON, stdout, stderr, normalized.Catalog, parsed.Provider)
	}
	if parsed.Command == "models" {
		return runModels(parsed, stdout, normalized)
	}
	var result commandResult
	if parsed.Command == "scheduler" {
		result = runSchedulerCommand(context.Background(), parsed, normalized, stderr)
	} else {
		result = runCommand(parsed, normalized)
	}
	return presentCommandResult(parsed, result, stdout, stderr)
}

// RunContext supplies caller cancellation for scheduler hosting and Gateway
// commands. Task invocations already owned by Delegate settle independently.
func RunContext(ctx context.Context, args []string, stdout, stderr io.Writer, deps Dependencies) int {
	parsed, err := ParseArguments(args)
	if err != nil {
		return argumentUsageErrorFor(args, stderr, err)
	}
	if parsed.Command == "help" {
		return presentHelp(parsed, stdout, stderr)
	}
	if parsed.Command == "gateway" {
		return runGatewayCLI(ctx, parsed, deps.withCapturedEnvironmentContext(ctx), stdout, stderr)
	}
	if parsed.Command != "scheduler" {
		return writeCLIError(stderr, errors.New("context execution requires a scheduler command"), 2)
	}
	result := runSchedulerCommand(ctx, parsed, deps.withCapturedEnvironmentContext(ctx).normalized(), stderr)
	return presentCommandResult(parsed, result, stdout, stderr)
}

func presentHelp(parsed Arguments, stdout, stderr io.Writer) int {
	text, err := HelpText(parsed.HelpPath)
	if err != nil {
		return writeCLIError(stderr, err, 2)
	}
	if _, err = io.WriteString(stdout, text); err != nil {
		return 1
	}
	return 0
}

func argumentUsageErrorFor(args []string, stderr io.Writer, err error) int {
	if _, writeErr := fmt.Fprintf(stderr, "error: %v\n\n%s", err, CommandUsage(args)); writeErr != nil {
		return 1
	}
	return 2
}

func presentCommandResult(parsed Arguments, result commandResult, stdout, stderr io.Writer) int {
	result.response = projectConnectorWorkspace(result.response, parsed.Connector.IncludeWorkspaceConstraint || connectorWorkspace(parsed.Connector) != nil)
	if parsed.JSON {
		if err := writeJSONResponse(stdout, parsed.Command, &result.response, result.err); err != nil {
			return scheduleOutputFailure(stderr, result.response, err)
		}
	} else {
		if err := writeHumanResponse(stdout, result.response); err != nil {
			return scheduleOutputFailure(stderr, result.response, err)
		}
		if result.err != nil {
			return writeCLIError(stderr, commandPresentationError(parsed.Command, result), result.code)
		}
	}
	return result.code
}

func scheduleOutputFailure(stderr io.Writer, response Response, err error) int {
	if response.Command == "schedule" && response.Schedule != nil && response.Schedule.ID != "" {
		return writeCLIError(stderr, fmt.Errorf("writing schedule output for %s: %w; inspect with schedule show %s", response.Schedule.ID, err, response.Schedule.ID), 1)
	}
	return 1
}

func runCommand(a Arguments, deps Dependencies) commandResult {
	if usesConnectorControl(a) {
		return runCommandContext(context.Background(), a, deps)
	}
	return runLegacyCommand(a, deps)
}

func runLegacyCommand(a Arguments, deps Dependencies) commandResult {
	switch a.Command {
	case "dispatch":
		return dispatch(a, deps)
	case "preflight":
		return preflight(a, deps)
	case "continue":
		return continueTask(a, deps)
	case "wait":
		return waitTasks(a, deps.storeDependencies())
	case "status":
		return status(a, deps)
	case "collect":
		return collect(a, deps.storeDependencies())
	case "recheck":
		return recheck(a, deps.storeDependencies())
	case "cancel":
		return cancel(a, deps)
	case "reply", "steer", "events":
		return interactionCommand(a, deps.storeDependencies())
	case "logs":
		return logs(a, deps.storeDependencies())
	case "supervisor":
		return supervisorCommand(a, deps)
	case "workspace":
		return workspaceCommand(a, deps)
	case "checkpoint":
		return checkpointCommand(a, deps)
	case "schedule":
		return scheduleCommandWithTeams(a, deps)
	default:
		return commandResult{response: newResponse(a.Command), code: 2, err: fmt.Errorf("unknown command %q", a.Command)}
	}
}

func writeCLIError(w io.Writer, err error, code int) int {
	if err == nil {
		return code
	}
	_, writeErr := fmt.Fprintf(w, "error: %v\n", err)
	if writeErr != nil {
		return 1
	}
	return code
}

func (d storeDependencies) registry() predicate.Registry {
	if d.predicateRegistry != nil {
		return d.predicateRegistry()
	}
	return NativePredicates()
}

func (d storeDependencies) providerCatalog() commonprovider.Catalog {
	if d.catalog.IsZero() {
		return NativeCatalog()
	}
	return d.catalog
}

func openStore(root string, deps storeDependencies, create bool) (*taskdir.Store, error) {
	return openStoreContext(context.Background(), root, deps, create)
}

func openStoreContext(ctx context.Context, root string, deps storeDependencies, create bool) (*taskdir.Store, error) {
	if deps.remoteRootPin != nil {
		create = false
	}
	var store *taskdir.Store
	var err error
	if create {
		store, err = taskdir.InitStoreWithPredicatesContext(ctx, root, deps.registry())
	} else {
		store, err = taskdir.OpenStoreWithPredicatesContext(ctx, root, deps.registry())
	}
	if err != nil {
		return nil, err
	}
	if deps.remoteRootPin != nil {
		pin, pinErr := store.PinRootContext(ctx)
		if pinErr == nil && !pin.Equal(*deps.remoteRootPin) {
			pinErr = task.ErrIdentityMismatch
		}
		if pinErr != nil {
			return nil, errors.Join(pinErr, store.Close())
		}
	}
	return store, nil
}

func bindInitialForAdmission(a Arguments, deps Dependencies, root string, supervisorOptions pueue.Options, deferPrivateStart bool) (*pueue.Client, error) {
	return bindInitialWithStart(a, deps, root, supervisorOptions, !deferPrivateStart)
}

func bindInitialWithStart(a Arguments, deps Dependencies, root string, supervisorOptions pueue.Options, startPrivate bool) (*pueue.Client, error) {
	return supervisorcore.BindInitialWithStart(supervisorAdmissionSelection(a, deps), supervisorAdmissionOptions(deps, supervisorOptions), root, startPrivate)
}

func continuationExpectation(req *task.TaskRecord) task.SessionExpectation {
	if req == nil || req.PriorSession == nil {
		return task.SessionExpectation{}
	}
	return task.SessionExpectation{Required: true, ID: req.PriorSession.ConversationID}
}

func copyTaskID(id *int64) *int64 {
	if id == nil {
		return nil
	}
	copied := *id
	return &copied
}

func mergeCommandClose(result *commandResult, closeFn func() error) {
	defer func() {
		if result.response.Connector != nil {
			scrubConnectorFailure(result)
		}
	}()
	if err := closeFn(); err != nil {
		result.err = errors.Join(result.err, err)
		result.code = 1
		// The command error owns failure authority. A new close fault
		// invalidates recovery selected before closing; the wire projection
		// must classify the complete error rather than an earlier pending one.
		result.response.Failure = nil
		result.response.setError(result.err)
	}
}
