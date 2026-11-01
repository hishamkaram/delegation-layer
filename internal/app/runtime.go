package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/execution"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// UsageText is the public command synopsis shared by delegate and its tests.
const UsageText = `Usage: delegate [global flags] <command> [command flags]

A durable single-machine agent delegation layer.

Commands:
  dispatch      Persist and submit one bounded provider turn
  providers     Describe compiled provider capabilities
  capabilities  Report one provider capability contract
  models        Discover native model IDs and effort choices
  status        Observe admission, liveness, and publication
  collect       Recover or read the immutable publication
  recheck       Re-evaluate a rejected task from sealed local evidence
  reply         Answer an outstanding native clarification request
  steer         Send guidance to a supported active native turn
  events        Read interaction events, optionally with a bounded watch
  cancel        Request one explicit supervisor stop
  logs          Return validated output descriptors
  preflight     Check one provider without creating a task
  continue      Continue one terminal task through its exact session
  supervisor    Inspect or migrate Delegate's private supervisor
  workspace     Inspect, verify, release, or forget a managed Git workspace
  checkpoint    Create, observe, list, or remove a Git content checkpoint
  schedule      Manage recurring definitions and inspect executions
  scheduler     Serve, inspect, stop, or run the local scheduler
  teams         Register, update, inspect, and explicitly select optional Teams
  help          Show this help message
  version       Print version information

Global flags:
  --root ABS              State root (default: user config directory)
  --pueue-config ABS      Initial supervisor configuration
  --runner ABS            Advanced custom runner override; omit for the automatic runner

Supervisor recovery:
  supervisor status --json
  supervisor migrate --json

Managed Git workspaces (optional):
  dispatch ... --cwd ABS --isolation git-worktree [--base REV]
  dispatch ... --workspace ID
  workspace list [--after ID] [--limit 50] --json
  workspace status ID [--operation OP] [--watch DURATION] --json
  workspace exec ID --budget DURATION -- COMMAND ARGS
  workspace release ID [--discard] [--budget DURATION] --json
  workspace forget ID [--budget DURATION] --json
  --workspace-budget DURATION sets the finite helper budget (default: 5m).
  Source edits are excluded; landing changes belongs to the Lead.

Git checkpoints (optional):
  checkpoint create --cwd ABS [--budget DURATION] --json
  checkpoint create --workspace ID [--budget DURATION] --json
  checkpoint status CHECKPOINT_ID [--operation OP] [--watch DURATION] --json
  checkpoint list [--after CHECKPOINT_ID] [--limit 50] --json
  checkpoint remove CHECKPOINT_ID [--budget DURATION] --json
  Creation names exactly one target. --operation selects an acknowledged
  removal attempt; without it status observes the creation and latest removal.

Recurring definitions:
  schedule create --brief FILE --cwd ABS (--provider ID | --auto) --cron RULE [--timezone ZONE] --json
  schedule list [--after ID] [--limit N] --json
  schedule show|pause|resume|delete ID --json
  schedule history SCHEDULE_ID [--before RFC3339Nano] [--limit 50] --json
  schedule execution TASK_ID --json
  Saving a definition never launches a provider. If no scheduler host is
  ready, the saved ID is retained and start_required gives the user command.
  History and execution are passive reads; collect TASK_ID recovers publication.

Optional Teams:
  teams register --scope user|project --definition FILE [--cwd ABS] --json
  teams show --scope user|project --team ID [--cwd ABS] --json
  teams update --scope user|project --team ID --expected-revision N --definition FILE [--cwd ABS] --json
  teams delete --scope user|project --team ID --expected-revision N [--cwd ABS] --json
  teams list [--cwd ABS] [--limit 50] [--after CURSOR] --json
  teams default show|clear --cwd ABS --json
  teams default set --cwd ABS --team-scope user|project --team ID --json
  dispatch ... --team-scope user|project --team ID --member ID
  schedule create ... --team-scope user|project --team ID --member ID
  Use --definition - for stdin. Outside Git, Local scope also requires
  --project-root ABS. Defaults guide explicit selection; they never auto-route.

Independent scheduler:
  scheduler serve [--root ABS] [--pueue-config ABS]
  scheduler status --json
  scheduler stop --json
  scheduler run [--root ABS] [--pueue-config ABS]
  Requires the automatic managed worker. Use colocated delegate/delegate-run
  binaries from the same build and omit the advanced --runner override.
  Start serve yourself in an independent terminal or tmux pane. It stays in
  the foreground and restarts its scheduler child after unexpected exits.
  Stop ends the host cooperatively. Host loss/reboot requires another serve.
  Run is the compatibility foreground runtime without child recovery.
  Existing admitted tasks continue after the scheduler stops.

Interactive tasks:
  dispatch ... --interactive
  continue --task ID ... --interactive
  reply ID --request REQUEST_ID --answers FILE --id MESSAGE_ID --json
  steer ID --brief FILE --id MESSAGE_ID --json
  events ID [--after SEQUENCE] [--watch DURATION] --json

Use --json on task, providers, capabilities, models, preflight, continue, and recheck commands for the versioned response.
`

// Dependencies is the explicit composition boundary for production and the
// hermetic acceptance harness. Production leaves hooks and profile selection at
// their safe defaults; acceptance supplies a compiled finite profile directly.
type Dependencies struct {
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
	Environment        []string
	useCatalogExisting bool
}

// withCapturedEnvironment binds the caller's launch environment at a public
// entrypoint. Composition reads that one snapshot afterward; it never reads
// the process environment again.
func (d Dependencies) withCapturedEnvironment() Dependencies {
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
	predicateRegistry func() predicate.Registry
	catalog           commonprovider.Catalog
	supervisorOptions pueue.Options
	supervisorLookup  supervisorcore.ExecutableLookup
	runnerExecutable  string
	environment       []string
	observeSupervisor bool
}

