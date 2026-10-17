package tasklifecycle

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func Cancel(root string, td *taskdir.TaskDir, options pueue.Options, initialExecutable string, openControl ControlClientOpener) (result Cancellation, resultErr error) {
	inspection, err := td.Inspect()
	result.Inspection = inspection
	if err != nil {
		return result, err
	}
	if supervisorcore.TaskTerminal(inspection) {
		return result, nil
	}
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return result, err
	}
	submission, err := td.ReadSubmission()
	if err == nil {
		if !task.SameSupervisorIdentity(submission.Supervisor, meta.SupervisorConfig) {
			return result, task.ErrIdentityMismatch
		}
	} else if !task.RecordAbsent(err) {
		result.Failure = CancellationFailureSubmissionRead
		return result, err
	}
	// Persist the stop request first. The active lifetime owner handles native
	// termination; only an idle task lets this command stop the saved queue row.
	requestID, err := randomid.NewRandomID()
	if err != nil {
		return result, err
	}
	stop, stopErr := StopTask(root, td, requestID, "user", options, initialExecutable, openControl)
	if errors.Is(stopErr, task.ErrTerminalTask) {
		return terminalAfterCancelRace(result, td)
	}
	result.Stop = &stop
	if records, recordsErr := td.ReadStopRecords(); recordsErr == nil {
		result.Stops, result.StopsRead = records, true
	} else {
		stopErr = errors.Join(stopErr, recordsErr)
	}
	finalInspection, inspectErr := td.Inspect()
	if finalInspection != nil {
		result.Inspection = finalInspection
	}
	return result, errors.Join(stopErr, inspectErr)
}

func terminalAfterCancelRace(result Cancellation, td *taskdir.TaskDir) (Cancellation, error) {
	inspection, err := td.Inspect()
	if inspection != nil {
		result.Inspection = inspection
	}
	if err != nil {
		return result, err
	}
	if supervisorcore.TaskTerminal(inspection) {
		return result, nil
	}
	return result, task.ErrTerminalTask
}
