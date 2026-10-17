package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/execution"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
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
  help          Show this help message
  version       Print version information

Global flags:
  --root ABS              State root (default: user config directory)
  --pueue-config ABS      Initial supervisor configuration
  --runner ABS            Runner executable for dispatch

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
	useCatalogExisting          bool
}

// storeDependencies is the narrow composition passed to read-only task-store
// commands. Profile selection, runner resolution, and supervisor mutation
// options remain outside this boundary.
type storeDependencies struct {
	predicateRegistry           func() predicate.Registry
	catalog                     commonprovider.Catalog
	supervisorOptions           pueue.Options
	initialSupervisorExecutable string
	runnerExecutable            string
	observeSupervisor           bool
}

func (d Dependencies) storeDependencies() storeDependencies {
	registry := d.PredicateRegistry
	if registry == nil && !d.Catalog.IsZero() {
		registry = d.Catalog.Registry
	}
	return storeDependencies{
		predicateRegistry:           registry,
		catalog:                     d.Catalog,
		supervisorOptions:           d.SupervisorOptions,
		initialSupervisorExecutable: d.InitialSupervisorExecutable,
		runnerExecutable:            d.RunnerExecutable,
		observeSupervisor:           true,
	}
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
		d.SupervisorOptions.PrivateMaintenanceLauncher = supervisorcore.LaunchQueueMaintenanceOwner
	}
	if d.SupervisorOptions.SuccessfulCleanupAuthorizer == nil {
		d.SupervisorOptions.SuccessfulCleanupAuthorizer = authorizePrivateSuccessfulCleanup
	}
	if d.SupervisorOptions.PrivateTerminalEvidencePreparer == nil {
		d.SupervisorOptions.PrivateTerminalEvidencePreparer = preparePrivateTerminalEvidence
	}
	if d.SupervisorOptions.PrivateMigrationAuthorizer == nil {
		d.SupervisorOptions.PrivateMigrationAuthorizer = authorizePrivateMigrationQueue
	}
	return d
}

// Run parses and executes one public delegate command.
func Run(args []string, stdout, stderr io.Writer, deps Dependencies) int {
	parsed, err := ParseArguments(args)
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "error: %v\n\n%s", err, UsageText); writeErr != nil {
			return 1
		}
		return 2
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

	normalized := deps.normalized()
	if parsed.Command == "providers" {
		return runProviders(parsed.JSON, stdout, stderr, normalized.Catalog)
	}
	if parsed.Command == "capabilities" {
		return runCapabilities(parsed.JSON, stdout, stderr, normalized.Catalog, parsed.Provider)
	}
	if parsed.Command == "models" {
		return runModels(parsed, stdout, normalized)
	}
	result := runCommand(parsed, normalized)
	if parsed.JSON {
		if err = writeJSONResponse(stdout, parsed.Command, &result.response, result.err); err != nil {
			return 1
		}
	} else {
		if err = writeHumanResponse(stdout, result.response); err != nil {
			return 1
		}
		if result.err != nil {
			return writeCLIError(stderr, commandPresentationError(parsed.Command, result), result.code)
		}
	}
	return result.code
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

func resolveInitialConfig(a Arguments) (string, error) {
	return supervisorcore.ResolveInitialConfig(supervisorAdmissionSelection(a))
}

func resolveCurrentPrivateSupervisorExecutables(root string, deps Dependencies) (client, daemon string, resultErr error) {
	return supervisorcore.ResolveCurrentPrivateSupervisorExecutables(root, deps.InitialSupervisorExecutable)
}

func newSupervisorClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, recoverPrivate bool, initialSupervisorExecutable string) (*pueue.Client, error) {
	return supervisorcore.NewClient(ctx, root, saved, options, recoverPrivate, initialSupervisorExecutable, openNativeSupervisorStore)
}

// newSupervisorControlClient proves the task's exact saved endpoint ready before
// a mutating queue command. If that endpoint is unavailable, completed private
// migration evidence may recover the current successor before the one control
// command is sent. An uncertain probe and a failed control are never retried.
func newSupervisorControlClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, initialSupervisorExecutable string) (*pueue.Client, error) {
	return supervisorcore.NewControlClient(ctx, root, saved, options, initialSupervisorExecutable, openNativeSupervisorStore)
}

func bindInitialWithOptions(a Arguments, deps Dependencies, root string, supervisorOptions pueue.Options) (*pueue.Client, error) {
	return bindInitialWithStart(a, deps, root, supervisorOptions, true)
}

func bindInitialForAdmission(a Arguments, deps Dependencies, root string, supervisorOptions pueue.Options, deferPrivateStart bool) (*pueue.Client, error) {
	return bindInitialWithStart(a, deps, root, supervisorOptions, !deferPrivateStart)
}

func bindInitialWithStart(a Arguments, deps Dependencies, root string, supervisorOptions pueue.Options, startPrivate bool) (*pueue.Client, error) {
	return supervisorcore.BindInitialWithStart(supervisorAdmissionSelection(a), supervisorAdmissionOptions(deps, supervisorOptions), root, startPrivate)
}

func sameExistingRequest(args Arguments, request, existing *task.TaskRecord) bool {
	return tasklifecycle.SameExistingRequest(args.Config.Budget, request, existing)
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