func (d Dependencies) storeDependencies() storeDependencies {
	registry := d.PredicateRegistry
	if registry == nil && !d.Catalog.IsZero() {
		registry = d.Catalog.Registry
	}
	return storeDependencies{
		predicateRegistry: registry,
		catalog:           d.Catalog,
		supervisorOptions: d.supervisorControlScope(),
		supervisorLookup:  d.supervisorLookup(),
		runnerExecutable:  d.RunnerExecutable,
		environment:       slices.Clone(d.Environment),
		observeSupervisor: true,
	}
}

// supervisorControlScope binds supervisor control for observation and stop
// commands to this composition's captured launch scope. An uncaptured
// composition keeps pueue's standalone per-invocation default.
func (d Dependencies) supervisorControlScope() pueue.Options {
	if d.Environment == nil {
		return d.SupervisorOptions
	}
	return supervisorcore.OptionsForEnvironment(d.SupervisorOptions, d.Environment)
}

// supervisorLookup locates an unbound supervisor pair in this composition's
// launch scope.
func (d Dependencies) supervisorLookup() supervisorcore.ExecutableLookup {
	return supervisorcore.ExecutableLookup{InitialExecutable: d.InitialSupervisorExecutable, Environment: slices.Clone(d.Environment)}
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
		d.SupervisorOptions.ControlOwnerExecutable = supervisorcore.DiscoverControlOwnerExecutable()
	}
	if d.SupervisorOptions.PrivateMaintenanceLauncher == nil && d.SupervisorOptions.ControlOwnerExecutable != "" {
		d.SupervisorOptions.PrivateMaintenanceLauncher = supervisorcore.LaunchQueueMaintenanceOwnerWithEnvironment
	}
	if d.SupervisorOptions.SuccessfulCleanupAuthorizer == nil {
		d.SupervisorOptions.SuccessfulCleanupAuthorizer = authorizePrivateSuccessfulCleanup
	}
	if d.SupervisorOptions.PrivateTerminalEvidencePreparer == nil {
		d.SupervisorOptions.PrivateTerminalEvidencePreparer = preparePrivateTerminalEvidence
	}
	if d.SupervisorOptions.PrivateTerminalCustodyReleaser == nil {
		d.SupervisorOptions.PrivateTerminalCustodyReleaser = releasePrivateTerminalCustody
	}
	if d.SupervisorOptions.PrivateMigrationAuthorizer == nil {
		d.SupervisorOptions.PrivateMigrationAuthorizer = authorizePrivateMigrationQueue
	}
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
		return argumentUsageError(stderr, err)
	}
	if parsed.Command == "help" {
		if _, err = io.WriteString(stdout, UsageText); err != nil {
			return 1
		}
		return 0
	}
	if parsed.Command == "version" {
		if _, err = fmt.Fprintf(stdout, "delegate %s\n", deps.normalized().PublisherVersion); err != nil {
			return 1
		}
		return 0
	}
	if parsed.Command == "teams" {
		return runTeams(parsed, deps, stdout, stderr)
	}

	normalized := deps
	if parsed.Command != "schedule" {
		normalized = deps.normalized()
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

// RunContext supplies only the resident scheduler's admission/stop lifetime.
// An already-owned task invocation settles independently before shutdown joins.
// Ordinary finite commands keep Run's established lifetime and behavior.
func RunContext(ctx context.Context, args []string, stdout, stderr io.Writer, deps Dependencies) int {
	parsed, err := ParseArguments(args)
	if err != nil {
		return argumentUsageError(stderr, err)
	}
	if parsed.Command != "scheduler" {
		return writeCLIError(stderr, errors.New("context execution requires a scheduler command"), 2)
	}
	result := runSchedulerCommand(ctx, parsed, deps.withCapturedEnvironment().normalized(), stderr)
	return presentCommandResult(parsed, result, stdout, stderr)
}

func argumentUsageError(stderr io.Writer, err error) int {
	if _, writeErr := fmt.Fprintf(stderr, "error: %v\n\n%s", err, UsageText); writeErr != nil {
		return 1
	}
	return 2
}

func presentCommandResult(parsed Arguments, result commandResult, stdout, stderr io.Writer) int {
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
	switch a.Command {
	case "dispatch":
		return dispatch(a, deps)
	case "preflight":
		return preflight(a, deps)
	case "continue":
		return continueTask(a, deps)
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
	registry := deps.registry()
	if create {
		return taskdir.InitStoreWithPredicates(root, registry)
	}
	return taskdir.OpenStoreWithPredicates(root, registry)
}

func openStoreContext(ctx context.Context, root string, deps storeDependencies, create bool) (*taskdir.Store, error) {
	if create {
		return taskdir.InitStoreWithPredicatesContext(ctx, root, deps.registry())
	}
	return taskdir.OpenStoreWithPredicatesContext(ctx, root, deps.registry())
}

func resolveCurrentPrivateSupervisorExecutables(root string, deps Dependencies) (client, daemon string, resultErr error) {
	return supervisorcore.ResolveCurrentPrivateSupervisorExecutables(root, deps.supervisorLookup())
}

// newSupervisorControlClient proves the task's exact saved endpoint ready before
// a mutating queue command. If that endpoint is unavailable, completed private
// migration evidence may recover the current successor before the one control
// command is sent. An uncertain probe and a failed control are never retried.
func newSupervisorControlClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, lookup supervisorcore.ExecutableLookup) (*pueue.Client, error) {
	return supervisorcore.NewControlClient(ctx, root, saved, options, lookup, openNativeSupervisorStore)
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
