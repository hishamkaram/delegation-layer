package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ScheduleHistoryResponse is a passive page of scheduler-owned facts. The
// exclusive cursor is an endpoint, not a task ID or a frozen snapshot token.
type ScheduleHistoryResponse struct {
	ScheduleID string                 `json:"schedule_id"`
	Entries    []ScheduleHistoryEntry `json:"entries"`
	NextBefore string                 `json:"next_before,omitempty"`
}

// ScheduleHistoryEntry preserves the store's occurrence-or-skip distinction.
type ScheduleHistoryEntry struct {
	Kind       string                      `json:"kind"`
	Endpoint   time.Time                   `json:"endpoint"`
	Occurrence *ScheduleOccurrenceResponse `json:"occurrence,omitempty"`
	Skip       *ScheduleSkipResponse       `json:"skip,omitempty"`
}

type ScheduleSkipResponse struct {
	From   time.Time           `json:"from"`
	To     time.Time           `json:"to"`
	Reason contract.SkipReason `json:"reason"`
}

// ScheduleOccurrenceResponse keeps reservation release separate from the
// last cached task observation; neither is a fresh liveness observation.
type ScheduleOccurrenceResponse struct {
	ResultCode       string                      `json:"result_code,omitempty"`
	ResultStatus     string                      `json:"result_status,omitempty"`
	ResultObservedAt *time.Time                  `json:"result_observed_at,omitempty"`
	ScheduleID       string                      `json:"schedule_id"`
	TaskID           string                      `json:"task_id"`
	Due              time.Time                   `json:"due"`
	ArmedAt          time.Time                   `json:"armed_at"`
	Reservation      string                      `json:"reservation"`
	Release          *ScheduleReleaseResponse    `json:"release,omitempty"`
	Observation      ScheduleObservationResponse `json:"observation"`
	Failure          *contract.AttemptFailure    `json:"failure,omitempty"`
}

type ScheduleReleaseResponse struct {
	ReleasedAt time.Time            `json:"released_at"`
	Kind       contract.ReleaseKind `json:"kind"`
	Ref        string               `json:"ref"`
}

type ScheduleObservationResponse struct {
	Provenance    string     `json:"provenance"`
	ObservedAt    *time.Time `json:"observed_at,omitempty"`
	Admission     string     `json:"admission"`
	Liveness      string     `json:"liveness"`
	Publication   string     `json:"publication"`
	OutcomeSHA256 string     `json:"outcome_sha256,omitempty"`
	FaultCode     string     `json:"fault_code,omitempty"`
}

func scheduleOccurrence(item contract.Occurrence) ScheduleOccurrenceResponse {
	observation := ScheduleObservationResponse{
		Provenance: "unobserved", Admission: task.AdmissionUnknown.String(),
		Liveness: task.LivenessUndetermined.String(), Publication: task.PublicationUnknown.String(),
	}
	if cached := item.Observation; cached != nil {
		observation = ScheduleObservationResponse{
			Provenance: "cached", ObservedAt: &cached.ObservedAt,
			Admission: cached.Admission.String(), Liveness: cached.Liveness.String(),
			Publication: cached.Publication.String(), OutcomeSHA256: cached.OutcomeSHA256, FaultCode: cached.FaultCode,
		}
	}
	result := ScheduleOccurrenceResponse{
		ScheduleID: item.ScheduleID, TaskID: item.TaskID, Due: item.Due,
		ArmedAt: item.ArmedAt, Reservation: "active", Observation: observation, Failure: item.Attempt,
	}
	if cached := item.Result; cached != nil {
		result.ResultCode = cached.Code
		result.ResultStatus = cached.Verdict
		result.ResultObservedAt = &cached.ObservedAt
	}
	if release := item.Release; release != nil {
		result.Reservation = "released"
		result.Release = &ScheduleReleaseResponse{ReleasedAt: release.ReleasedAt, Kind: release.Kind, Ref: release.Ref}
	}
	return result
}

func scheduleHistoryEntry(scheduleID string, entry contract.HistoryEntry) (ScheduleHistoryEntry, error) {
	if err := contract.ValidateHistoryEntry(scheduleID, entry); err != nil {
		return ScheduleHistoryEntry{}, err
	}
	endpoint, err := entry.Endpoint()
	if err != nil {
		return ScheduleHistoryEntry{}, err
	}
	result := ScheduleHistoryEntry{Endpoint: endpoint}
	if item, ok := entry.Occurrence(); ok {
		occurrence := scheduleOccurrence(item)
		result.Kind, result.Occurrence = "occurrence", &occurrence
	} else if item, ok := entry.Skip(); ok {
		result.Kind = "skip"
		result.Skip = &ScheduleSkipResponse{From: item.From, To: item.To, Reason: item.Reason}
	}
	return result, nil
}

func writeHumanScheduleHistory(output *strings.Builder, history *ScheduleHistoryResponse) {
	if history == nil {
		return
	}
	fmt.Fprintf(output, "schedule_history schedule_id=%s entries=%d\n", history.ScheduleID, len(history.Entries))
	for _, entry := range history.Entries {
		if item := entry.Occurrence; item != nil {
			writeHumanScheduleOccurrence(output, *item)
			fmt.Fprintf(output, "next_command=schedule execution %s\n", item.TaskID)
		} else if item := entry.Skip; item != nil {
			fmt.Fprintf(output, "skip from_exclusive=%s to_inclusive=%s reason=%s\n", item.From.Format(time.RFC3339Nano), item.To.Format(time.RFC3339Nano), item.Reason)
		}
	}
	if history.NextBefore != "" {
		fmt.Fprintf(output, "next_before=%s\n", history.NextBefore)
	}
}

func writeHumanScheduleOccurrence(output *strings.Builder, item ScheduleOccurrenceResponse) {
	fmt.Fprintf(output, "occurrence task_id=%s schedule_id=%s due=%s armed_at=%s reservation=%s\n", item.TaskID, item.ScheduleID, item.Due.Format(time.RFC3339Nano), item.ArmedAt.Format(time.RFC3339Nano), item.Reservation)
	if item.ResultObservedAt != nil {
		fmt.Fprintf(output, "result status=%s code=%s observed_at=%s\n", item.ResultStatus, item.ResultCode, item.ResultObservedAt.Format(time.RFC3339Nano))
	}
	observation := item.Observation
	fmt.Fprintf(output, "observation provenance=%s admission=%s liveness=%s publication=%s", observation.Provenance, observation.Admission, observation.Liveness, observation.Publication)
	if observation.ObservedAt != nil {
		fmt.Fprintf(output, " observed_at=%s", observation.ObservedAt.Format(time.RFC3339Nano))
	}
	if observation.OutcomeSHA256 != "" {
		fmt.Fprintf(output, " outcome_sha256=%s", observation.OutcomeSHA256)
	}
	if observation.FaultCode != "" {
		fmt.Fprintf(output, " fault_code=%s", observation.FaultCode)
	}
	output.WriteByte('\n')
	if f := item.Failure; f != nil {
		fmt.Fprintf(output, "failure code=%s stage=%s next_action=%s message=%s\n", f.Code, f.Stage, f.NextAction, f.Message)
	}
	if release := item.Release; release != nil {
		fmt.Fprintf(output, "release released_at=%s kind=%s ref=%s\n", release.ReleasedAt.Format(time.RFC3339Nano), release.Kind, release.Ref)
	}
}
