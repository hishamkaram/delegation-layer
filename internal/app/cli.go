package app

import (
	"errors"
	"flag"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app/command"
)

// The CLI owns concrete Arguments bindings and semantic validation. The pure
// command package owns syntax/help; neither exports private runtime authority.
type commandSpec = command.Spec[argumentParser]

func leaf(name, summary, operands string) commandSpec {
	return commandSpec{Name: name, Summary: summary, Operands: operands}
}

func commandTree() commandSpec {
	return commandSpec{Register: argumentFlags(registerGlobalFlags), Notes: "\nTask results keep admission, liveness and publication independent.\n", Summary: "Manage bounded agent tasks and their evidence", Children: []commandSpec{
		withFlags(commandSpec{Name: "tui", Summary: "Open the terminal workbench", Operands: "", Example: []string{"delegate", "tui"}, Notes: "\nRequires terminal stdin and stdout. Closing the workbench does not cancel admitted tasks or stop the independent scheduler host.\n", ExitNotes: "  3 forced exit with unresolved operations; signals use 128 + signal.\n"}, registerGlobalFlags),
		{Name: "context", Summary: "Inspect or explicitly upgrade connector context", Children: []commandSpec{leaf("inspect", "Passively inspect pinned root and project", ""), leaf("enable-connector", "Explicit offline one-way root upgrade", "")}},
		{Name: "resolve", Summary: "Passively resolve an immutable task or explicitly fence/recover it", Operands: "TASK_ID"},
		{Name: "result", Summary: "Read verified committed result ranges", Children: []commandSpec{leaf("read", "Verify complete result before returning a range", "TASK_ID")}},
		{Name: "origin", Summary: "Explicit local origin recovery administration", Children: []commandSpec{leaf("barrier", "Close a fresh generation under root exclusion", ""), leaf("inventory", "Read canonical origin membership", ""), leaf("reopen", "Reopen only with current complete inventory proof", "")}},
		{Name: "dispatch", Summary: "Admit one bounded provider task", ExitNotes: "  3 managed workspace admission pending, or remote acceptance/connectivity uncertain. Inspect the same task/reference before another admission.\n  4 returned remote task outcome is rejected.\n", Operands: "", Notes: "\nRequired: --brief FILE; --cwd ABS or --workspace ID; --provider ID, --auto, or an explicit Team/member.\nRemote dispatch requires --device, --provider, --brief and exactly one of --remote-cwd or --input-manifest.\nProvider/model discovery is advisory; requested values are never silently substituted.\n", Example: []string{"delegate", "dispatch", "--provider", "codex:exec", "--brief", "/path/review.md", "--cwd", "/path/repository", "--budget", "30m", "--json"}},
		{Name: "continue", Summary: "Continue a task through its exact recorded session", ExitNotes: "  3 managed workspace preparation pending; inspect the acknowledged workspace before another continuation.\n", Operands: "", Notes: "\nRequired: --task TASK_ID. The follow-up brief is optional. Continuation uses the predecessor's exact recorded session and inherited permissions.\n", Example: []string{"delegate", "continue", "--task", "TASK_ID", "--budget", "10m", "--json"}},
		{Name: "status", Summary: "Inspect task evidence and current supervisor state", Operands: "TASK_ID", ExitNotes: "  3 remote connectivity/observation uncertain; inspect the recorded reference, not a new launch.\n  4 returned remote task outcome is rejected.\n"},
		{Name: "collect", Summary: "Collect a result, with an optional finite watch", Operands: "TASK_ID", ExitNotes: "  3 finite watch expired or result pending; 4 published result rejected.\n"},
		{Name: "recheck", Summary: "Reassess an eligible rejected local result", Operands: "TASK_ID", ExitNotes: "  4 evidence remains rejected; no recovered answer was established.\n"},
		{Name: "cancel", Summary: "Request an explicit task stop", Operands: "TASK_ID", ExitNotes: "  3 remote control/connectivity uncertain; preserve acknowledged control IDs and reconcile the same reference.\n  4 returned remote task outcome is rejected.\n"},
		{Name: "logs", Summary: "Inspect validated output descriptors or remote log ranges", Operands: "TASK_ID", ExitNotes: "  3 remote log observation/transfer pending or unavailable; no final result is implied.\n"},
		{Name: "reply", Summary: "Answer a native clarification request", ExitNotes: "  3 remote control/connectivity uncertain; preserve the message ID and inspect its delivery.\n", Operands: "TASK_ID", Notes: "\nRequired: TASK_ID, --request ID, --answers FILE and --id MESSAGE_ID.\n", Example: []string{"delegate", "reply", "TASK_ID", "--request", "REQUEST_ID", "--answers", "/path/answers.json", "--id", "MESSAGE_ID", "--json"}},
		{Name: "steer", Summary: "Send guidance to a supported active turn", ExitNotes: "  3 remote control/connectivity uncertain; preserve the message ID and inspect its delivery.\n", Operands: "TASK_ID", Notes: "\nRequired: TASK_ID, --brief FILE and --id MESSAGE_ID.\n", Example: []string{"delegate", "steer", "TASK_ID", "--brief", "/path/guidance.md", "--id", "MESSAGE_ID", "--json"}},
		{Name: "events", Summary: "Read bounded interaction events", Operands: "TASK_ID", ExitNotes: "  3 finite watch found no new events, or remote connectivity is uncertain; inspect the response.\n  4 returned remote task outcome is rejected.\n"},
		{Name: "wait", ExitNotes: "  3 finite watch expired with no matching terminal evidence.\n", Summary: "Passively wait for any or all task results", Operands: "TASK_ID...", Notes: "\nRequired: one or more separate local TASK_ID arguments. Watch is finite; undetermined liveness alone is not terminal evidence.\n", Example: []string{"delegate", "wait", "--any", "--watch", "5m", "TASK_ID", "--json"}, Register: func(fs *flag.FlagSet, p *argumentParser) error {
			registerWaitFlags(fs, &p.arguments, &p.anyMode)
			return nil
		}},
		withFlags(commandSpec{Name: "doctor", Summary: "Observe installation and static readiness", Notes: "\nOptional provider checks require --provider ID and --cwd ABS together. --model, --effort and --review-target require that pair. No provider turn is launched.\n", Example: []string{"delegate", "doctor", "--json"}}, registerDoctorFlags),
		{Name: "preflight", Summary: "Explicitly check one provider without creating a task", Operands: "", Notes: "\nRequired: --provider ID and --cwd ABS. This explicit check can launch the selected provider, but creates no task.\n", Example: []string{"delegate", "preflight", "--provider", "codex:exec", "--cwd", "/path/repository", "--json"}},
		leaf("providers", "List supported provider profiles", ""),
		{Name: "capabilities", Summary: "Inspect provider capabilities", Notes: "\nRequired: --provider ID. This reads the compiled contract without probing the provider.\n", Example: []string{"delegate", "capabilities", "--provider", "codex:exec", "--json"}},
		{Name: "models", Summary: "Discover advisory provider models and effort levels", Notes: "\nRequired: --provider ID. Optional --cwd ABS selects the native inspection context. Discovery is advisory and creates no task.\n", Example: []string{"delegate", "models", "--provider", "codex:exec", "--json"}},
		{Name: "supervisor", AllowActionAfterSeparator: true, Summary: "Inspect and explicitly maintain the private supervisor", Children: []commandSpec{
			leaf("status", "Inspect supervisor state or dated restart custody", ""), leaf("migrate", "Migrate a legacy supervisor explicitly", ""), {Name: "restart", Summary: "Restart a quiescent private runtime from its exact startup", Notes: "\nRequires --from-startup from supervisor status --runtime-restart. Run from a prepared persistent user context. Repeat only the same token to reconcile an accepted handoff.\n", ExitNotes: "  3 restart is blocked or pending; no provider task is launched.\n"}, {Name: "ensure-owner", Summary: "Prepare an owner in a persistent user context", Operands: "", Notes: "\nRun only from a prepared persistent user context outside task-owned scope.\n"}, {Name: "stop-owner", Summary: "Stop infrastructure serving without cancelling tasks", ExitNotes: "  3 owner is busy; stop was refused and tasks were not cancelled.\n"},
		}},
		{Name: "workspace", AllowActionAfterSeparator: true, Register: registerWorkspaceParserFlags, Summary: "Inspect and control managed Git workspaces", Children: []commandSpec{
			leaf("list", "List managed workspaces", ""), {Name: "status", Summary: "Observe a workspace or acknowledged operation", Operands: "WORKSPACE_ID", ExitNotes: "  3 workspace operation/observation pending; inspect the selected workspace and acknowledged operation before retrying.\n"}, {Name: "exec", Summary: "Run explicit argv within a managed workspace", ExitNotes: "  3 workspace operation pending; inspect its acknowledged operation before another exec.\n", Operands: "WORKSPACE_ID -- COMMAND ARGS...", Notes: "\nPass each command argument separately after --. No shell expansion is performed.\n", Example: []string{"delegate", "workspace", "exec", "WORKSPACE_ID", "--budget", "5m", "--", "go", "test", "./..."}}, {Name: "release", Summary: "Release a managed checkout", Operands: "WORKSPACE_ID", ExitNotes: "  3 workspace operation/observation pending; inspect the selected workspace and acknowledged operation before retrying.\n"}, {Name: "forget", Summary: "Forget an eligible managed workspace", Operands: "WORKSPACE_ID", ExitNotes: "  3 workspace operation/observation pending; inspect the selected workspace and acknowledged operation before retrying.\n"},
		}},
		{Name: "checkpoint", AllowActionAfterSeparator: true, Register: func(fs *flag.FlagSet, p *argumentParser) error {
			registerCheckpointFlags(fs, &p.arguments, &p.watch)
			return nil
		}, Summary: "Manage Git content checkpoints", Children: []commandSpec{
			{Name: "create", Summary: "Capture one explicit Git target", ExitNotes: "  3 no final creation receipt; inspect reason_code, next_action and any acknowledged ID. Do not assume creation completed.\n", Operands: "", Notes: "\nRequired: exactly one of --cwd ABS or --workspace ID.\n", Example: []string{"delegate", "checkpoint", "create", "--cwd", "/path/repository", "--json"}}, {Name: "status", Summary: "Observe checkpoint evidence", Operands: "CHECKPOINT_ID", ExitNotes: "  3 checkpoint observation pending; watch the same acknowledged checkpoint/operation.\n"}, leaf("list", "List checkpoints", ""), {Name: "remove", Summary: "Remove an eligible checkpoint", Operands: "CHECKPOINT_ID", ExitNotes: "  3 no final removal receipt; inspect reason_code, next_action and acknowledged IDs before retrying.\n"},
		}},
		{Name: "schedule", Register: argumentFlags(registerScheduleFlags), Summary: "Manage immutable recurring definitions and inspect executions", Children: scheduleSpecs()},
		{Name: "scheduler", AllowActionAfterSeparator: true, Summary: "Inspect or explicitly control the independent scheduler host", Children: []commandSpec{
			{Name: "serve", Summary: "Run the independent host in this foreground terminal", ExitNotes: "  3 host shutdown/cleanup is still pending; inspect scheduler status.\n", Operands: "", Notes: "\nThis stays in the foreground. Start it in an independent terminal or tmux pane.\n"}, {Name: "status", Summary: "Inspect host readiness", ExitNotes: "  3 host readiness observation pending; inspect the response, not schedule success.\n"}, {Name: "stop", Summary: "Stop the host without cancelling admitted tasks", ExitNotes: "  3 host shutdown is pending; stop acceptance does not prove completed shutdown.\n"}, {Name: "run", Summary: "Run the foreground compatibility runtime", Operands: "", Notes: "\nThis stays in the foreground. Start it in an independent terminal or tmux pane.\n"}, {Name: "migrate", Summary: "Explicitly migrate a stopped scheduler", Operands: "", Notes: "\nStop the scheduler host before migration. Observation commands never migrate automatically.\n"},
		}},
		{Name: "teams", Register: func(fs *flag.FlagSet, p *argumentParser) error { return registerTeamsFlags(fs, &p.arguments) }, Summary: "Manage optional scoped Teams and explicit member selection", Children: teamSpecs()},
		{Name: "devices", AllowActionAfterSeparator: true, Register: argumentFlags(registerRemoteManagementFlags), Summary: "Inspect configured SSH aliases and explicit trust", Children: []commandSpec{
			leaf("list", "List SSH aliases without connecting", ""), {Name: "probe", Summary: "Explicitly probe a remote installation", ExitNotes: "  3 connectivity/observation uncertain; inspect the reported probe stages.\n", Notes: "\nRequired: --device SSH_ALIAS naming an existing SSH configuration alias. This explicitly connects; browsing does not.\n", Example: []string{"delegate", "devices", "probe", "--device", "review-mac", "--json"}}, {Name: "trust", Summary: "Enroll an explicitly verified host fingerprint", ExitNotes: "  3 connectivity/enrollment uncertain; inspect trust before repeating enrollment.\n", Notes: "\nRequired: --device SSH_ALIAS and --fingerprint VERIFIED_FINGERPRINT. Independently verify the exact host key before enrollment; trust is never automatic.\n", Example: []string{"delegate", "devices", "trust", "--device", "review-mac", "--fingerprint", "SHA256:VERIFIED_FINGERPRINT", "--json"}},
		}},
		{Name: "remote", AllowActionAfterSeparator: true, Register: argumentFlags(registerRemoteManagementFlags), Summary: "Inspect origin references and control remote resources", Children: []commandSpec{
			leaf("list", "List origin-recorded remote task references", ""), {Name: "reconcile", Summary: "Reconcile an uncertain remote admission", Operands: "remote:ID", ExitNotes: "  3 remote observation/control pending or connectivity uncertain; inspect the same reference and selected scope before repeating effects.\n  4 returned remote task outcome is rejected.\n"}, {Name: "usage", Summary: "Inspect owned remote byte families", Operands: "remote:ID", ExitNotes: "  3 remote observation/control pending or connectivity uncertain; inspect the same reference and selected scope before repeating effects.\n"}, {Name: "retire", Summary: "Preview or retire selected owned bytes", Operands: "remote:ID", ExitNotes: "  3 remote observation/control pending or connectivity uncertain; inspect the same reference and selected scope before repeating effects.\n"}, {Name: "serve", Summary: "Private framed control server", Hidden: true},
		}},
		leaf("help", "Show root or command-specific help", "[COMMAND [ACTION...]]"),
		leaf("version", "Print the installed version", ""),
	}}
}

