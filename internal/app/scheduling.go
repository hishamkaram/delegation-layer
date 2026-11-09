package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	schedulecommand "github.com/hishamkaram/delegation-layer/internal/scheduler/command"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/recurrence"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// prepareScheduleCreate validates only input and filesystem boundaries. The
// caller supplies clock and ID sources; neither providers nor storage are used.
func prepareScheduleCreate(a Arguments, now func() time.Time, newID func() (string, error)) (schedulecommand.CreateInput, error) {
	if err := validateScheduleCreateShape(a); err != nil {
		return schedulecommand.CreateInput{}, err
	}
	if err := a.applyReviewTarget(); err != nil {
		return schedulecommand.CreateInput{}, schedulecommand.InvalidInput(err)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return schedulecommand.CreateInput{}, err
	}
	root, cwd, err := config.ValidateDirectories(root, a.Cwd)
	if err != nil {
		return schedulecommand.CreateInput{}, classifyScheduleDirectoryError(err)
	}
	request := contract.Request{Provider: a.Provider, Auto: a.Auto, CanonicalCwd: cwd, Config: a.Config, Team: a.teamTarget}
	if request.Config.ReviewTarget != nil {
		target := *request.Config.ReviewTarget
		request.Config.ReviewTarget = &target
	}
	if _, err = contract.MarshalRequest(request); err != nil {
		return schedulecommand.CreateInput{}, schedulecommand.InvalidInput(err)
	}
	brief, err := readScheduleBrief(a.Brief, tasklifecycle.ReadBrief)
	if err != nil {
		return schedulecommand.CreateInput{}, err
	}
	id, createdAt, err := captureScheduleIdentity(a.ScheduleID, now, newID)
	if err != nil {
		return schedulecommand.CreateInput{}, err
	}
	return schedulecommand.CreateInput{
		Root: root, ID: id, CreatedAt: createdAt, Cron: a.ScheduleCron,
		Timezone: a.ScheduleTimezone, TimezoneSet: a.ScheduleTimezoneSet, Request: request, Brief: brief, Hook: a.ScheduleHook,
	}, nil
}

func validateScheduleCreateShape(a Arguments) error {
	if a.Brief == "" || !filepath.IsAbs(a.Cwd) {
		return schedulecommand.InvalidInput(errors.New("schedule create requires --brief FILE and --cwd ABS"))
	}
	if a.ScheduleCron == "" {
		return schedulecommand.InvalidInput(errors.New("schedule create requires --cron RULE"))
	}
	if a.ScheduleTimezoneSet && a.ScheduleTimezone == "" {
		return schedulecommand.InvalidInput(errors.New("schedule --timezone must not be empty"))
	}
	return nil
}

func classifyScheduleDirectoryError(err error) error {
	if !errors.Is(err, fs.ErrPermission) && (errors.Is(err, config.ErrInvalidCwd) || errors.Is(err, config.ErrRootWorkspaceOverlap)) {
		return schedulecommand.InvalidInput(err)
	}
	return err
}

func readScheduleBrief(path string, readBrief func(string) ([]byte, error)) ([]byte, error) {
	briefPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	length, err := config.ValidateBriefFile(briefPath)
	if err != nil {
		return nil, classifyScheduleBriefError(err)
	}
	if length == 0 {
		return nil, schedulecommand.InvalidInput(config.ErrBriefEmpty)
	}
	brief, err := readBrief(briefPath)
	return brief, classifyScheduleBriefError(err)
}

func classifyScheduleBriefError(err error) error {
	if scheduleBriefInputError(err) {
		return schedulecommand.InvalidInput(err)
	}
	return err
}

// scheduleBriefInputError excludes joined operational faults from invalid-input guidance.
func scheduleBriefInputError(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !scheduleBriefInputError(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return scheduleBriefInputError(wrapped.Unwrap())
	}
	return errors.Is(err, config.ErrBriefNotFound) || errors.Is(err, config.ErrBriefNotRegular) || errors.Is(err, config.ErrBriefTooLarge) || errors.Is(err, config.ErrBriefEmpty) || errors.Is(err, task.ErrControlRecordTooBig)
}

