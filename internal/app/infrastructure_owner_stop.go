package app

import (
	"fmt"
	"strings"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// SupervisorOwnerResponse describes only the selected infrastructure owner.
// Its serving release does not report Pueue or task termination.
type SupervisorOwnerResponse struct {
	Action string `json:"action"`
	infrastructure.OwnerStopResult
}

func supervisorOwnerStop(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("supervisor")
	response.SupervisorOwner = &SupervisorOwnerResponse{Action: "stop-owner", OwnerStopResult: infrastructure.OwnerStopResult{State: infrastructure.OwnerStopUnknown, ProcessClosure: infrastructure.OwnerProcessUnknown}}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, publicSupervisorError("infrastructure owner state root is unavailable", err), 1)
	}
	ctx, cancel := supervisorcore.ControlContext(supervisorControlOptions(deps))
	defer cancel()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if onlyNotExist(err) {
		response.SupervisorOwner.State = infrastructure.OwnerNotInitialized
		return commandResult{response: response}
	}
	if err != nil {
		return failed(response, publicSupervisorError("infrastructure owner state root is unavailable", err), 1)
	}
	defer func() { mergeCommandClose(&result, reader.Close) }()
	response.RootID = reader.RootID()
	observed, err := infrastructure.StopOwnerServing(ctx, reader)
	response.SupervisorOwner.OwnerStopResult = observed
	if err != nil {
		return failed(response, publicSupervisorError("infrastructure owner stop reported a control or observation fault", err), 1)
	}
	if observed.State == infrastructure.OwnerStopBusy {
		return commandResult{response: response, code: 3}
	}
	return commandResult{response: response}
}

func writeHumanSupervisorOwner(output *strings.Builder, owner *SupervisorOwnerResponse) {
	if owner == nil {
		return
	}
	fmt.Fprintf(output, "supervisor_owner action=%s state=%s process_closure=%s", owner.Action, owner.State, owner.ProcessClosure)
	if owner.Startup.Sequence != 0 {
		fmt.Fprintf(output, " startup_sequence=%d startup_epoch=%s", owner.Startup.Sequence, owner.Startup.Epoch)
	}
	output.WriteByte('\n')
}
