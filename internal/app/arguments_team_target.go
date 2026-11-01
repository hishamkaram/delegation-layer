package app

import (
	"errors"
	"flag"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func registerTeamTargetFlags(fs *flag.FlagSet, a *Arguments) {
	fs.StringVar(&a.TeamScope, "team-scope", "", "optional Team scope: user or project")
	fs.StringVar(&a.TeamID, "team", "", "explicit Team ID")
	fs.StringVar(&a.MemberID, "member", "", "explicit Member ID")
	fs.StringVar(&a.ProjectRoot, "project-root", "", "explicit non-Git project root")
}

func requestedTeamTarget(a Arguments) *task.TeamTarget {
	if a.teamTarget != nil {
		target := a.teamTarget.Clone()
		return &target
	}
	if a.TeamScope == "" && a.TeamID == "" && a.MemberID == "" {
		return nil
	}
	return &task.TeamTarget{Team: task.TeamReference{Scope: a.TeamScope, TeamID: a.TeamID}, MemberID: a.MemberID}
}

func validateTeamTargetInputs(a Arguments) error {
	target := requestedTeamTarget(a)
	if target == nil {
		if a.ProjectRoot != "" {
			return errors.New("--project-root requires an explicit project-scoped Team target")
		}
		return nil
	}
	if a.ResumeTask != "" {
		return errors.New("continuation inherits its Team selection; new targeting flags are not allowed")
	}
	if target.Team.Scope != "user" && target.Team.Scope != "project" {
		return errors.New("team targeting requires --team-scope user|project, --team ID and --member ID")
	}
	if err := task.ValidateTeamID(target.Team.TeamID); err != nil {
		return err
	}
	if err := task.ValidateTeamID(target.MemberID); err != nil {
		return err
	}
	if a.ProjectRoot != "" && (target.Team.Scope != "project" || !filepath.IsAbs(a.ProjectRoot)) {
		return errors.New("--project-root must be absolute and requires project scope")
	}
	return nil
}
