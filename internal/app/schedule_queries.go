package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

var errScheduleQueryClose = errors.New("schedule query resource close failed")

func scheduleQueryCommand(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("schedule")
	if a.ScheduleAction == "execution" {
		response.TaskID = a.TaskID
	} else {
		response.ScheduleHistory = &ScheduleHistoryResponse{ScheduleID: a.ScheduleID}
	}
	if err := validateScheduleRequest(a); err != nil {
		return failed(response, err, 2)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failedScheduleQuery(response, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), scheduleOperationTimeout)
	defer cancel()
	evidence, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.registry())
	if err != nil {
		return failedScheduleQuery(response, err)
	}
	defer func() { mergeScheduleQueryClose(&result, evidence.Close) }()
	response.RootID = evidence.RootID()
	store, err := scheduler.OpenReadOnly(ctx, evidence)
	if err != nil {
		return failedScheduleQuery(response, err)
	}
	defer func() { mergeScheduleQueryClose(&result, store.Close) }()
	if a.ScheduleAction == "history" {
		return scheduleHistory(ctx, store, a, response)
	}
	return scheduleExecution(ctx, store, evidence, a.TaskID, response)
}

func scheduleExecution(ctx context.Context, schedules contract.Store, evidence *taskdir.ReadOnlyStore, id string, response Response) commandResult {
	occurrence, err := schedules.GetOccurrence(ctx, id)
	if err != nil {
		return failedScheduleQuery(response, err)
	}
	if err = contract.ValidateOccurrence(occurrence); err != nil {
		return failedScheduleQuery(response, err)
	}
	if occurrence.TaskID != id {
		return failedScheduleQuery(response, task.ErrIdentityMismatch)
	}
	item := &ScheduleExecutionResponse{Occurrence: scheduleOccurrence(occurrence), NextAction: "inspect_task"}
	response.ScheduleExecution = item
	schedule, err := schedules.GetSchedule(ctx, occurrence.ScheduleID)
	if err != nil {
		return failedScheduleQuery(response, err)
	}
	if schedule.Definition.ScheduleID != occurrence.ScheduleID {
		return failedScheduleQuery(response, task.ErrIdentityMismatch)
	}
	facts, err := evidence.ReadTaskEvidence(ctx, id)
	if err != nil {
		if facts.Exists && !task.AllErrorCauses(err, func(cause error) bool {
			return errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
		}) {
			err = errors.Join(task.ErrEvidenceFault, err)
		}
		return failedScheduleQuery(response, err)
	}
	projection, err := scheduleExecutionEvidence(facts)
	if err != nil {
		return failedScheduleQuery(response, err)
	}
	item.LocalEvidence, item.Complete = &projection, true
	// The generic task axes derive from local evidence, never the cached row.
	response.Admission, response.Liveness, response.Publication = projection.Admission, projection.Liveness, projection.Publication
	item.NextAction = scheduleExecutionNextAction(projection)
	if err = writeJSON(io.Discard, response); err != nil {
		item.LocalEvidence, item.Complete = nil, false
		return failedScheduleQuery(response, err)
	}
	if err = writeHumanResponse(io.Discard, response); err != nil {
		item.LocalEvidence, item.Complete = nil, false
		return failedScheduleQuery(response, err)
	}
	return commandResult{response: response}
}

func scheduleExecutionNextAction(evidence ScheduleExecutionEvidenceResponse) string {
	if evidence.Publication == task.PublicationPending.String() {
		return "collect_task"
	}
	if evidence.Publication == task.PublicationCommitted.String() || evidence.Publication == task.PublicationRejected.String() {
		return "none"
	}
	return "inspect_task"
}

func failedScheduleQuery(response Response, err error) commandResult {
	code := scheduleQueryErrorCode(err)
	response.ScheduleQueryError = &ScheduleQueryErrorResponse{Code: code}
	message := fmt.Sprintf("schedule query failed (%s)", code)
	if code == "database_busy" {
		message += "; retry this read after database contention settles"
	}
	return failed(response, &fixedDetailPublicError{message: message, cause: err}, 1)
}

func mergeScheduleQueryClose(result *commandResult, closeFn func() error) {
	if closeErr := closeFn(); closeErr != nil {
		closeErr = errors.Join(errScheduleQueryClose, closeErr)
		*result = failedScheduleQuery(result.response, errors.Join(result.err, closeErr))
	}
}

func scheduleQueryErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrJSONResponseTooLarge):
		return "response_too_large"
	case errors.Is(err, task.ErrIdentityMismatch):
		return "identity_mismatch"
	case errors.Is(err, task.ErrUnsupportedSchema):
		return "unsupported_schema"
	case errors.Is(err, task.ErrEvidenceFault), errors.Is(err, task.ErrInvariantFault), errors.Is(err, task.ErrDuplicateKey), errors.Is(err, task.ErrTrailingJSON):
		return "invalid_evidence"
	case errors.Is(err, errScheduleQueryClose):
		// A failed cleanup is a concrete operational fault, not a busy read.
		return "query_failed"
	case onlyNotExist(err):
		return "not_found"
	case errors.Is(err, task.ErrLockBusy):
		return "database_busy"
	case errors.Is(err, context.DeadlineExceeded):
		return "query_timeout"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	default:
		return "query_failed"
	}
}