func scheduleSpecs() []commandSpec {
	return []commandSpec{
		{Name: "create", Summary: "Save an immutable schedule without launching a task", ExitNotes: "  3 host readiness uncertain or not ready; when saved is true the definition is already saved. Inspect next_action; do not repeat creation.\n", Operands: "", Notes: "\nRequired: --brief FILE, --cwd ABS, --cron RULE and a provider, --auto or explicit Team/member selection.\nDefinitions are immutable. Saving never starts a scheduler host or provider.\n", Example: []string{"delegate", "schedule", "create", "--provider", "claude:print", "--brief", "/path/review.md", "--cwd", "/path/repository", "--cron", "0 */2 * * *", "--timezone", "Asia/Bangkok", "--json"}},
		leaf("list", "List schedule definitions", ""), leaf("show", "Inspect a schedule definition", "SCHEDULE_ID"), leaf("health", "Inspect per-schedule health and active holds", "SCHEDULE_ID"), leaf("history", "Read bounded occurrence and skip history", "SCHEDULE_ID"), leaf("execution", "Read an occurrence and local task evidence", "TASK_ID"),
		{Name: "pause", Summary: "Pause future admissions without cancelling active work", Operands: "SCHEDULE_ID", Notes: "\nThis changes future scheduling; it does not cancel an admitted task.\n"},
		{Name: "resume", Summary: "Resume future admissions and clear an active hold", Operands: "SCHEDULE_ID", ExitNotes: "  3 host readiness uncertain or not ready; when saved is true resume is already saved. Inspect scheduler/next_action rather than repeating the mutation.\n"},
		{Name: "delete", Summary: "Delete future scheduling without cancelling active work", Operands: "SCHEDULE_ID", Notes: "\nThis changes future scheduling; it does not cancel an admitted task.\n"},
		{Name: "policy", Summary: "Inspect or change schedule quota policy", Children: []commandSpec{leaf("get", "Inspect the current quota policy", "SCHEDULE_ID"), {Name: "set", Summary: "Set explicit quota hold policy", Operands: "SCHEDULE_ID", Notes: "\nRequired: SCHEDULE_ID and at least one of --quota-hold on|off or --quota-max-hold DURATION. Policy changes do not mutate the saved definition.\n", Example: []string{"delegate", "schedule", "policy", "set", "SCHEDULE_ID", "--quota-hold", "on", "--quota-max-hold", "6h", "--json"}}}},
	}
}

