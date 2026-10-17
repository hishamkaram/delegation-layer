package app

import (
	"strings"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func applyTimeoutContinuation(response *Response, root string, req *task.TaskRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, session *task.ProviderRefRecord, runner string, supervisor *task.SupervisorRef) {
	applyTimeoutContinuationWithRunnerState(response, root, req, records, catalog, session, runner, supervisor, true, false)
}

func applyTimeoutContinuationWithRunnerState(response *Response, root string, req *task.TaskRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, session *task.ProviderRefRecord, runner string, supervisor *task.SupervisorRef, runnerReleased, launchStateRecorded bool) {
	applyTimeoutContinuationWithRunnerStateAndCommandRunner(response, root, req, records, catalog, session, runner, runner, supervisor, runnerReleased, launchStateRecorded)
}

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
