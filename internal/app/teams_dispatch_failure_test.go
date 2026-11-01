package app

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/teams"
)

func TestTeamDispatchSeparatesInvalidSelectionAndOperationalFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		code    int
		failure string
	}{
		{"deleted member", teams.ErrNotFound, 2, "team_selection_invalid"},
		{"wrong project", teams.ErrProjectMismatch, 2, "team_selection_invalid"},
		{"catalog IO", teams.ErrUnavailable, 1, "team_catalog_unavailable"},
		{"catalog schema", teams.ErrSchema, 1, "team_catalog_unavailable"},
		{"joined validation and IO", errors.Join(task.ErrInvalidTeamSelection, &os.PathError{Op: "open", Path: "redacted", Err: io.ErrUnexpectedEOF}), 1, "team_catalog_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := dispatchCommandResult(tasklifecycle.DispatchResult{Record: tasklifecycle.RecordAbsent, Phase: tasklifecycle.DispatchValidate, Failure: admission.WithStage("resolve-team-member", tc.err)})
			response := jsonResponseFor(t, "dispatch", result)
			if result.code != tc.code || response.Failure == nil || response.Failure.Code != tc.failure || response.Failure.ReasonCode == "" {
				t.Fatalf("wrong failure: %+v", result)
			}
			if result.response.Team != nil {
				t.Fatal("failed selection advertised admitted Team")
			}
		})
	}
}