func teamSpecs() []commandSpec {
	return []commandSpec{
		{Name: "register", Summary: "Register a scoped Team definition", Operands: "", Notes: "\nRequired: --scope user|project and --definition FILE (or - for stdin). Project scope also requires --cwd ABS; outside Git supply --project-root ABS. User scope does not take project paths.\n", Example: []string{"delegate", "teams", "register", "--scope", "user", "--definition", "/path/team.json", "--json"}},
		{Name: "show", Summary: "Inspect one scoped Team", Notes: "\nRequired: --scope user|project and --team TEAM_ID. Project scope requires --cwd ABS; outside Git also supply --project-root ABS.\n", Example: []string{"delegate", "teams", "show", "--scope", "user", "--team", "TEAM_ID", "--json"}},
		{Name: "update", Summary: "Update a Team at its expected revision", Operands: "", Notes: "\nRequired: --scope user|project, --team TEAM_ID, --definition FILE (or - for stdin), and positive --expected-revision N. Project scope requires --cwd ABS; outside Git also supply --project-root ABS. A revision conflict requires reload and review.\n", Example: []string{"delegate", "teams", "update", "--scope", "user", "--team", "TEAM_ID", "--definition", "/path/team.json", "--expected-revision", "1", "--json"}},
		{Name: "delete", Summary: "Delete a Team at its expected revision", Operands: "", Notes: "\nRequired: --scope user|project, --team TEAM_ID and positive --expected-revision N. Project scope requires --cwd ABS; outside Git also supply --project-root ABS.\n", Example: []string{"delegate", "teams", "delete", "--scope", "user", "--team", "TEAM_ID", "--expected-revision", "1", "--json"}},
		leaf("list", "List scoped Teams", ""),
		{Name: "default", Summary: "Manage an explicit project Team default", Children: []commandSpec{{Name: "show", Summary: "Inspect the project default", Notes: "\nRequired: --cwd ABS. Outside Git also supply --project-root ABS. This observation does not change the default or route tasks.\n", Example: []string{"delegate", "teams", "default", "show", "--cwd", "/path/repository", "--json"}}, {Name: "set", Summary: "Set the project default without auto-routing tasks", Notes: "\nRequired: --cwd ABS, --team-scope user|project and --team TEAM_ID. Outside Git also supply --project-root ABS. Defaults do not automatically route tasks.\n", Example: []string{"delegate", "teams", "default", "set", "--cwd", "/path/repository", "--team-scope", "user", "--team", "TEAM_ID", "--json"}}, {Name: "clear", Summary: "Clear the project default", Notes: "\nRequired: --cwd ABS. Outside Git also supply --project-root ABS. This removes only the selected project default.\n", Example: []string{"delegate", "teams", "default", "clear", "--cwd", "/path/repository", "--json"}}}},
	}
}