func captureScheduleIdentity(requested string, now func() time.Time, newID func() (string, error)) (string, time.Time, error) {
	id := requested
	if id == "" {
		var err error
		id, err = newID()
		if err != nil {
			return "", time.Time{}, fmt.Errorf("allocate schedule ID: %w", err)
		}
	}
	if err := task.ValidateTaskID(id); err != nil {
		return "", time.Time{}, schedulecommand.InvalidInput(err)
	}
	createdAt := now().UTC()
	if err := contract.ValidateInstant("schedule created_at", createdAt); err != nil {
		return "", time.Time{}, err
	}
	return id, createdAt, nil
}

func createScheduleCommand(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("schedule")
	input, err := prepareScheduleCreate(a, time.Now, randomid.NewTaskID)
	if err != nil {
		code := 1
		if errors.Is(err, schedulecommand.ErrInvalidInput) {
			code = 2
		}
		return failed(response, err, code)
	}
	response.Schedule = &ScheduleResponse{ID: input.ID}
	definition, warnings, err := schedulecommand.BuildDefinition(input)
	if err != nil {
		code := 1
		if !errors.Is(err, recurrence.ErrLookup) && !errors.Is(err, recurrence.ErrRangeExhausted) && (errors.Is(err, recurrence.ErrInvalidExpression) || errors.Is(err, recurrence.ErrInvalidTimezone) || errors.Is(err, recurrence.ErrLocalTimezone) || errors.Is(err, schedulecommand.ErrInvalidInput)) {
			code = 2
		}
		return failed(response, err, code)
	}
	for _, warning := range warnings {
		response.ScheduleWarnings = append(response.ScheduleWarnings, ScheduleWarning{Code: warning.Code, Message: warning.Message})
	}
	response.Schedule.State = contract.StateEnabled
	if err = writeJSON(io.Discard, response); err != nil {
		return failed(response, err, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulecommand.OperationTimeout)
	defer cancel()
	root, err := openStoreContext(ctx, input.Root, deps, true)
	if err != nil {
		response.Schedule.State = ""
		return failed(response, err, 1)
	}
	defer func() { mergeScheduleClose(&result, root.Close) }()
	response.RootID = root.RootID
	store, err := scheduler.OpenOrInitialize(ctx, root)
	if err != nil {
		response.Schedule.State = ""
		return failed(response, err, 1)
	}
	defer func() { mergeScheduleClose(&result, store.Close) }()
	if err = writeJSON(io.Discard, response); err != nil {
		response.Schedule.State = ""
		return failed(response, err, 1)
	}
	return createSchedule(ctx, store, definition, response)
}

func createSchedule(ctx context.Context, store contract.Store, definition contract.Definition, response Response) commandResult {
	match, err := schedulecommand.Create(ctx, store, definition)
	if err != nil {
		response.Schedule.State = ""
		response.Schedule.NextAction = "inspect_schedule"
		response.Schedule.DefinitionMatched = match
		return failed(response, err, 1)
	}
	return commandResult{response: response}
}

func mergeScheduleClose(result *commandResult, closeFn func() error) {
	mergeCommandClose(result, closeFn)
	if result.code == 1 && result.response.Schedule != nil && (result.response.Schedule.State == contract.StateEnabled || result.response.Schedule.NextAction == "inspect_schedule") {
		result.response.Schedule.NextAction = "inspect_schedule"
	}
}

func scheduleCommand(a Arguments, deps storeDependencies) (result commandResult) {
	if a.ScheduleAction == "create" {
		return createScheduleCommand(a, deps)
	}
	if a.ScheduleAction == "history" || a.ScheduleAction == "execution" || a.ScheduleAction == "health" || (a.ScheduleAction == "policy" && a.SchedulePolicyOperation == "get") {
		return scheduleQueryCommand(a, deps)
	}
	response := newResponse("schedule")
	if err := validateScheduleRequest(a); err != nil {
		return failed(response, err, 2)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulecommand.OperationTimeout)
	defer cancel()
	store, err := openStoreContext(ctx, root, deps, false)
	if err != nil {
		if missingScheduleRoot(root, err) {
			return missingScheduleResult(a, response)
		}
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	schedules, err := scheduler.Open(ctx, store)
	if err != nil {
		if onlyNotExist(err) {
			return missingScheduleResult(a, response)
		}
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, schedules.Close) }()
	return executeStoredScheduleCommand(ctx, schedules, a, response)
}

func executeStoredScheduleCommand(ctx context.Context, schedules contract.Store, a Arguments, response Response) commandResult {
	switch a.ScheduleAction {
	case "list":
		return listSchedules(ctx, schedules, a, response)
	case "show":
		return showSchedule(ctx, schedules, a.ScheduleID, response)
	case "policy":
		return schedulePolicy(ctx, schedules, a, response)
	case "pause", "resume", "delete":
		return changeScheduleState(ctx, schedules, a, response, time.Now().UTC())
	default:
		return failed(response, errors.New("unknown schedule action"), 2)
	}
}

func validateScheduleRequest(a Arguments) error {
	if a.ScheduleAction == "list" {
		return contract.ValidateSchedulePage(contract.SchedulePage{AfterScheduleID: a.ScheduleAfter, Limit: a.ScheduleLimit})
	}
	if a.ScheduleAction == "history" {
		_, err := scheduleHistoryPage(a)
		return err
	}
	if a.ScheduleAction == "execution" {
		return task.ValidateTaskID(a.TaskID)
	}
	return task.ValidateTaskID(a.ScheduleID)
}

func missingScheduleRoot(root string, err error) bool {
	if !onlyNotExist(err) {
		return false
	}
	_, statErr := os.Lstat(root)
	return onlyNotExist(statErr)
}

// onlyNotExist checks every leaf, so a joined close/storage fault is retained.
func onlyNotExist(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, fs.ErrNotExist) })
}

