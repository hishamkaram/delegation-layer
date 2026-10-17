package app

import (
	"strings"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

const (
	preflightStatusReady       = "ready"
	preflightStatusBlocked     = "blocked"
	preflightStatusUnsupported = "unsupported"
)

// selectAutoProvider performs provider-owned static admission before a task is
// created. The selected provider is dispatched once by the normal path, which
// performs the stronger supervised runtime probe. A runtime failure after
// selection is returned to the caller; this function never silently retries a
// dispatched task with another provider.
func selectAutoProvider(deps Dependencies, root, rootID, taskID, cwd string, requested task.TaskConfig, brief []byte) (string, error) {
	return admissioncore.SelectAutoProvider(staticAdmissionOptions(deps), root, rootID, taskID, cwd, requested, brief)
}

func preflight(a Arguments, deps Dependencies) commandResult {
	response := newResponse("preflight")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	_, cwd, err := config.ValidateDirectories(root, a.Cwd)
	if err != nil {
		return failed(response, err, 2)
	}
	request, err := admissioncore.BuildRequest(strings.Repeat("0", 32), strings.Repeat("1", 32), a.Provider, cwd, a.Config, []byte("preflight"), nil)
	if err != nil {
		return failed(response, err, 2)
	}
	observed, err := admissioncore.Preflight(staticAdmissionOptions(deps), root, request)
	return preflightCommandResult(response, observed, err)
}