func argumentsForPath(path []string) Arguments {
	var a Arguments
	if len(path) == 0 {
		a.Command = "help"
		return a
	}
	a.Command = path[0]
	if len(path) < 2 {
		return a
	}
	switch a.Command {
	case "context", "result", "origin":
		a.Connector.Action = path[1]
	case "supervisor":
		a.SupervisorAction = path[1]
	case "scheduler":
		a.SchedulerAction = path[1]
	case "workspace":
		a.WorkspaceAction = path[1]
	case "checkpoint":
		a.CheckpointAction = path[1]
	case "schedule":
		a.ScheduleAction = path[1]
		if len(path) > 2 {
			a.SchedulePolicyOperation = path[2]
		}
	case "teams":
		a.TeamAction = path[1]
		if len(path) > 2 {
			a.TeamDefaultAction = path[2]
		}
	case "remote", "devices":
		a.RemoteAction = path[1]
	}
	return a
}

type argumentParser struct {
	arguments Arguments
	watch     string
	anyMode   bool
}

func argumentFlags(register func(*flag.FlagSet, *Arguments)) func(*flag.FlagSet, *argumentParser) error {
	return func(fs *flag.FlagSet, p *argumentParser) error { register(fs, &p.arguments); return nil }
}

func withFlags(s commandSpec, register func(*flag.FlagSet, *Arguments)) commandSpec {
	s.Register = argumentFlags(register)
	return s
}

