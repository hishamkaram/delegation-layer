package supervisor

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const environmentConfigKey = "DELEGATE_PUEUE_CONFIG"

// EnvironmentConfigPath returns the DELEGATE_PUEUE_CONFIG value of one
// captured launch scope. As with the process environment, the first entry
// wins and an absent key is empty.
func EnvironmentConfigPath(environment []string) string {
	for _, entry := range environment {
		if key, value, ok := strings.Cut(entry, "="); ok && key == environmentConfigKey {
			return value
		}
	}
	return ""
}

// ContinuationConfigEnvironment returns the DELEGATE_PUEUE_CONFIG value in
// effect inside a predecessor's selected launch scope. A private predecessor
// binding suppresses that value, so a successor never retargets an external
// configuration away from the predecessor's own private supervisor.
func ContinuationConfigEnvironment(root string, predecessor task.MetaRecord, environment []string) string {
	if pueue.IsPrivateConfig(root, predecessor.SupervisorConfig.ConfigPath) {
		return ""
	}
	return EnvironmentConfigPath(environment)
}
