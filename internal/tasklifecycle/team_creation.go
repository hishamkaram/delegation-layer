package tasklifecycle

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func teamCreationConflict(err error) bool {
	if err == nil {
		return false
	}
	return task.AllErrorCauses(err, func(leaf error) bool {
		return errors.Is(leaf, task.ErrRequestConflict)
	})
}

func rebaseExistingTeam(td *taskdir.TaskDir, candidate *task.TaskRecord) error {
	if candidate.Team == nil {
		return nil
	}
	old, err := td.RecordedRequest()
	if err != nil {
		return err
	}
	return rebaseTeamRequest(candidate, old)
}

// The losing prepared supervisor/profile has closed before this function runs.
// Only the existing saved-request owner may recover or submit the winner.
func adoptCreatedTeamWinner(request DispatchRequest, options DispatchOptions, store *taskdir.Store, candidate task.TaskRecord, result DispatchResult) DispatchResult {
	td, err := store.OpenTask(candidate.TaskID)
	if err != nil {
		result.Creation.Failure = errors.Join(result.Creation.Failure, err)
		return result
	}
	old, _, err := td.PreparedRecords()
	if err == nil {
		err = rebaseTeamRequest(&candidate, old)
	}
	if err != nil {
		result.Creation.Failure = errors.Join(result.Creation.Failure, err, td.Close())
		return result
	}
	existing := DispatchExisting(ExistingTaskOptions{Creation: options.Creation, Selection: request.Selection, RequestedBudget: request.Config.Budget, BriefPath: request.BriefPath}, store, td, candidate)
	result.Record, result.Provider = RecordCreated, candidate.Provider
	result.Team = candidate.Team.Clone()
	result.Creation, result.Resources, result.Existing = nil, nil, &existing
	return result
}
