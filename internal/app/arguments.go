// Package app composes public commands with durable task and supervisor APIs.
package app

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	schedulercontract "github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

// Arguments contains requested values only. Parsing grants no external authority.
type Arguments struct {
	LogStream            string
	LogCursor            string
	LogOffset            int64
	LogLimit             int64
	LogOffsetSet         bool
	logOptionsSet        bool
	RemoteScope          string
	RemoteApply          bool
	RemoteDryRun         bool
	remoteScopeSet       bool
	ResourceGuards       resourceadmission.GuardPolicy
	Device               string
	RemoteRoot           string
	RemoteCwd            string
	InputManifest        string
	OutputManifest       string
	OutputDirectory      string
	RemoteExecutable     string
	SSHConfig            string
	RemoteAfter          string
	RemoteAction         string
	Fingerprint          string
	Stdio                bool
	remoteBinding        *task.RemoteBinding
	remoteCancelID       string
	MessageID            string
	RequestID            string
	Answers              string
	AnswersBytes         []byte
	SuccessorTaskID      string
	HelpPath             []string
	After                uint64
	Command              string
	SupervisorAction     string
	SchedulerAction      string
	Root                 string
	PueueConfig          string
	Runner               string
	TaskID               string
	Auto                 bool
	Provider             string
	Brief                string
	BriefBytes           []byte
	Cwd                  string
	Config               task.TaskConfig
	ReviewTarget         string
	ResumeTask           string
	JSON                 bool
	Watch                time.Duration
	WaitTaskIDs          []string
	WaitAll              bool
	Isolation            string
	WorkspaceID          string
	Base                 string
	WorkspaceBudget      string
	WorkspaceAction      string
	WorkspaceOperationID string
	WorkspaceAfter       string
	WorkspaceLimit       int
	Discard              bool
	ExecArguments        []string
	workspaceBinding     *task.WorkspaceBinding

	// Checkpoint values are requested only; creation targets reuse Cwd and
	// WorkspaceID, and status reuses Watch.
	CheckpointAction      string
	CheckpointID          string
	CheckpointOperationID string
	CheckpointAfter       string
	CheckpointLimit       int
	CheckpointBudget      string

	// Schedule values record the requested definition and query cursors only.
	ScheduleAction          string
	ScheduleID              string
	ScheduleCron            string
	ScheduleTimezone        string
	ScheduleTimezoneSet     bool
	ScheduleAfter           string
	ScheduleBefore          string
	ScheduleLimit           int
	ScheduleHook            *schedulercontract.FailureHook
	SchedulePolicyOperation string
	ScheduleQuotaHold       string
	ScheduleQuotaMaxHold    string

	TeamAction            string
	TeamDefaultAction     string
	TeamScope             string
	TeamID                string
	MemberID              string
	TeamDefinitionBytes   []byte
	TeamDefinition        string
	ExpectedTeamSelection *teamcontract.Selection
	TeamExpectedRevision  int64
	TeamAfter             string
	TeamLimit             int
	ProjectRoot           string
	teamTarget            *task.TeamTarget
	teamContext           *task.TeamContext

	// savedSupervisor is populated only by the continuation command after the
	// predecessor binding has been loaded and validated. It never comes from
	// public argv; continuation admission must reuse that exact authority.
	savedSupervisor *task.SupervisorRef
	// runnerOwnership is set only when continuation reuses recorded ownership.
	// A public --runner value remains custom even when it matches a managed path.
	runnerOwnership string
	// generatedTaskID is allocated by continue after its successor has passed
	// compatibility validation. It is never populated from public argv.
	generatedTaskID string
	// These flags are tracked separately from their values so an explicitly
	// empty unsupported supervisor option is rejected too.
	supervisorPueueConfigSet bool
	supervisorRunnerSet      bool
	schedulerHostReadFD      int
	schedulerHostWriteFD     int
	schedulerResident        bool
}