func missingScheduleResult(a Arguments, response Response) commandResult {
	if a.ScheduleAction == "list" {
		rows := []ScheduleListRow{}
		response.Schedules = &rows
		return commandResult{response: response}
	}
	response.Schedule = &ScheduleResponse{ID: a.ScheduleID}
	return failed(response, fmt.Errorf("schedule %s: %w", a.ScheduleID, fs.ErrNotExist), 1)
}

func listSchedules(ctx context.Context, store contract.Store, a Arguments, response Response) commandResult {
	page, err := store.ListSchedules(ctx, contract.SchedulePage{AfterScheduleID: a.ScheduleAfter, Limit: a.ScheduleLimit})
	if err != nil {
		return failed(response, err, 1)
	}
	rows := make([]ScheduleListRow, 0, len(page.Schedules))
	for _, item := range page.Schedules {
		rows = append(rows, scheduleListRow(item))
	}
	response.Schedules = &rows
	if page.More && len(rows) > 0 {
		response.ScheduleCursor = rows[len(rows)-1].ID
	}
	if err := writeJSON(io.Discard, response); err != nil {
		return failed(newResponse("schedule"), err, 1)
	}
	return commandResult{response: response}
}

func showSchedule(ctx context.Context, store contract.Store, id string, response Response) commandResult {
	response.Schedule = &ScheduleResponse{ID: id}
	item, err := store.GetSchedule(ctx, id)
	if err != nil {
		return failed(response, err, 1)
	}
	projection, err := scheduleShow(item)
	if err != nil {
		return failed(response, err, 1)
	}
	response.Schedule = &projection
	if err = writeJSON(io.Discard, response); err != nil {
		response.Schedule = &ScheduleResponse{ID: id}
		return failed(response, err, 1)
	}
	if err = writeHumanResponse(io.Discard, response); err != nil {
		response.Schedule = &ScheduleResponse{ID: id}
		return failed(response, err, 1)
	}
	return commandResult{response: response}
}

