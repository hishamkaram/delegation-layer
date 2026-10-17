package tasklifecycle

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func CreateTask(selection supervisorcore.AdmissionSelection, options CreationOptions, store *taskdir.Store, req task.TaskRecord, brief []byte, briefPath string) (result CreationResult) {
	prepared, err := admissioncore.Prepare(selection, options.Admission, store, req)
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
	runnerOwnership := prepared.RunnerOwnership
	meta := NewMeta(req, profile, supervisor.Binding(), runner, runnerOwnership, options.PublisherVersion)
	if brief == nil {
		brief, err = readBriefFileForRequest(briefPath, req)
		if err != nil {
			err = annotateMissingDispatchStage("reading brief", err)
			return creationFailure(result, CreationBrief, admissioncore.WithStage("read-brief", err))
		}
	}
	if err = workspaces.RecordTaskCapture(store, req); err != nil {
		return creationFailure(result, CreationCapture, admissioncore.WithStage("record-workspace-capture", err))
	}
	td, err := store.CreateTask(req.TaskID, &req, brief, &meta)
	if err != nil {
		result.TaskRecord = CreationRecordUnknown
		err = annotateMissingDispatchStage("creating ordinary task", err)
		return creationFailure(result, CreationRecord, admissioncore.WithStage("create-task-record", err))
	}
	result.TaskRecord = CreationRecordCreated
	submitted := SubmitWithProfile(td, &req, &meta, supervisor, profile, SubmissionOptions{RequestedRunner: runner, DefaultRunner: options.Admission.Supervisor.RunnerExecutable, OpenValidationStore: options.OpenValidationStore})
	result.Submission = &submitted
	return result
}