func noteControlFlags(fs *flag.FlagSet, a *Arguments) {
	fs.Visit(func(selected *flag.Flag) {
		switch a.Command {
		case "logs":
			if selected.Name == "offset" {
				a.LogOffsetSet = true
			}
			if selected.Name == "offset" || selected.Name == "stream" || selected.Name == "limit" || selected.Name == "cursor" {
				a.logOptionsSet = true
			}
		case "supervisor":
			switch selected.Name {
			case "pueue-config":
				a.supervisorPueueConfigSet = true
			case "runner":
				a.supervisorRunnerSet = true
			}
		case "scheduler":
			if selected.Name == "scheduler-host-read-fd" || selected.Name == "scheduler-host-write-fd" {
				a.schedulerResident = true
			}
		}
	})
}

func registerCommandFlags(fs *flag.FlagSet, a *Arguments, watch *string) {
	if a.Command == "help" || a.Command == "version" {
		return
	}
	fs.BoolVar(&a.JSON, "json", false, "emit versioned JSON")
	if a.Command == "supervisor" || a.Command == "scheduler" {
		if a.Command == "scheduler" && a.SchedulerAction == "run" {
			fs.IntVar(&a.schedulerHostReadFD, "scheduler-host-read-fd", 0, "private host channel")
			fs.IntVar(&a.schedulerHostWriteFD, "scheduler-host-write-fd", 0, "private resident channel")
		}
		return
	}
	if a.Command == "collect" || a.Command == "events" {
		fs.StringVar(watch, "watch", "0s", "finite observation bound")
	}
	if registerInteractionFlags(fs, a) {
		return
	}
	if a.Command == "models" {
		fs.StringVar(&a.Provider, "provider", "", "provider profile")
		fs.StringVar(&a.Cwd, "cwd", "", "working directory (default: current directory)")
		return
	}
	if a.Command == "capabilities" {
		fs.StringVar(&a.Provider, "provider", "", "provider profile")
		return
	}
	if a.Command == "preflight" {
		fs.StringVar(&a.Provider, "provider", "", "provider profile")
		fs.StringVar(&a.Cwd, "cwd", "", "absolute working directory")
		fs.StringVar(&a.Config.Permission, "permission", config.DefaultMode, "permission intent")
		fs.StringVar(&a.Config.Budget, "budget", "", "finite execution budget")
		fs.StringVar(&a.Config.NativeTimeout, "native-timeout", "", "optional provider-native timeout")
		fs.StringVar(&a.Config.Model, "model", "", "requested model")
		fs.StringVar(&a.Config.Effort, "effort", "", "requested effort")
		fs.StringVar(&a.ReviewTarget, "review-target", "", "Codex review target: uncommitted, base:BRANCH, or commit:SHA")
		return
	}
	registerAdmissionFlags(fs, a)
}

func (a *Arguments) validate(positional []string, watch string) error {
	for _, p := range []string{a.Root, a.PueueConfig, a.Runner} {
		if p != "" && !filepath.IsAbs(p) {
			return errors.New("root, pueue-config and runner must be absolute paths")
		}
	}
	return a.validateCommand(positional, watch)
}

func (a *Arguments) validateCommand(positional []string, watch string) error {
	switch a.Command {
	case "help", "version", "providers", "tui":
		return validateNoPositional(a.Command, positional)
	case "supervisor":
		return a.validateSupervisor(positional)
	case "scheduler":
		if a.schedulerResident && (a.SchedulerAction != "run" || a.schedulerHostReadFD != 3 || a.schedulerHostWriteFD != 4) {
			return errors.New("scheduler resident requires its exact inherited host channels")
		}
		switch a.SchedulerAction {
		case "serve", "status", "stop", "run", "migrate":
			return validateNoPositional("scheduler "+a.SchedulerAction, positional)
		default:
			return errors.New("scheduler requires serve, status, stop, run or migrate")
		}
	case "models":
		return a.validateModels(positional)
	case "capabilities":
		return validateCapabilities(positional, a.Provider)
	case "preflight", "continue", "dispatch":
		return a.validatePreparationCommand(positional)
	case "status", "collect", "cancel", "logs", "reply", "steer", "events":
		return a.validateTaskInteraction(positional, watch)
	case "recheck":
		return a.validateRecheck(positional)
	}
	return nil
}

