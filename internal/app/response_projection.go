package app

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func newResponse(command string) Response {
	response := Response{SchemaVersion: OutputSchemaVersion, Command: command, Admission: task.AdmissionUnknown.String(), Liveness: task.LivenessUndetermined.String(), Publication: task.PublicationUnknown.String()}
	if command == "providers" || command == "capabilities" {
		response.WorkspaceSupport = workspaceSupport()
	}
	return response
}

func (r *Response) setError(err error) {
	if err != nil {
		r.Error = err.Error()
	}
}

func statusStrings(insp *taskdir.TaskInspection) (publication string, outcome *task.OutcomeRecord) {
	publication = insp.Publication.String()
	if insp.Outcome != nil {
		copy := *insp.Outcome
		outcome = &copy
	}
	return publication, outcome
}

func fillInspection(response *Response, insp *taskdir.TaskInspection) {
	if insp == nil {
		return
	}
	response.Publication, response.Outcome = statusStrings(insp)
	if insp.Outcome != nil {
		payload := insp.Outcome.Payload
		response.Payload = &payload
		response.EvidenceSHA256 = insp.Outcome.EvidenceSHA256
		applyOutputDiagnostics(response, insp.Outcome.Diagnostics)
	}
}

func applyOutputDiagnostics(response *Response, diagnostics []task.OutputDiagnostic) {
	if response == nil || len(diagnostics) == 0 {
		return
	}
	response.Diagnostics = task.CloneOutputDiagnostics(diagnostics)
	rejected := response.Outcome != nil && response.Outcome.Verdict == task.VerdictRejected
	if response.Recheck != nil {
		rejected = response.Recheck.Status == task.RecheckStatusRejected
	}
	if rejected && response.Error == "" {
		first := diagnostics[0]
		response.Error = first.Code + ": " + first.Message
	}
}

func classifyCode(err error, fallback int) int {
	if err == nil {
		return 0
	}
	for _, candidate := range []error{config.ErrInvalidBudget, config.ErrInvalidMode, config.ErrUnsupportedProvider, config.ErrInvalidCwd, config.ErrRootWorkspaceOverlap, config.ErrTokenBudgetRejected, config.ErrRawArgvRejected, config.ErrBriefNotFound, config.ErrBriefNotRegular, config.ErrBriefTooLarge, task.ErrInvalidTaskID, task.ErrInvalidRootID, task.ErrRequestConflict, task.ErrIdentityMismatch, task.ErrInvalidPermit, task.ErrIncompatiblePredicate, pueue.ErrConfiguration} {
		if errors.Is(err, candidate) {
			return 2
		}
	}
	if errors.Is(err, pueue.ErrBinding) || errors.Is(err, inspection.ErrAdmissionExpired) {
		return 1
	}
	if errors.Is(err, ErrProfileUnavailable) {
		return 2
	}
	return fallback
}

func describeStopRecord(record taskdir.StopRecord) StopResponse {
	result := StopResponse{}
	if record.Request != nil {
		result.RequestID = record.Request.RequestID
		result.Cause = record.Request.Cause
		result.Requested = true
		result.NumericTaskID = copyTaskID(record.Request.NumericTaskID)
	}
	if record.Reply != nil {
		result.Action = record.Reply.Action
		result.Acknowledged = boolPointer(record.Reply.Acknowledged)
		result.Message = record.Reply.Message
		result.Attempted = true
	}
	if record.Observation != nil {
		result.ObservedState = record.Observation.State
		result.Terminated = record.Observation.Terminated
	}
	return result
}

func boolPointer(value bool) *bool {
	return &value
}