func changeScheduleState(ctx context.Context, store contract.Store, a Arguments, response Response, now time.Time) commandResult {
	response.Schedule = &ScheduleResponse{ID: a.ScheduleID}
	current, err := store.GetSchedule(ctx, a.ScheduleID)
	if err != nil {
		return failed(response, err, 1)
	}
	if resumed, ok := resumeHeldSchedule(ctx, store, a, current, response, now); ok {
		return resumed
	}

	change, warning, err := schedulecommand.PlanStateChange(current, a.ScheduleAction, now)
	if err != nil {
		return failed(response, err, 2)
	}
	response.Schedule.State = change.To
	if warning != nil {
		response.ScheduleWarnings = []ScheduleWarning{{Code: warning.Code, Message: warning.Message}}
	}
	if err = writeJSON(io.Discard, response); err != nil {
		return failed(response, err, 1)
	}
	observed, err := schedulecommand.ApplyStateChange(ctx, store, change)
	if err != nil {
		response.Schedule.NextAction = "inspect_schedule"
		response.Schedule.State = observed
		return failed(response, err, 1)
	}

	return commandResult{response: response}
}

func scheduleHistoryPage(a Arguments) (contract.HistoryPage, error) {
	page := contract.HistoryPage{ScheduleID: a.ScheduleID, Limit: a.ScheduleLimit}
	if a.ScheduleBefore != "" {
		before, err := time.Parse(time.RFC3339Nano, a.ScheduleBefore)
		if err != nil {
			return page, fmt.Errorf("history --before must be RFC3339Nano: %w", err)
		}
		before = before.UTC()
		page.Before = &before
	}
	return page, contract.ValidateHistoryPage(page)
}

func scheduleHistory(ctx context.Context, store contract.Store, a Arguments, response Response) commandResult {
	query, err := scheduleHistoryPage(a)
	if err != nil {
		return failed(response, err, 2)
	}
	response.ScheduleHistory = &ScheduleHistoryResponse{ScheduleID: a.ScheduleID}
	page, err := store.History(ctx, query)
	if err != nil {
		return failedScheduleQuery(response, err)
	}
	response.ScheduleHistory.Entries = make([]ScheduleHistoryEntry, 0, len(page.Entries))
	for _, entry := range page.Entries {
		row, err := scheduleHistoryEntry(a.ScheduleID, entry)
		if err != nil {
			return failedScheduleQuery(response, err)
		}
		response.ScheduleHistory.Entries = append(response.ScheduleHistory.Entries, row)
	}
	if next, more, err := page.Next(query); err != nil {
		return failedScheduleQuery(response, err)
	} else if more {
		response.ScheduleHistory.NextBefore = next.Before.Format(time.RFC3339Nano)
	}
	if err := writeJSON(io.Discard, response); err != nil {
		response.ScheduleHistory.Entries = nil
		response.ScheduleHistory.NextBefore = ""
		return failedScheduleQuery(response, err)
	}
	return commandResult{response: response}
}

var errScheduleQueryClose = errors.New("schedule query resource close failed")