func (a *Arguments) validateSupervisor(positional []string) error {
	if a.SupervisorAction != "status" && a.SupervisorAction != "migrate" && a.SupervisorAction != "stop-owner" && a.SupervisorAction != "ensure-owner" {
		return errors.New("supervisor requires status, migrate or stop-owner")
	}
	if a.supervisorPueueConfigSet || a.supervisorRunnerSet {
		return errors.New("supervisor does not accept --pueue-config or --runner")
	}
	return validateNoPositional("supervisor", positional)
}

func (a *Arguments) validateModels(positional []string) error {
	if a.Cwd != "" && !filepath.IsAbs(a.Cwd) {
		return errors.New("models --cwd must be absolute")
	}
	return validateCapabilities(positional, a.Provider)
}

func (a *Arguments) validatePreparationCommand(positional []string) error {
	if _, err := task.ParseOutputSilenceTimeout(a.Config.OutputSilenceTimeout); err != nil {
		return err
	}
	switch a.Command {
	case "preflight":
		return a.validatePreflight(positional)
	case "continue":
		return a.validateContinue(positional)
	case "dispatch":
		return a.validateDispatch(positional)
	default:
		return nil
	}
}

func (a *Arguments) validateTaskInteraction(positional []string, watch string) error {
	if err := a.validateTaskCommand(positional, watch); err != nil {
		return err
	}
	if err := a.validateInteractionArguments(); err != nil {
		return err
	}
	return a.validateLogArguments()
}

func (a *Arguments) validateRecheck(positional []string) error {
	if len(positional) != 1 {
		return errors.New("exactly one task ID is required")
	}
	a.TaskID = positional[0]
	if strings.HasPrefix(a.TaskID, "remote:") {
		return errors.New("recheck is unsupported for remote references")
	}
	return task.ValidateTaskID(a.TaskID)
}

func validateNoPositional(command string, positional []string) error {
	if len(positional) != 0 {
		return fmt.Errorf("unexpected extra argument %q for %s", positional[0], command)
	}
	return nil
}

func validateCapabilities(positional []string, provider string) error {
	if err := validateNoPositional("capabilities", positional); err != nil {
		return err
	}
	return config.ValidateProvider(provider)
}

func (a *Arguments) validatePreflight(positional []string) error {
	if err := validateNoPositional("preflight", positional); err != nil {
		return err
	}
	if err := config.ValidateProvider(a.Provider); err != nil {
		return err
	}
	if !filepath.IsAbs(a.Cwd) {
		return errors.New("preflight requires --cwd ABS")
	}
	if err := config.ValidateMode(a.Config.Permission); err != nil {
		return err
	}
	if _, err := config.ParseBudget(a.Config.Budget); err != nil {
		return err
	}
	return a.applyReviewTarget()
}

func (a *Arguments) validateContinue(positional []string) error {
	if err := validateNoPositional("continue", positional); err != nil {
		return err
	}
	if a.TaskID == "" {
		return errors.New("continue requires --task TASK_ID")
	}
	if strings.HasPrefix(a.TaskID, "remote:") {
		return errors.New("continue is unsupported for remote references")
	}
	if err := task.ValidateTaskID(a.TaskID); err != nil {
		return err
	}
	if err := validateContentSources(a.Brief, a.BriefBytes, false); err != nil {
		return err
	}
	if a.Brief != "" && !filepath.IsAbs(a.Brief) {
		return errors.New("continue --brief must be an absolute path")
	}
	_, err := config.ParseBudget(a.Config.Budget)
	return err
}

func (a *Arguments) validateTaskCommand(positional []string, watch string) error {
	if len(positional) != 1 {
		return errors.New("exactly one task ID is required")
	}
	a.TaskID = positional[0]
	if err := validateRoutedTaskID(a.TaskID); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(watch)
	if err != nil || parsed < 0 {
		return errors.New("watch must be a finite nonnegative duration")
	}
	a.Watch = parsed
	return nil
}

