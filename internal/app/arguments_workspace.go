package app

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
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
		_, err := workspaces.ParseBudget(a.WorkspaceBudget)
		return err
	}
	return nil
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
	if _, err := workspaces.ParseBudget(a.WorkspaceBudget); err != nil {
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
