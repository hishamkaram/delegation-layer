package app

import (
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ScheduleExecutionResponse separates the durable scheduler reservation from
// current validated local task evidence. It makes no live OS or queue claim.
type ScheduleExecutionResponse struct {
	Occurrence    ScheduleOccurrenceResponse         `json:"occurrence"`
	Complete      bool                               `json:"complete"`
	LocalEvidence *ScheduleExecutionEvidenceResponse `json:"local_evidence,omitempty"`
	NextAction    string                             `json:"next_action"`
}

type ScheduleQueryErrorResponse struct {
	Code string `json:"code"`
}

type ScheduleExecutionEvidenceResponse struct {
	Provenance        string                   `json:"provenance"`
	TaskPresence      string                   `json:"task_presence"`
	Admission         string                   `json:"admission"`
	Liveness          string                   `json:"liveness"`
	Publication       string                   `json:"publication"`
	SubmittedAt       string                   `json:"submitted_at,omitempty"`
	SupervisorReceipt bool                     `json:"supervisor_receipt"`
	ProviderStartedAt string                   `json:"provider_started_at,omitempty"`
	ProviderClosedAt  string                   `json:"provider_closed_at,omitempty"`
	InvocationState   string                   `json:"invocation_state,omitempty"`
	ProviderExitCode  *int                     `json:"provider_exit_code,omitempty"`
	Closure           ScheduleExecutionClosure `json:"closure"`
	Verdict           string                   `json:"verdict,omitempty"`
	OutcomeSHA256     string                   `json:"outcome_sha256,omitempty"`
	EvidenceSHA256    string                   `json:"evidence_sha256,omitempty"`
	Payload           *task.PayloadDescriptor  `json:"payload,omitempty"`
	Predicate         *task.PredicateRef       `json:"predicate,omitempty"`
	DiagnosticCodes   []string                 `json:"diagnostic_codes,omitempty"`
	Stops             []ScheduleExecutionStop  `json:"stops"`
	Raw               []taskdir.LogDescriptor  `json:"raw"`
}

type ScheduleExecutionClosure struct {
	Kind              task.LifetimeClosureKind `json:"kind"`
	Closed            bool                     `json:"closed"`
	TerminationCause  string                   `json:"termination_cause,omitempty"`
	TerminatedAt      string                   `json:"terminated_at,omitempty"`
	WorkerCompletedAt string                   `json:"worker_completed_at,omitempty"`
}

// Stop messages and provider errors are deliberately not projected. Their
// typed disposition and recorded timestamps are sufficient for inspection.
type ScheduleExecutionStop struct {
	RequestID     string `json:"request_id"`
	Cause         string `json:"cause,omitempty"`
	RequestedAt   string `json:"requested_at,omitempty"`
	Attempted     bool   `json:"attempted"`
	Acknowledged  *bool  `json:"acknowledged,omitempty"`
	RepliedAt     string `json:"replied_at,omitempty"`
	ObservedState string `json:"observed_state,omitempty"`
	Terminated    bool   `json:"terminated"`
	ObservedAt    string `json:"observed_at,omitempty"`
}

func scheduleExecutionEvidence(facts taskdir.TaskEvidence) (ScheduleExecutionEvidenceResponse, error) {
	closure, err := task.ResolveLifetimeClosure(facts.Lifetime)
	if err != nil {
		return ScheduleExecutionEvidenceResponse{}, err
	}
	result := ScheduleExecutionEvidenceResponse{
		Provenance: "validated_local", TaskPresence: "absent", Admission: task.AdmissionUnknown.String(),
		Liveness: task.LivenessUndetermined.String(), Publication: task.PublicationUnknown.String(),
		Closure: ScheduleExecutionClosure{Kind: closure.Kind, Closed: closure.Closed()},
		Stops:   []ScheduleExecutionStop{}, Raw: []taskdir.LogDescriptor{},
	}
	if !facts.Exists {
		return result, nil
	}
	result.TaskPresence, result.Admission = "present", task.AdmissionNotAdmitted.String()
	if facts.Submission != nil {
		result.Admission = task.AdmissionUnknown.String()
		result.SubmittedAt = facts.Submission.CreatedAt
	}
	if facts.Receipt != nil {
		result.Admission, result.SupervisorReceipt = task.AdmissionAdmitted.String(), true
	}
	if facts.Started != nil {
		result.ProviderStartedAt = facts.Started.StartedAt
	}
	if facts.Seal != nil {
		result.ProviderClosedAt, result.InvocationState = facts.Seal.ClosedAt, facts.Seal.InvocationState
		exitCode := facts.Seal.ExitCode
		result.ProviderExitCode = &exitCode
	}
	if closure.Closed() {
		result.Liveness = task.LivenessEnded.String()
	}
	if termination := facts.Lifetime.Termination; termination != nil {
		result.Closure.TerminationCause, result.Closure.TerminatedAt = termination.Cause, termination.ObservedAt
	}
	if completion := facts.Lifetime.WorkerWait; completion != nil {
		result.Closure.WorkerCompletedAt = completion.ObservedAt
	}
	if inspection := facts.Inspection; inspection != nil {
		result.Publication = inspection.Publication.String()
		if winner := inspection.Outcome; winner != nil {
			payload, predicate := winner.Payload, winner.Predicate
			result.Verdict, result.OutcomeSHA256 = winner.Verdict, facts.OutcomeSHA256
			result.EvidenceSHA256, result.Payload, result.Predicate = winner.EvidenceSHA256, &payload, &predicate
			for _, diagnostic := range winner.Diagnostics {
				result.DiagnosticCodes = append(result.DiagnosticCodes, diagnostic.Code)
			}
		}
	}
	for _, stop := range facts.Stops {
		result.Stops = append(result.Stops, scheduleExecutionStop(stop))
	}
	result.Raw = append(result.Raw, facts.Logs...)
	return result, nil
}

func scheduleExecutionStop(stop taskdir.StopRecord) ScheduleExecutionStop {
	result := ScheduleExecutionStop{}
	if request := stop.Request; request != nil {
		result.RequestID, result.Cause, result.RequestedAt = request.RequestID, request.Cause, request.RequestedAt
	}
	if reply := stop.Reply; reply != nil {
		result.Attempted, result.Acknowledged, result.RepliedAt = true, boolPointer(reply.Acknowledged), reply.RepliedAt
	}
	if observed := stop.Observation; observed != nil {
		result.ObservedState, result.Terminated, result.ObservedAt = observed.State, observed.Terminated, observed.ObservedAt
	}
	return result
}

func writeHumanScheduleExecution(output *strings.Builder, execution *ScheduleExecutionResponse) {
	if execution == nil {
		return
	}
	writeHumanScheduleOccurrence(output, execution.Occurrence)
	fmt.Fprintf(output, "inspection complete=%t next_action=%s\n", execution.Complete, execution.NextAction)
	if evidence := execution.LocalEvidence; evidence != nil {
		fmt.Fprintf(output, "local_evidence provenance=%s task_presence=%s admission=%s liveness=%s publication=%s\n", evidence.Provenance, evidence.TaskPresence, evidence.Admission, evidence.Liveness, evidence.Publication)
		fmt.Fprintf(output, "closure kind=%s closed=%t\n", evidence.Closure.Kind, evidence.Closure.Closed)
		if evidence.ProviderStartedAt != "" {
			fmt.Fprintf(output, "provider_started_at=%s\n", evidence.ProviderStartedAt)
		}
		if evidence.ProviderClosedAt != "" && evidence.ProviderExitCode != nil {
			fmt.Fprintf(output, "provider_closed_at=%s invocation_state=%s exit_code=%d\n", evidence.ProviderClosedAt, evidence.InvocationState, *evidence.ProviderExitCode)
		}
		writeHumanPayload(output, evidence.Payload)
		writeHumanRaw(output, evidence.Raw)
	}
	switch execution.NextAction {
	case "collect_task":
		fmt.Fprintf(output, "next_command=collect %s\n", execution.Occurrence.TaskID)
	case "inspect_task":
		fmt.Fprintf(output, "next_command=status %s\n", execution.Occurrence.TaskID)
	}
}
