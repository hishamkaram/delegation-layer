package tasklifecycle

import (
	"context"
	"errors"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// CreateTask creates an ordinary task without a resource check. Existing and
// partial-task recovery uses it; only a fresh dispatch carries a resource gate.
func CreateTask(selection supervisorcore.AdmissionSelection, options CreationOptions, store *taskdir.Store, req task.TaskRecord, brief []byte, briefPath string) CreationResult {
	return CreateTaskContext(context.Background(), selection, options, store, req, brief, briefPath)
}

func CreateTaskContext(ctx context.Context, selection supervisorcore.AdmissionSelection, options CreationOptions, store *taskdir.Store, req task.TaskRecord, brief []byte, briefPath string) CreationResult {
	return createTaskContext(ctx, selection, options, store, req, brief, briefPath, nil)
}

func createTaskContext(ctx context.Context, selection supervisorcore.AdmissionSelection, options CreationOptions, store *taskdir.Store, req task.TaskRecord, brief []byte, briefPath string, gate *resourceGate) (result CreationResult) {
	ctx, lease, err := ordinaryRootAdmission(ctx, store, options.RootAdmission, options.Admission.Supervisor.Control)
	if lease != nil && options.RootAdmission == nil {
		defer func() {
			result.RootAdmissionCleanup, result.RootAdmissionReleaseError = releaseRootAdmission(lease, result.Err())
		}()
	}
	if err != nil {
		return creationFailure(result, CreationAdmission, err)
	}
	prepared, err := admissioncore.PrepareContext(ctx, selection, options.Admission, store, req)
	if err != nil {
		err = admissioncore.WithStage("prepare-admission", annotateMissingDispatchStage("preparing inspection admission", err))
		return creationFailure(result, CreationAdmission, err)
	}
	result.Capability = &prepared.Capability
	profile, supervisor := prepared.Profile, prepared.Supervisor
	defer func() { supervisorcore.CloseMaintenance(supervisor, result.Err()) }()
	runner := prepared.RunnerExecutable
	if err = profile.ValidateAdmission(req); err != nil {
		return creationFailure(result, CreationProfile, admissioncore.WithStage("validate-profile", err))
	}
	if err = gate.refreshContext(ctx, supervisor.Binding(), options.Admission.Supervisor); err != nil {
		return creationFailure(result, CreationResources, admissioncore.WithStage("check-resources", err))
	}
	runnerOwnership := prepared.RunnerOwnership
	meta := NewMeta(req, profile, supervisor.Binding(), runner, runnerOwnership, options.PublisherVersion)
	if brief == nil {
		brief, err = readBriefFileForRequest(briefPath, req)
		if err != nil {
			err = annotateMissingDispatchStage("reading brief", err)
			return creationFailure(result, CreationBrief, admissioncore.WithStage("read-brief", err))
		}
	}
	td, err := store.CreateTaskContext(ctx, req.TaskID, &req, brief, &meta)
	if err != nil {
		result.TaskRecord = CreationRecordUnknown
		err = annotateMissingDispatchStage("creating ordinary task", err)
		return creationFailure(result, CreationRecord, admissioncore.WithStage("create-task-record", err))
	}
	result.TaskRecord = CreationRecordCreated
	persistedSupervisor := meta.SupervisorConfig
	result.Supervisor = &persistedSupervisor
	// The canonical task decides the winner before any capture obligation is
	// bound to its hash. Existing recovery repairs a missing obligation from
	// that same saved request; submission always follows a successful capture.
	if err = workspaces.RecordTaskCaptureContext(ctx, store, req); err != nil {
		return creationFailure(result, CreationCapture, admissioncore.WithStage("record-workspace-capture", errors.Join(err, td.Close())))
	}
	submitted := SubmitWithProfileContext(ctx, td, &req, &meta, supervisor, profile, SubmissionOptions{Supervisor: SupervisorOptions{AdmissionStore: store, RootAdmission: lease}, RequestedRunner: runner, DefaultRunner: options.Admission.Supervisor.RunnerExecutable, OpenValidationStore: options.OpenValidationStore})
	result.Submission = &submitted
	return result
}
