package app

import (
	"context"
	"encoding/json"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func supervisorOptionsForMeta(base pueue.Options, meta task.MetaRecord) pueue.Options {
	return supervisorcore.OptionsWithEnvironment(base, meta.Environment)
}

func prepareAdmissionExecutables(a Arguments, deps Dependencies, root string) (string, error) {
	prepared, err := supervisorcore.PrepareAdmissionExecutables(supervisorAdmissionSelection(a), supervisorAdmissionOptions(deps, deps.SupervisorOptions), root)
	return prepared.Executable, admissioncore.WithStage(prepared.FailureStage, err)
}

func requestedRunnerOwnership(a Arguments) string {
	return supervisorcore.RequestedRunnerOwnership(supervisorAdmissionSelection(a))
}

func admissionQueueRepairRunner(a Arguments, deps Dependencies, taskRunner string) (string, error) {
	return supervisorcore.AdmissionQueueRepairRunner(supervisorAdmissionSelection(a), supervisorAdmissionOptions(deps, deps.SupervisorOptions), taskRunner)
}

func admissionUsesPrivateSupervisor(a Arguments, root string) (bool, error) {
	return supervisorcore.AdmissionUsesPrivateSupervisor(supervisorAdmissionSelection(a), root)
}

func preparePrivateAdmissionSupervisor(deps Dependencies, store *taskdir.Store, options pueue.Options) (*pueue.Client, error) {
	return supervisorcore.PreparePrivateAdmissionSupervisor(deps.InitialSupervisorExecutable, store, options)
}

func inspectLegacyEndedJob(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord, job pueue.Job) (resultErr error) {
	return privateCleanupOwner().InspectEndedJob(ctx, store, legacy, job)
}

func admissionInspectionFacts(a Arguments, deps Dependencies, store *taskdir.Store, req task.TaskRecord, candidate commonprovider.ProfileCandidate, supervisor *pueue.Client, inspectionGroupReady bool) (facts json.RawMessage, deadline time.Time, resultErr error) {
	return admissioncore.InspectProvider(admissioncore.InspectionOptions{Runner: a.Runner, DefaultRunner: deps.RunnerExecutable, RunnerOwnership: requestedRunnerOwnership(a), PreserveGeneration: a.savedSupervisor != nil}, store, req, candidate, supervisor, inspectionGroupReady)
}
