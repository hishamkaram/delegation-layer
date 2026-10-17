package app

import (
	"errors"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestSubmissionClosurePreservesPrimaryFailureClassification(t *testing.T) {
	closeFault := errors.New("task close failed")
	for _, test := range []struct {
		name    string
		primary error
		status  string
	}{
		{name: "successful submission", status: "created"},
		{name: "identity failure", primary: task.ErrIdentityMismatch, status: "created"},
		{name: "primary authentication failure", primary: commonprovider.ErrAuthenticationBlocked, status: preflightStatusBlocked},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := newResponse("dispatch")
			response.Status = "created"
			response.Failure = &DispatchFailureResponse{Code: "old_recovery"}
			submitted := tasklifecycle.SubmissionResult{
				Stage: tasklifecycle.SubmissionRunnerValidation, Failure: test.primary,
				PermitReleaseError: commonprovider.ErrAuthenticationBlocked, TaskCloseError: closeFault,
			}
			result := submissionCommandResult(response, submitted)
			if result.code != 1 || result.response.Status != test.status || result.response.Failure != nil {
				t.Fatalf("closure changed primary classification: %+v", result)
			}
			if !errors.Is(result.err, commonprovider.ErrAuthenticationBlocked) || !errors.Is(result.err, closeFault) || (test.primary != nil && !errors.Is(result.err, test.primary)) {
				t.Fatalf("lost primary or closure cause: %v", result.err)
			}
			if result.response.Error != submitted.Err().Error() {
				t.Fatalf("closure order changed: got %q want %q", result.response.Error, submitted.Err())
			}
		})
	}
}
