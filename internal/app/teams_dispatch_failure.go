package app

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/admission"
)

func teamDispatchExitCode(err error) (int, bool) {
	var staged *admission.StageError
	if !errors.As(err, &staged) || !teamDispatchStage(staged.Stage()) {
		return 0, false
	}
	return TeamsErrorExitCode(err), true
}

func teamDispatchStage(stage string) bool {
	return stage == "resolve-team-member" || stage == "validate-team-project"
}

func teamDispatchFailure(err error) DispatchFailureResponse {
	failure := teamsFailure(err)
	code := "team_selection_invalid"
	if TeamsErrorExitCode(err) == 1 {
		code = "team_catalog_unavailable"
	}
	return DispatchFailureResponse{Code: code, ReasonCode: failure.Code, NextAction: failure.NextAction}
}
