package app

import (
	"errors"
	"testing"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestCreationProjectionPreservesPrimaryFailureAndClosureOrder(t *testing.T) {
	closeFault := errors.New("task close failed")
	submitted := tasklifecycle.SubmissionResult{
		Observation:        tasklifecycle.Observation{Admission: task.AdmissionAdmitted, AdmissionObserved: true},
		PermitReleaseError: commonprovider.ErrAuthenticationBlocked, TaskCloseError: closeFault,
	}
	response := newResponse("dispatch")
	response.Status, response.TaskRecord = "created", "created"
	response.Failure = &DispatchFailureResponse{Code: "old_recovery"}
	created := tasklifecycle.CreationResult{Submission: &submitted}
	result := creationCommandResult(response, "fixture", created)
	if result.code != 1 || result.response.Status != "created" || result.response.TaskRecord != "created" || result.response.Failure != nil {
		t.Fatalf("creation reclassified late closure or unobserved task presence: %+v", result)
	}
	if !errors.Is(result.err, commonprovider.ErrAuthenticationBlocked) || !errors.Is(result.err, closeFault) {
		t.Fatalf("creation lost closure causes: %v", result.err)
	}
	var stage *admissioncore.StageError
	if !errors.As(result.err, &stage) || stage.Stage() != "record-submission-evidence" || result.response.Admission != task.AdmissionAdmitted.String() {
		t.Fatalf("creation lost admitted submission stage: %+v, %v", result.response, result.err)
	}
	if result.response.Error != submitted.Err().Error() {
		t.Fatalf("creation changed closure error bytes: got %q want %q", result.response.Error, submitted.Err())
	}
}