func scheduleQueryCommand(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("schedule")
	switch a.ScheduleAction {
	case "execution":
		response.TaskID = a.TaskID
	case "history":
		response.ScheduleHistory = &ScheduleHistoryResponse{ScheduleID: a.ScheduleID}
	}
	if err := validateScheduleRequest(a); err != nil {
		return failed(response, err, 2)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failedScheduleQuery(response, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulecommand.OperationTimeout)
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
	if a.ScheduleAction == "health" {
		return scheduleHealth(ctx, store, a.ScheduleID, response)
	}
	if a.ScheduleAction == "policy" {
		return schedulePolicy(ctx, store, a, response)
	}
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
	if projection.TaskPresence == "absent" {
		item.NextAction = "inspect_schedule"
		if occurrence.Attempt != nil {
			item.NextAction = occurrence.Attempt.NextAction
		}
	}
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
	if code == "migration_required" {
		message += "; stop the scheduler and run delegate scheduler migrate for the same state root"
	}
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
	case errors.Is(err, contract.ErrMigrationRequired):
		return "migration_required"
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

// inspectScheduleHost opens only existing metadata and the authenticated host
// endpoint. It never probes H or initializes scheduler/supervisor resources.
func inspectScheduleHost(ctx context.Context, a Arguments, deps Dependencies) (observed SchedulerResponse, err error) {
	observed.State = "no_host_observed"
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return observed, err
	}
	self, err := schedulerExecutable()
	if err != nil {
		return observed, err
	}
	observed.ServeCommand = []string{self, "--root", root, "scheduler", "serve"}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if onlyNotExist(err) {
		return observed, nil
	}
	if err != nil {
		return observed, err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	status, err := host.Observe(ctx, host.Query{Store: reader})
	command := observed.ServeCommand
	observed = projectSchedulerStatus(status)
	observed.ServeCommand = command
	if err != nil {
		return observed, err
	}
	if status.State != "no_host_observed" && status.State != "stopped" {
		err = host.CheckCompatible(status)
	}
	return observed, err
}

func checkScheduleHostBeforeMutation(a Arguments, deps Dependencies) error {
	ctx, cancel := context.WithTimeout(context.Background(), schedulecommand.OperationTimeout)
	defer cancel()
	_, err := inspectScheduleHost(ctx, a, deps)
	return err
}

func finishScheduleHost(a Arguments, deps Dependencies, result commandResult) commandResult {
	if result.code != 0 || result.err != nil || result.response.Schedule == nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulecommand.OperationTimeout)
	defer cancel()
	observed, err := inspectScheduleHost(ctx, a, deps)
	return savedScheduleHostResult(result, observed, err)
}

// savedScheduleHostResult projects host readiness only after a successful
// definition mutation. It never creates a host or changes the saved definition.
func savedScheduleHostResult(result commandResult, observed SchedulerResponse, observationErr error) commandResult {
	if result.err != nil || result.code != 0 || result.response.Schedule == nil {
		return result
	}
	result.response.Schedule.Saved = true
	result.response.Scheduler = &observed
	if observationErr != nil {
		if code := schedulerHostResultCode(observationErr); code != 3 {
			return failed(result.response, observationErr, code)
		}
		result.code = 3
		result.response.Scheduler.NextAction = "inspect_scheduler"
		result.response.Pending = &PendingResponse{Kind: "scheduler"}
		return result
	}
	if observed.Fault {
		return failed(result.response, fmt.Errorf("scheduler host fault: %s", observed.LastError), 1)
	}
	if observed.State == "ready" && observed.Ready {
		return result
	}
	result.code = 3
	if observed.State == "no_host_observed" || observed.State == "stopped" {
		result.response.Schedule.NextAction = "start_required"
		result.response.Scheduler.NextAction = "start_required"
		result.response.Pending = &PendingResponse{Kind: "start_required"}
		return result
	}
	result.response.Scheduler.NextAction = "inspect_scheduler"
	result.response.Pending = &PendingResponse{Kind: "scheduler"}
	return result
}

func resumeHeldSchedule(ctx context.Context, store contract.Store, a Arguments, current contract.Schedule, response Response, now time.Time) (commandResult, bool) {
	if a.ScheduleAction != "resume" {
		return commandResult{}, false
	}
	reliability, ok := store.(contract.ReliabilityStore)
	if !ok {
		return commandResult{}, false
	}
	hold, err := reliability.QuotaHold(ctx, a.ScheduleID)
	if err != nil {
		return failed(response, err, 1), true
	}
	if current.State != contract.StatePaused && hold == nil {
		return commandResult{}, false
	}
	if err = reliability.ResumeWithHold(ctx, a.ScheduleID, now); err != nil {
		return failed(response, err, 1), true
	}
	response.Schedule.State = contract.StateEnabled
	if now.Before(current.Cursor) {
		response.ScheduleWarnings = []ScheduleWarning{{Code: "clock_behind_cursor", Message: "Clock is behind the schedule cursor; the cursor was preserved."}}
	}
	return commandResult{response: response}, true
}
