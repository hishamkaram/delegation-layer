package app

import (
	"strings"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	"github.com/hishamkaram/delegation-layer/internal/config"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

const (
	preflightStatusReady       = "ready"
	preflightStatusBlocked     = "blocked"
	preflightStatusUnsupported = "unsupported"
)

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
