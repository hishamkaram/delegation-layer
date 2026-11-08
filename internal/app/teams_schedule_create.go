package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/config"
	schedulecommand "github.com/hishamkaram/delegation-layer/internal/scheduler/command"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func scheduleCommandWithTeams(a Arguments, deps Dependencies) (result commandResult) {
	if a.ScheduleAction == "create" || a.ScheduleAction == "resume" {
		if err := checkScheduleHostBeforeMutation(a, deps); err != nil {
			return failed(newResponse("schedule"), err, schedulerHostResultCode(err))
		}
		defer func() { result = finishScheduleHost(a, deps, result) }()
	}
	if a.ScheduleAction == "create" && requestedTeamTarget(a) != nil {
		return createTeamScheduleCommand(a, deps)
	}
	return scheduleCommand(a, deps.storeDependencies())
}

// Only requested choices and the catalog identity are saved. Member defaults
// are validated here, then resolved anew for each definitely new occurrence.
func createTeamScheduleCommand(a Arguments, deps Dependencies) commandResult {
	response := newResponse("schedule")
	if err := validateScheduleCreateShape(a); err != nil {
		return failed(response, err, 2)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	root, cwd, err := config.ValidateDirectories(root, a.Cwd)
	if err != nil {
		return failed(response, err, 2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulecommand.OperationTimeout)
	defer cancel()
	member, err := lookupTeamMember(ctx, *requestedTeamTarget(a), root, cwd, a.ProjectRoot, deps)
	if err != nil {
		return failed(response, err, TeamsErrorExitCode(err))
	}
	if _, err = teamcontract.MergeExecution(member.Member.ExecutionDefaults, task.TeamIntent{Provider: a.Provider, Auto: a.Auto, Config: a.Config}); err != nil {
		return failed(response, err, 2)
	}
	target := task.TeamTarget{Team: member.Record.Reference.Clone(), MemberID: member.Member.ID, CatalogID: member.CatalogID}
	a.teamTarget = &target
	return createScheduleCommand(a, deps.storeDependencies())
}
