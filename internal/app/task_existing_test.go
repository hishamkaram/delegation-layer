package app

import (
	"errors"
	"os"
	"strings"
	"testing"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestExistingRetryMissingEnvironmentPortCannotSubmit(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	calls := 0
	deps := Dependencies{PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
		calls++
		return PreparedProfile{}, errors.New("unexpected profile preparation")
	}}
	options := tasklifecycle.ExistingTaskOptions{Creation: tasklifecycle.CreationOptions{Admission: admissionPreparationOptions(deps), OpenValidationStore: submissionValidationStore(deps)}}
	result := tasklifecycle.DispatchExisting(options, store, td, *req)
	if !errors.Is(result.Failure, task.ErrEvidenceFault) || result.Stage != tasklifecycle.ExistingEnvironment || calls != 0 {
		t.Fatalf("missing environment owner did not stop retry before profile: %+v, calls=%d", result, calls)
	}
	if _, err := td.ReadSubmission(); !task.RecordAbsent(err) {
		t.Fatalf("missing environment owner acquired submission authority: %v", err)
	}
}

func TestExistingRetryRestoresSavedEnvironmentAfterProfileRefusal(t *testing.T) {
	const key = "DELEGATE_EXISTING_ENV_PROOF"
	t.Setenv(key, "caller")
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	successor := *req
	successor.TaskID = strings.Repeat("d", 32)
	saved := *meta
	saved.TaskID = successor.TaskID
	saved.Environment, saved.EnvironmentRecorded = []string{key + "=recorded"}, true
	retry, err := store.CreateTask(successor.TaskID, &successor, []byte("brief"), &saved)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := retry.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	calls := 0
	deps := Dependencies{PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
		calls++
		if os.Getenv(key) != "recorded" {
			t.Errorf("profile saw caller environment instead of saved launch state")
		}
		return PreparedProfile{}, task.ErrIdentityMismatch
	}}
	result := dispatch(Arguments{Root: store.Root, TaskID: successor.TaskID, Provider: successor.Provider, Cwd: successor.CanonicalCwd, Config: successor.RequestedConfig, BriefBytes: []byte("brief")}, deps)
	if result.code != 2 || !errors.Is(result.err, task.ErrIdentityMismatch) || calls != 1 {
		t.Fatalf("retry did not refuse at the real profile boundary: %+v, calls=%d", result, calls)
	}
	if os.Getenv(key) != "caller" {
		t.Fatal("retry did not restore caller environment after refusal")
	}
	if _, err = retry.ReadSubmission(); !task.RecordAbsent(err) {
		t.Fatalf("profile refusal created submission authority: %v", err)
	}
}

func TestExistingProjectionPreservesRestoreThenTaskClose(t *testing.T) {
	primary := errors.New("profile refused")
	restoreFault := commonprovider.ErrAuthenticationBlocked
	for _, closeFault := range []error{nil, errors.New("task close failed")} {
		response := newResponse("dispatch")
		response.Status = "created"
		result := existingCommandResult(response, "fixture", tasklifecycle.ExistingTaskResult{Stage: tasklifecycle.ExistingProfile, Failure: primary, EnvironmentRestoreError: restoreFault, TaskCloseError: closeFault})
		wantCode := 2
		wantErr := errors.Join(primary, restoreFault)
		if closeFault != nil {
			wantCode = 1
			wantErr = errors.Join(wantErr, closeFault)
		}
		if result.code != wantCode || result.response.Status != "created" || result.response.Error != wantErr.Error() {
			t.Fatalf("existing closure changed primary classification or order: %+v", result)
		}
		var stage *admissioncore.StageError
		if !errors.As(result.err, &stage) || stage.Stage() != "existing-task" || !errors.Is(result.err, primary) || !errors.Is(result.err, restoreFault) {
			t.Fatalf("lost existing-task stage or causes: %v", result.err)
		}
	}
}
