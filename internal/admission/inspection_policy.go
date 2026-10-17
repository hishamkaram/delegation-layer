package admission

import (
	"errors"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ConcurrentInspection(root string, binding inspection.Binding) bool {
	return binding.DefinitionRevision == commonprovider.RuntimeInspectionRevision && binding.CapabilitySHA256 != "" && pueue.IsPrivateConfig(root, binding.Supervisor.ConfigPath)
}

func markerlessRuntimeReplayBindingMatches(saved, current inspection.Binding) bool {
	return saved.DefinitionRevision == commonprovider.RuntimeInspectionRevision &&
		current.DefinitionRevision == commonprovider.RuntimeInspectionRevision &&
		saved.CapabilitySHA256 == "" && current.CapabilitySHA256 != "" &&
		(saved.RunnerOwnership == "" || saved.RunnerOwnership == current.RunnerOwnership) &&
		saved.WorkerExitProof == current.WorkerExitProof &&
		saved.WorkerExecutable == current.WorkerExecutable &&
		saved.WorkerSHA256 == current.WorkerSHA256 &&
		task.SameSupervisorIdentity(saved.Supervisor, current.Supervisor)
}

func sameInspectionBinding(left, right inspection.Binding) bool {
	return sameInspectionBindingBase(left, right) &&
		(left.RunnerOwnership == right.RunnerOwnership || left.RunnerOwnership == "") &&
		left.WorkerExitProof == right.WorkerExitProof &&
		left.WorkerExecutable == right.WorkerExecutable &&
		left.WorkerSHA256 == right.WorkerSHA256
}

func sameInspectionBindingBase(left, right inspection.Binding) bool {
	return sameInspectionEnvironment(left, right) &&
		task.SameSupervisorIdentity(left.Supervisor, right.Supervisor) &&
		sameInspectionDefinition(left, right)
}

func sameInspectionEnvironment(left, right inspection.Binding) bool {
	leftEnvironmentRecorded := left.EnvironmentRecorded || len(left.Environment) > 0
	rightEnvironmentRecorded := right.EnvironmentRecorded || len(right.Environment) > 0
	return leftEnvironmentRecorded == rightEnvironmentRecorded && slices.Equal(left.Environment, right.Environment)
}

func sameInspectionDefinition(left, right inspection.Binding) bool {
	if left.DefinitionRevision == commonprovider.RuntimeInspectionRevision && right.DefinitionRevision == commonprovider.RuntimeInspectionRevision {
		return sameRuntimeInspectionDefinition(left, right)
	}
	return sameExactInspectionDefinition(left, right)
}

func sameRuntimeInspectionDefinition(left, right inspection.Binding) bool {
	// Only bindings carrying the runtime-only capability contract may relax
	// executable identity. A mixed native/runtime binding without that marker
	// remains exact so native command and helper changes cannot be silently
	// reused.
	if left.CapabilitySHA256 != "" && right.CapabilitySHA256 != "" {
		return left.CapabilitySHA256 == right.CapabilitySHA256
	}
	return sameExactInspectionDefinition(left, right)
}

func sameExactInspectionDefinition(left, right inspection.Binding) bool {
	return left.DefinitionRevision == right.DefinitionRevision &&
		left.DefinitionSHA256 == right.DefinitionSHA256 &&
		left.HelperExecutable == right.HelperExecutable &&
		left.HelperSHA256 == right.HelperSHA256
}

func sameInspectionBindingExceptManagedWorker(saved, current inspection.Binding) bool {
	return saved.RunnerOwnership == task.RunnerOwnershipManaged &&
		current.RunnerOwnership == task.RunnerOwnershipManaged &&
		saved.WorkerExitProof == current.WorkerExitProof &&
		sameInspectionBindingBase(saved, current)
}

func guardedInspectionLaunch(runner, root string) pueue.Launch {
	return pueue.Launch{
		RunnerExecutable: runner,
		RootPath:         root,
		// Every inspection worker needs the guardian's independent empty-scope
		// proof. The immutable runner ownership still controls whether the child
		// may consume the saved add receipt; custom runners reconcile the exact
		// queue row instead.
		Supervised: true,
	}
}

func shouldStopInspectionPolling(err error) bool {
	return errors.Is(err, inspection.ErrAdmissionExpired) || supervisorcore.PendingInspectionCommand(err) != nil
}

func validateTimelyCompletedInspectionEvidence(deadline time.Time, result inspection.ResultRecord, completion inspection.CompletionRecord, termination *task.LifetimeTerminationRecord) error {
	return validateCompletedInspectionEvidence(deadline, result, completion, termination, true)
}

func validateCompletedInspectionEvidence(deadline time.Time, result inspection.ResultRecord, completion inspection.CompletionRecord, termination *task.LifetimeTerminationRecord, requireTermination bool) error {
	if result.Reason != inspection.ResultEligible || (requireTermination && termination == nil) {
		return inspection.ErrAdmissionExpired
	}
	completedAt, err := time.Parse(time.RFC3339Nano, completion.CompletedAt)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if !completedAt.Before(deadline) {
		return inspection.ErrAdmissionExpired
	}
	if !requireTermination {
		return nil
	}
	terminatedAt, err := time.Parse(time.RFC3339Nano, termination.ObservedAt)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if termination.Cause != task.LifetimeTerminationCompleted || !termination.ScopeEmpty || !terminatedAt.Before(deadline) {
		return inspection.ErrAdmissionExpired
	}
	return nil
}

const (
	inspectionStatusWatchdog        = supervisorcore.InspectionStatusWatchdog
	inspectionTerminalEventWatchdog = 5 * time.Second
)

func (schedule *inspectionPollSchedule) due(now time.Time, completed bool) bool {
	prompt := completed && !schedule.completionSeen && !schedule.callbackExpected
	schedule.completionSeen = schedule.completionSeen || completed
	if !schedule.nextWatchdog.IsZero() && now.Before(schedule.nextWatchdog) && !prompt {
		return false
	}
	schedule.nextWatchdog = now.Add(schedule.watchdog())
	return true
}

func (schedule *inspectionPollSchedule) observed(now time.Time, completed bool) {
	schedule.completionSeen = schedule.completionSeen || completed
	schedule.nextWatchdog = now.Add(schedule.watchdog())
}

func (schedule *inspectionPollSchedule) watchdog() time.Duration {
	if schedule.callbackExpected {
		return inspectionTerminalEventWatchdog
	}
	return inspectionStatusWatchdog
}

// classifyInspectionStatusObservation is the single policy boundary shared by
// watchdog polling and terminal-event reconciliation. A valid no-match and a
// completed but unusable status command remain retryable only within the
// caller's immutable observation deadline. The latter retains its cause so a
// persistent control failure is reported at that deadline. Pending commands,
// identity faults, evidence faults, and other policy failures stop immediately.
func classifyInspectionStatusObservation(observation pueue.InspectionObservation, err error) (retry bool, resultErr error) {
	if err == nil {
		return false, nil
	}
	if shouldStopInspectionPolling(err) {
		return false, err
	}
	if errors.Is(err, task.ErrIdentityMismatch) || errors.Is(err, task.ErrEvidenceFault) ||
		errors.Is(err, pueue.ErrBinding) || errors.Is(err, pueue.ErrConfiguration) {
		return false, err
	}
	if errors.Is(err, pueue.ErrStatusTransient) {
		return true, err
	}
	if observation.Pending == nil && task.AllErrorCauses(err, func(candidate error) bool {
		return errors.Is(candidate, pueue.ErrUnknown)
	}) {
		return true, nil
	}
	return false, err
}
