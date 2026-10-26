package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

const environmentConfigKey = "DELEGATE_PUEUE_CONFIG"

// EnvironmentConfigPath returns the DELEGATE_PUEUE_CONFIG value of one
// captured launch scope. As with the process environment, the first entry
// wins and an absent key is empty.
func EnvironmentConfigPath(environment []string) string {
	return verifiedexec.EnvironmentValue(environment, environmentConfigKey)
}

// ContinuationConfigEnvironment returns the DELEGATE_PUEUE_CONFIG value of
// the caller-selected launch scope that the environment argument carries for
// the new successor task. A private predecessor binding suppresses that value,
// so a successor never retargets an external configuration away from the
// predecessor's own private supervisor.
func ContinuationConfigEnvironment(root string, predecessor task.MetaRecord, environment []string) string {
	if pueue.IsPrivateConfig(root, predecessor.SupervisorConfig.ConfigPath) {
		return ""
	}
	return EnvironmentConfigPath(environment)
}
