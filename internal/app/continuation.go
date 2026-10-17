package app

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func continuationCommand(root, taskID, runner string, supervisor *task.SupervisorRef) string {
	command := "delegate --root " + shellQuote(root)
	if supervisor != nil && supervisor.ConfigPath != "" && !pueue.IsPrivateConfig(root, supervisor.ConfigPath) {
		command += " --pueue-config " + shellQuote(supervisor.ConfigPath)
	}
	if runner != "" {
		command += " --runner " + shellQuote(runner)
	}
	return command + " continue --task " + taskID + " --json"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
