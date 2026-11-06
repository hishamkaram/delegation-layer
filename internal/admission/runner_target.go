package admission

import supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

// configuredRole comes from application installation configuration, never the
// task's custom runner or the command being checked. Unchanged native-owned
// images do not depend on another installation lookup.
func managedSuccessorTarget(configuredRole, recorded, selected string) (supervisorcore.RunnerRepairTarget, error) {
	if recorded == selected {
		return supervisorcore.RunnerRepairTarget{}, nil
	}
	installed, err := supervisorcore.ResolveRunner("", configuredRole)
	if err != nil {
		return supervisorcore.RunnerRepairTarget{}, err
	}
	return supervisorcore.PlanQueuedRunnerRepairTarget(installed)
}