func registerOrdinaryParserFlags(fs *flag.FlagSet, p *argumentParser) error {
	registerGlobalFlags(fs, &p.arguments)
	registerCommandFlags(fs, &p.arguments, &p.watch)
	registerRemoteFlags(fs, &p.arguments)
	return nil
}

func registerWorkspaceParserFlags(fs *flag.FlagSet, p *argumentParser) error {
	registerWorkspaceFlags(fs, &p.arguments)
	if p.arguments.WorkspaceAction == "status" {
		fs.StringVar(&p.watch, "watch", "0s", "finite workspace observation bound")
	}
	return nil
}

func registerGlobalFlags(fs *flag.FlagSet, a *Arguments) {
	fs.StringVar(&a.Root, "root", "", "Advanced explicit state root `ABS`; omit for the configured default")
	fs.StringVar(&a.PueueConfig, "pueue-config", "", "Advanced initial supervisor configuration `ABS`")
	fs.StringVar(&a.Runner, "runner", "", "Advanced custom runner override; omit for the automatic runner (absolute path `ABS`)")
}

func extractExecArguments(a *Arguments, rest []string) ([]string, error) {
	if a.Command != "workspace" || a.WorkspaceAction != "exec" {
		return rest, nil
	}
	for i, arg := range rest {
		if arg == "--" {
			if i+1 == len(rest) || rest[i+1] == "" {
				return nil, errors.New("workspace exec requires a command after --")
			}
			a.ExecArguments = append([]string(nil), rest[i+1:]...)
			return rest[:i], nil
		}
	}
	// Help must remain usable without a command payload.
	for _, arg := range rest {
		if arg == "--help" || arg == "-h" {
			return rest, nil
		}
	}
	return nil, errors.New("workspace exec requires a command after --")
}

