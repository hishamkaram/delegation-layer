package app

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const maxWaitTasks = 32

func registerWaitFlags(fs *flag.FlagSet, a *Arguments, anyMode *bool) {
	fs.StringVar(&a.Root, "root", "", "absolute state root")
	fs.BoolVar(&a.JSON, "json", false, "emit versioned JSON")
	fs.BoolVar(anyMode, "any", false, "wait for any task (default)")
	fs.BoolVar(&a.WaitAll, "all", false, "wait for all tasks")
	fs.DurationVar(&a.Watch, "watch", 5*time.Minute, "finite observation duration")
}

func validateWaitArguments(a Arguments) error {
	if a.Root != "" && !filepath.IsAbs(a.Root) {
		return errors.New("wait --root must be absolute")
	}
	if a.Watch < 0 {
		return errors.New("wait --watch must not be negative")
	}
	if len(a.WaitTaskIDs) == 0 || len(a.WaitTaskIDs) > maxWaitTasks {
		return fmt.Errorf("wait requires between 1 and %d task IDs", maxWaitTasks)
	}
	seen := make(map[string]bool, len(a.WaitTaskIDs))
	for _, id := range a.WaitTaskIDs {
		if err := task.ValidateTaskID(id); err != nil {
			return fmt.Errorf("wait requires each valid task ID as its own argument: %w", err)
		}
		if seen[id] {
			return errors.New("wait task IDs must be unique")
		}
		seen[id] = true
	}
	return nil
}