func (a *Arguments) validateDispatch(positional []string) error {
	if a.Device != "" {
		return a.validateRemoteDispatch(positional)
	}
	if err := validateWorkspaceDispatch(*a); err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("dispatch does not accept positional arguments or raw argv")
	}
	if err := a.validateDispatchProvider(); err != nil {
		return err
	}
	if err := validateContentSources(a.Brief, a.BriefBytes, true); err != nil {
		return err
	}
	if a.WorkspaceID == "" && !filepath.IsAbs(a.Cwd) {
		return errors.New("dispatch requires --brief FILE and --cwd ABS or --workspace ID")
	}
	if err := config.ValidateMode(a.Config.Permission); err != nil {
		return err
	}
	if _, err := config.ParseBudget(a.Config.Budget); err != nil {
		return err
	}
	if err := a.applyReviewTarget(); err != nil {
		return err
	}
	return a.validateDispatchIDs()
}

func (a *Arguments) applyReviewTarget() error {
	if a.ReviewTarget == "" {
		return nil
	}
	target, err := parseReviewTarget(a.ReviewTarget)
	if err != nil {
		return err
	}
	a.Config.ReviewTarget = &target
	return nil
}

func parseReviewTarget(value string) (task.ReviewTarget, error) {
	if value == task.ReviewTargetUncommitted {
		return task.ReviewTarget{Kind: task.ReviewTargetUncommitted}, nil
	}
	for _, kind := range []string{task.ReviewTargetBase, task.ReviewTargetCommit} {
		prefix := kind + ":"
		if strings.HasPrefix(value, prefix) {
			target := task.ReviewTarget{Kind: kind, Value: strings.TrimPrefix(value, prefix)}
			if err := task.ValidateReviewTarget(&target); err != nil {
				return task.ReviewTarget{}, err
			}
			return target, nil
		}
	}
	return task.ReviewTarget{}, errors.New("review target must be uncommitted, base:BRANCH, or commit:SHA")
}

func (a *Arguments) validateDispatchProvider() error {
	if err := validateTeamTargetInputs(*a); err != nil {
		return err
	}
	if a.Auto {
		if a.Provider != "" {
			return errors.New("--auto and --provider cannot be used together")
		}
		return nil
	}
	if a.Provider == "" && requestedTeamTarget(*a) != nil {
		return nil
	}
	return config.ValidateProvider(a.Provider)
}

func (a *Arguments) validateDispatchIDs() error {
	for _, id := range []string{a.TaskID, a.ResumeTask} {
		if id != "" {
			if err := task.ValidateTaskID(id); err != nil {
				return err
			}
		}
	}
	if a.TaskID != "" && a.TaskID == a.ResumeTask {
		return errors.New("continuation requires a different task ID")
	}
	if a.Auto && a.ResumeTask != "" {
		return errors.New("automatic dispatch cannot resume a predecessor")
	}
	return nil
}

func (a *Arguments) validateInteractionArguments() error {
	if a.Command != "reply" && a.Command != "steer" {
		return nil
	}
	if err := task.ValidateTaskID(a.MessageID); err != nil {
		return err
	}
	if a.Command == "reply" {
		if err := validateContentSources(a.Answers, a.AnswersBytes, true); err != nil {
			return err
		}
	}
	if a.Command == "reply" && a.RequestID == "" {
		return errors.New("reply requires --request ID --answers FILE --id MESSAGE_ID")
	}
	if a.Command == "steer" {
		return validateContentSources(a.Brief, a.BriefBytes, true)
	}
	return nil
}

