package app

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// defaultCheckpointPageSize is both the default and the largest list page.
const defaultCheckpointPageSize = 50

func registerCheckpointFlags(fs *flag.FlagSet, a *Arguments, watch *string) {
	fs.StringVar(&a.Root, "root", "", "absolute state root")
	fs.BoolVar(&a.JSON, "json", false, "emit versioned JSON")
	switch a.CheckpointAction {
	case "create":
		fs.StringVar(&a.Cwd, "cwd", "", "absolute ordinary Git checkout")
		fs.StringVar(&a.WorkspaceID, "workspace", "", "managed workspace ID")
		fs.StringVar(&a.CheckpointBudget, "budget", "", "finite helper budget (default: 5m)")
	case "status":
		fs.StringVar(&a.CheckpointOperationID, "operation", "", "acknowledged removal operation ID")
		fs.StringVar(watch, "watch", "0s", "finite checkpoint observation bound")
	case "list":
		fs.StringVar(&a.CheckpointAfter, "after", "", "checkpoint cursor from a previous page")
		fs.IntVar(&a.CheckpointLimit, "limit", defaultCheckpointPageSize, "checkpoints per page")
	case "remove":
		fs.StringVar(&a.CheckpointBudget, "budget", "", "finite helper budget (default: 5m)")
	}
}

func validateCheckpointArguments(a *Arguments, positional []string, watch string) error {
	var err error
	switch a.CheckpointAction {
	case "create":
		err = validateCheckpointCreation(*a, positional)
	case "status", "remove":
		err = a.validateCheckpointID(positional)
	case "list":
		err = validateCheckpointList(*a, positional)
	default:
		return fmt.Errorf("unknown checkpoint action %q", a.CheckpointAction)
	}
	if err != nil {
		return err
	}
	if a.CheckpointOperationID != "" {
		if err = task.ValidateTaskID(a.CheckpointOperationID); err != nil {
			return err
		}
	}
	if _, err = workspaces.ParseBudget(a.CheckpointBudget); err != nil {
		return err
	}
	duration, err := time.ParseDuration(watch)
	if err != nil || duration < 0 {
		return errors.New("checkpoint watch must be a finite nonnegative duration")
	}
	a.Watch = duration
	return a.validate(nil, watch)
}

// validateCheckpointCreation requires exactly one explicit target. No target
// is inferred from the current directory or from an omitted flag.
func validateCheckpointCreation(a Arguments, positional []string) error {
	if err := validateNoPositional("checkpoint create", positional); err != nil {
		return err
	}
	if (a.Cwd == "") == (a.WorkspaceID == "") {
		return errors.New("checkpoint create requires exactly one of --cwd ABS or --workspace ID")
	}
	if a.WorkspaceID != "" {
		return task.ValidateTaskID(a.WorkspaceID)
	}
	if !filepath.IsAbs(a.Cwd) {
		return errors.New("checkpoint create --cwd must be absolute")
	}
	return nil
}

func (a *Arguments) validateCheckpointID(positional []string) error {
	if len(positional) != 1 {
		return fmt.Errorf("checkpoint %s requires exactly one checkpoint ID", a.CheckpointAction)
	}
	a.CheckpointID = positional[0]
	return task.ValidateTaskID(a.CheckpointID)
}

func validateCheckpointList(a Arguments, positional []string) error {
	if err := validateNoPositional("checkpoint list", positional); err != nil {
		return err
	}
	if a.CheckpointAfter != "" {
		if err := task.ValidateTaskID(a.CheckpointAfter); err != nil {
			return err
		}
	}
	if a.CheckpointLimit < 1 || a.CheckpointLimit > defaultCheckpointPageSize {
		return errors.New("checkpoint list limit must be between 1 and 50")
	}
	return nil
}
