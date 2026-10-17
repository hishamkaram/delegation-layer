package tasklifecycle

import (
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ContinuationConfig(requested, overrides task.TaskConfig) task.TaskConfig {
	requested.Interactive = requested.Interactive || overrides.Interactive
	if overrides.Budget != "" {
		requested.Budget = overrides.Budget
	}
	if overrides.Model != "" {
		requested.Model = overrides.Model
	}
	if overrides.Effort != "" {
		requested.Effort = overrides.Effort
	}
	if overrides.Budget == "" || requested.NativeTimeout == "" {
		return requested
	}
	budget, err := config.ParseBudget(requested.Budget)
	if err != nil {
		return requested
	}
	nativeTimeout, err := time.ParseDuration(requested.NativeTimeout)
	if err == nil && nativeTimeout > budget {
		requested.NativeTimeout = budget.String()
	}
	return requested
}

func ValidateContinuationEnvironment(meta *task.MetaRecord) error {
	if meta == nil || (!meta.EnvironmentRecorded && len(meta.Environment) == 0) {
		return errors.New("continuation unavailable: predecessor launch environment is not recorded")
	}
	return nil
}