func registerInteractionFlags(fs *flag.FlagSet, a *Arguments) bool {
	if a.Command == "reply" || a.Command == "steer" {
		fs.StringVar(&a.MessageID, "id", "", "idempotent message ID")
		if a.Command == "reply" {
			fs.StringVar(&a.RequestID, "request", "", "pending request ID")
			fs.StringVar(&a.Answers, "answers", "", "answers JSON file")
		} else {
			fs.StringVar(&a.Brief, "brief", "", "steering brief file")
		}
		return true
	}
	if a.Command == "events" {
		fs.Uint64Var(&a.After, "after", 0, "last observed event sequence")
		return true
	}
	if a.Command == "dispatch" || a.Command == "continue" || a.Command == "preflight" {
		fs.BoolVar(&a.Config.Interactive, "interactive", false, "enable native clarification and supported steering")
	}

	return false
}

// guardFlag exposes positive default-on CLI flags while keeping zero-value
// Arguments safe for programmatic callers.
type guardFlag struct{ disabled *bool }

func (f guardFlag) String() string {
	if f.disabled == nil {
		return "true"
	}
	return strconv.FormatBool(!*f.disabled)
}
func (f guardFlag) IsBoolFlag() bool { return true }
func (f guardFlag) Set(value string) error {
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	*f.disabled = !enabled
	return nil
}

// registerAdmissionFlags owns flags for fresh ordinary admission only.
func registerAdmissionFlags(fs *flag.FlagSet, a *Arguments) {
	if a.Command != "dispatch" && a.Command != "continue" {
		return
	}
	fs.StringVar(&a.Config.OutputSilenceTimeout, "output-silence-timeout", "", "optional provider output-silence limit; 0 disables")
	fs.Var(guardFlag{disabled: &a.ResourceGuards.DisableCPU}, "cpu-guard", "check CPU pressure before fresh admission (default true)")
	fs.Var(guardFlag{disabled: &a.ResourceGuards.DisableMemory}, "memory-guard", "check memory pressure before fresh admission (default true)")
	if a.Command == "continue" {
		fs.StringVar(&a.TaskID, "task", "", "predecessor task ID")
		fs.StringVar(&a.Brief, "brief", "", "optional follow-up brief file")
		fs.StringVar(&a.Config.Budget, "budget", "", "finite continuation budget")
		fs.StringVar(&a.Config.Model, "model", "", "continuation model override")
		fs.StringVar(&a.Config.Effort, "effort", "", "continuation effort override")
		return
	}
	fs.StringVar(&a.TaskID, "id", "", "task ID")
	fs.BoolVar(&a.Auto, "auto", false, "select the first ready provider")
	fs.StringVar(&a.Provider, "provider", "", "provider profile")
	fs.StringVar(&a.Brief, "brief", "", "brief file")
	fs.StringVar(&a.Cwd, "cwd", "", "absolute working directory")
	fs.StringVar(&a.Config.Permission, "permission", config.DefaultMode, "permission intent")
	fs.StringVar(&a.Config.Budget, "budget", "", "finite execution budget")
	fs.StringVar(&a.Config.NativeTimeout, "native-timeout", "", "optional agy timeout within the task budget")
	fs.StringVar(&a.Config.Model, "model", "", "requested model")
	fs.StringVar(&a.Config.Effort, "effort", "", "requested effort")
	fs.StringVar(&a.ReviewTarget, "review-target", "", "Codex review target: uncommitted, base:BRANCH, or commit:SHA")
	fs.StringVar(&a.ResumeTask, "resume-task", "", "exact predecessor task ID")
	fs.StringVar(&a.Isolation, "isolation", "", "optional workspace isolation: git-worktree")
	fs.StringVar(&a.WorkspaceID, "workspace", "", "retained managed workspace ID")
	fs.StringVar(&a.Base, "base", "", "committed Git base (default: HEAD)")
	fs.StringVar(&a.WorkspaceBudget, "workspace-budget", "", "finite workspace helper budget (default: 5m)")
	registerTeamTargetFlags(fs, a)
}

// validateContentSources preserves absent versus explicitly supplied content.
// The content owner validates the bytes; argv can only select the file path.
func validateContentSources(path string, content []byte, required bool) error {
	if path != "" && content != nil {
		return errors.New("choose a file path or inline content, not both")
	}
	if required && path == "" && content == nil {
		return errors.New("a content file or inline content is required")
	}
	return nil
}
