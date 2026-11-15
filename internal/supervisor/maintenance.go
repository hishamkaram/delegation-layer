package supervisor

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// MaintenanceAssessment is derived evidence, never transition authority. The
// caller owns its queue observation and must repeat inspection under the
// existing mutation leases before replacing a supervisor.
type MaintenanceAssessment struct {
	Readiness    string
	ActiveWork   string
	FirstBlocker *MaintenanceBlocker
}

// MaintenanceBlocker contains only validated control metadata, never error
// text, executable paths, provider output or a reusable numeric queue address.
type MaintenanceBlocker struct {
	Kind        string      `json:"kind"`
	TaskID      string      `json:"task_id,omitempty"`
	OperationID string      `json:"operation_id,omitempty"`
	QueueState  pueue.State `json:"queue_state,omitempty"`
	ReasonCode  string      `json:"reason_code"`
	NextAction  string      `json:"next_action"`
}

// AssessMaintenance composes queue evidence with every canonical root resource.
// Queue activity remains a separate observation; a rowless live or ambiguous
// resource can block replacement without inventing an active queue row.
func (c *Cleanup) AssessMaintenance(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, jobs []pueue.Job) (MaintenanceAssessment, error) {
	var assessment MaintenanceAssessment
	err := withMaintenanceLease(ctx, store, func(locked context.Context) error {
		var queueErr error
		assessment, queueErr = c.InspectMigrationQueue(locked, store, InspectionGroup(store.RootID, binding), jobs)
		rootErr := checkMaintenanceQuiescence(locked, store, binding, jobs)
		var combined error
		assessment, combined = combineMaintenance(assessment, queueErr, rootErr)
		return combined
	})
	if err != nil && (assessment.Readiness == "" || !MaintenanceBusy(err)) {
		assessment.Readiness = "unknown"
		assessment.ActiveWork = QueueActivity(jobs)
		assessment.FirstBlocker = runtimeMaintenanceBlocker(err, false)
	}
	return assessment, err
}

func combineMaintenance(assessment MaintenanceAssessment, queueErr, rootErr error) (MaintenanceAssessment, error) {
	if rootErr == nil {
		return assessment, queueErr
	}
	if queueErr != nil && !MaintenanceBusy(queueErr) {
		return assessment, errors.Join(queueErr, rootErr)
	}
	if !MaintenanceBusy(rootErr) {
		assessment.Readiness = "unknown"
		assessment.FirstBlocker = runtimeMaintenanceBlocker(rootErr, false)
	} else if assessment.Readiness == "clear" {
		assessment.Readiness = "blocked"
		assessment.FirstBlocker = runtimeMaintenanceBlocker(rootErr, true)
	}
	return assessment, &MaintenanceError{Assessment: assessment, Cause: errors.Join(queueErr, rootErr)}
}

func runtimeMaintenanceBlocker(cause error, busy bool) *MaintenanceBlocker {
	blocker := &MaintenanceBlocker{Kind: "observation", ReasonCode: "runtime_evidence_unreadable", NextAction: "stop_and_report"}
	var resource *RuntimeMaintenanceError
	if errors.As(cause, &resource) {
		blocker.Kind = resource.Kind
		if resource.Kind == "task" {
			blocker.TaskID = resource.ID
		} else {
			blocker.OperationID = resource.ID
		}
	}
	if busy {
		blocker.ReasonCode = "runtime_closure_unproven"
		blocker.NextAction = "wait_for_legacy_work"
		if blocker.Kind == "task" {
			blocker.NextAction = "human_review_task"
		}
	}
	return blocker
}

// MaintenanceError retains the owner's actual causes alongside a bounded
// immutable projection. Joined faults must still be classified independently.
type MaintenanceError struct {
	Assessment MaintenanceAssessment
	Cause      error
}

func (e *MaintenanceError) Error() string {
	return "private supervisor maintenance is " + e.Assessment.Readiness
}
func (e *MaintenanceError) Unwrap() error { return e.Cause }

type maintenanceFact struct {
	job    pueue.Job
	taskID string
	err    error
}

// InspectMigrationQueue uses the same passive evidence readers for status and
// migration eligibility. It does not recover, record exits or remove rows.
func (c *Cleanup) InspectMigrationQueue(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, jobs []pueue.Job) (MaintenanceAssessment, error) {
	if ctx == nil || store == nil {
		return unknownMaintenance(task.ErrInvalidPermit, "unknown")
	}
	if err := ctx.Err(); err != nil {
		return unknownMaintenance(err, QueueActivity(jobs))
	}
	facts := make([]maintenanceFact, 0, len(jobs))
	labels, seen := PrivateQueueLabelCounts(jobs), make(map[int64]bool, len(jobs))
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return interruptedMaintenance(err, facts, jobs)
		}
		fact := maintenanceQueueFact(store.RootID, job, labels, seen)
		seen[job.ID] = true
		if fact.err == nil && fact.taskID != "" && job.State == pueue.StateEnded {
			fact.err = c.InspectEndedJob(ctx, store, group, job)
		}
		facts = append(facts, fact)
	}
	if err := ctx.Err(); err != nil {
		return interruptedMaintenance(err, facts, jobs)
	}
	assessment, cause := classifyMaintenance(facts)
	if cause != nil {
		return assessment, &MaintenanceError{Assessment: assessment, Cause: cause}
	}
	return assessment, nil
}

