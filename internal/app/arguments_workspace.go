package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func validateWorkspaceDispatch(a Arguments) error {
	if a.Isolation != "" && a.Isolation != "git-worktree" {
		return errors.New("--isolation must be git-worktree")
	}
	if a.WorkspaceID != "" {
		if err := task.ValidateTaskID(a.WorkspaceID); err != nil {
			return err
		}
		if a.Isolation != "" || a.Base != "" || a.Cwd != "" {
			return errors.New("--workspace cannot be combined with --cwd, --base, or --isolation")
		}
	}
	if a.Base != "" && a.Isolation == "" {
		return errors.New("--base requires --isolation git-worktree")
	}
	if a.WorkspaceBudget != "" {
		if a.Isolation == "" && a.WorkspaceID == "" {
			return errors.New("--workspace-budget requires a managed workspace")
		}
		_, err := parseWorkspaceBudget(a.WorkspaceBudget)
		return err
	}
	return nil
}

func parseWorkspaceBudget(value string) (time.Duration, error) {
	if value == "" {
		return workspace.DefaultBudget, nil
	}
	budget, err := time.ParseDuration(value)
	if err != nil || budget <= 0 {
		return 0, errors.New("workspace budget must be a positive finite duration")
	}
	return budget, nil
}

func parseWorkspaceArguments(a Arguments, rest []string) (Arguments, error) {
	var err error
	a.WorkspaceAction, rest, err = parseSupervisorAction(rest)
	if err != nil {
		return Arguments{}, errors.New("workspace requires list, status, exec, release, or forget")
	}
	flags, command := rest, []string(nil)
	if a.WorkspaceAction == "exec" {
		for index, argument := range rest {
			if argument == "--" {
				flags, command = rest[:index], rest[index+1:]
				break
			}
		}
		if len(command) == 0 || command[0] == "" {
			return Arguments{}, errors.New("workspace exec requires a command after --")
		}
		a.ExecArguments = append([]string(nil), command...)
	}
	fs := flag.NewFlagSet("workspace", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerWorkspaceFlags(fs, &a)
	watch := "0s"
	if a.WorkspaceAction == "status" {
		fs.StringVar(&watch, "watch", "0s", "finite workspace observation bound")
	}
	ordered, err := orderFlags(fs, flags)
	if err != nil {
		return Arguments{}, err
	}
	if err = fs.Parse(ordered); err != nil {
		return Arguments{}, err
	}
	if err = validateWorkspaceArguments(&a, fs.Args(), watch); err != nil {
		return Arguments{}, err
	}
	return a, nil
}

func registerWorkspaceFlags(fs *flag.FlagSet, a *Arguments) {
	fs.StringVar(&a.Root, "root", "", "absolute state root")
	fs.BoolVar(&a.JSON, "json", false, "emit versioned JSON")
	if a.WorkspaceAction == "list" {
		fs.StringVar(&a.WorkspaceAfter, "after", "", "workspace cursor from a previous page")
		fs.IntVar(&a.WorkspaceLimit, "limit", defaultWorkspacePageSize, "workspace summaries per page")
	}
	if a.WorkspaceAction == "status" {
		fs.StringVar(&a.WorkspaceOperationID, "operation", "", "exact workspace operation ID")
	}
	if a.WorkspaceAction == "exec" || a.WorkspaceAction == "release" || a.WorkspaceAction == "forget" {
		fs.StringVar(&a.WorkspaceBudget, "budget", "", "finite operation budget")
	}
	if a.WorkspaceAction == "release" {
		fs.BoolVar(&a.Discard, "discard", false, "accept loss of uncaptured checkout content")
	}
}

func validateWorkspaceArguments(a *Arguments, positional []string, watch string) error {
	switch a.WorkspaceAction {
	case "list":
		if err := validateWorkspaceListArguments(*a, positional); err != nil {
			return err
		}
	case "status", "exec", "release", "forget":
		if len(positional) != 1 {
			return errors.New("workspace command requires exactly one workspace ID")
		}
		a.WorkspaceID = positional[0]
		if err := task.ValidateTaskID(a.WorkspaceID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown workspace action %q", a.WorkspaceAction)
	}
	if a.WorkspaceOperationID != "" {
		if err := task.ValidateTaskID(a.WorkspaceOperationID); err != nil {
			return err
		}
	}
	if _, err := parseWorkspaceBudget(a.WorkspaceBudget); err != nil {
		return err
	}
	duration, err := time.ParseDuration(watch)
	if err != nil || duration < 0 {
		return errors.New("workspace watch must be a finite nonnegative duration")
	}
	a.Watch = duration
	for _, argument := range a.ExecArguments {
		if strings.ContainsRune(argument, 0) {
			return errors.New("workspace command arguments cannot contain NUL")
		}
	}
	return a.validate(nil, watch)
}

func validateWorkspaceListArguments(a Arguments, positional []string) error {
	if err := validateNoPositional("workspace list", positional); err != nil {
		return err
	}
	if a.WorkspaceAfter != "" {
		if err := task.ValidateTaskID(a.WorkspaceAfter); err != nil {
			return err
		}
	}
	if a.WorkspaceLimit < 1 || a.WorkspaceLimit > defaultWorkspacePageSize {
		return errors.New("workspace list limit must be between 1 and 50")
	}
	return nil
}