func (p *argumentParser) validate(fs *flag.FlagSet) error {
	a := &p.arguments
	noteControlFlags(fs, a)
	switch a.Command {
	case "workspace":
		return validateWorkspaceArguments(a, fs.Args(), p.watch)
	case "checkpoint":
		return validateCheckpointArguments(a, fs.Args(), p.watch)
	case "schedule":
		return validateParsedSchedule(a, fs)
	case "teams":
		if err := validateNoPositional("teams "+a.TeamAction, fs.Args()); err != nil {
			return err
		}
		return validateTeamsArguments(*a)
	case "devices", "remote":
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "scope" {
				a.remoteScopeSet = true
			}
		})
		validated, err := validateRemoteAction(*a, fs.Args())
		*a = validated
		return err
	case "doctor":
		if err := validateNoPositional("doctor", fs.Args()); err != nil {
			return err
		}
		return validateDoctorArguments(a, fs)
	case "wait":
		if p.anyMode && a.WaitAll {
			return errors.New("wait accepts either --any or --all")
		}
		a.WaitTaskIDs = fs.Args()
		return validateWaitArguments(*a)
	default:
		return a.validate(fs.Args(), p.watch)
	}
}

func validateParsedSchedule(a *Arguments, fs *flag.FlagSet) error {
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "timezone" {
			a.ScheduleTimezoneSet = true
		}
	})
	if err := a.validate(nil, "0s"); err != nil {
		return err
	}
	if err := validateSchedulePositionals(a, fs.Args()); err != nil {
		return err
	}
	return validateScheduleCreateOptions(a)
}

func commandEngine() command.Engine[argumentParser] {
	root := commandTree()
	return command.Engine[argumentParser]{Root: root, NewState: func(path []string) argumentParser {
		return argumentParser{arguments: argumentsForPath(path), watch: "0s"}
	}, DefaultRegister: registerOrdinaryParserFlags, Validate: func(p *argumentParser, fs *flag.FlagSet) error { return p.validate(fs) }, Prepare: func(p *argumentParser, args []string) ([]string, error) {
		return extractExecArguments(&p.arguments, args)
	}}
}

// ParseArguments grants no external authority and performs no I/O.
func ParseArguments(args []string) (Arguments, error) {
	parsed, err := commandEngine().Parse(args)
	if err != nil {
		return Arguments{}, err
	}
	a := parsed.State.arguments
	if parsed.Help {
		a.Command = "help"
		a.HelpPath = parsed.HelpPath
	}
	return a, nil
}

// HelpText renders actual registered flags without external discovery.
func HelpText(path []string) (string, error) { return commandEngine().Help(path) }

// CommandUsage chooses scoped error guidance without executing a command.
func CommandUsage(argv []string) string { return commandEngine().UsageForArgs(argv) }

// CommandDefaults returns typed registration defaults without validating
// required user inputs or duplicating flag defaults in the TUI.
func CommandDefaults(path []string) (Arguments, error) {
	p, err := commandEngine().Defaults(path)
	if err != nil {
		return Arguments{}, err
	}
	if p.arguments.Command != "wait" {
		duration, parseErr := time.ParseDuration(p.watch)
		if parseErr != nil {
			return Arguments{}, parseErr
		}
		p.arguments.Watch = duration
	}
	return p.arguments, nil
}