func maintenanceQueueFact(rootID string, job pueue.Job, labels map[string]int, seen map[int64]bool) maintenanceFact {
	fact := maintenanceFact{job: job}
	if job.ID < 0 || seen[job.ID] {
		fact.err = task.ErrEvidenceFault
	}
	if job.Label == nil {
		return fact
	}
	id, managed, _, err := LegacyJobTaskID(*job.Label, rootID)
	fact.err = errors.Join(fact.err, err)
	if managed {
		fact.taskID = id
		if labels[*job.Label] != 1 {
			fact.err = errors.Join(fact.err, task.ErrEvidenceFault)
		}
	}
	return fact
}

func interruptedMaintenance(err error, facts []maintenanceFact, jobs []pueue.Job) (MaintenanceAssessment, error) {
	_, previous := classifyMaintenance(facts)
	return unknownMaintenance(errors.Join(previous, err), QueueActivity(jobs))
}

func unknownMaintenance(err error, activity string) (MaintenanceAssessment, error) {
	assessment := MaintenanceAssessment{Readiness: "unknown", ActiveWork: activity, FirstBlocker: &MaintenanceBlocker{Kind: "observation", ReasonCode: "observation_failed", NextAction: "stop_and_report"}}
	return assessment, &MaintenanceError{Assessment: assessment, Cause: err}
}

// QueueActivity reports positive activity separately from an unknown state.
func QueueActivity(jobs []pueue.Job) string {
	activity := "none"
	for _, job := range jobs {
		activity = queueActivityAfter(activity, job.State)
	}
	return activity
}

func queueActivityAfter(activity string, state pueue.State) string {
	switch state {
	case pueue.StateQueued, pueue.StateStashed, pueue.StateRunning:
		return "present"
	case pueue.StateEnded:
		return activity
	case pueue.StateUnknown:
	default:
	}
	if activity == "present" {
		return activity
	}
	return "unknown"
}

func classifyMaintenance(facts []maintenanceFact) (MaintenanceAssessment, error) {
	assessment := MaintenanceAssessment{Readiness: "clear", ActiveWork: "none"}
	var cause error
	for _, fact := range facts {
		assessment.ActiveWork = queueActivityAfter(assessment.ActiveWork, fact.job.State)
	}
	for _, fact := range facts {
		blocker, err := classifyMaintenanceFact(fact)
		if err == nil {
			continue
		}
		if !MaintenanceBusy(err) {
			if assessment.Readiness != "unknown" {
				assessment.FirstBlocker = &blocker
			}
			assessment.Readiness = "unknown"
		} else if assessment.Readiness == "clear" {
			assessment.Readiness = "blocked"
			assessment.FirstBlocker = &blocker
		}
		cause = errors.Join(cause, err)
	}
	return assessment, cause
}

func classifyMaintenanceFact(fact maintenanceFact) (MaintenanceBlocker, error) {
	blocker := MaintenanceBlocker{Kind: "queue", TaskID: fact.taskID, QueueState: fact.job.State}
	if fact.err != nil {
		var detail *MaintenanceError
		if errors.As(fact.err, &detail) && detail.Assessment.FirstBlocker != nil {
			blocker = *detail.Assessment.FirstBlocker
			blocker.QueueState = fact.job.State
		}
		if blocker.ReasonCode == "" || !MaintenanceBusy(fact.err) {
			blocker.Kind = "evidence"
			blocker.ReasonCode = "evidence_unreadable"
			blocker.NextAction = "stop_and_report"
		}
		return blocker, fact.err
	}
	switch fact.job.State {
	case pueue.StateEnded:
		return MaintenanceBlocker{}, nil
	case pueue.StateQueued, pueue.StateStashed, pueue.StateRunning:
		blocker.ReasonCode = "queue_work_present"
		blocker.NextAction = "wait_for_legacy_work"
		return blocker, pueue.ErrPrivateSupervisorMigrationBusy
	case pueue.StateUnknown:
		blocker.ReasonCode = "queue_state_unknown"
		blocker.NextAction = "stop_and_report"
		return blocker, pueue.ErrUnknown
	default:
		blocker.ReasonCode = "queue_state_invalid"
		blocker.NextAction = "stop_and_report"
		return blocker, task.ErrEvidenceFault
	}
}

// MaintenanceBusy classifies the entire error, including teardown. A busy
// marker cannot hide a joined storage, identity, unlock or observation fault.
func MaintenanceBusy(err error) bool {
	var diagnostic *MaintenanceError
	if errors.As(err, &diagnostic) && diagnostic.Assessment.Readiness == "unknown" {
		return false
	}
	marker := errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy) || errors.Is(err, inspection.ErrGroupMigrationBusy) || errors.Is(err, ErrRuntimeQuiescenceBusy) || errors.Is(err, guardian.ErrProofPending)
	return marker && !taskdir.CleanupFault(err) && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, pueue.ErrPrivateSupervisorMigrationBusy) || errors.Is(cause, inspection.ErrGroupMigrationBusy) || errors.Is(cause, ErrRuntimeQuiescenceBusy) || errors.Is(cause, guardian.ErrProofPending) || errors.Is(cause, pueue.ErrPrivateSupervisorMigrationRequired) || errors.Is(cause, inspection.ErrGroupMigrationRequired)
	})
}

func taskMaintenancePending(id, reason string, cause error) error {
	assessment := MaintenanceAssessment{Readiness: "blocked", ActiveWork: "none", FirstBlocker: &MaintenanceBlocker{Kind: "task", TaskID: id, ReasonCode: reason, NextAction: "human_review_task"}}
	return &MaintenanceError{Assessment: assessment, Cause: cause}
}
