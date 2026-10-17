package tasklifecycle

import (
	"errors"
	"fmt"
	"os"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func SubmissionFailure(err error, admitted bool) error {
	if err != nil {
		stage := "prepare-task-submission"
		switch {
		case errors.Is(err, pueue.ErrSubmissionUncertain):
			stage = "submit-task"
		case errors.Is(err, pueue.ErrEnqueueUncertain):
			stage = "enqueue-task"
		case admitted:
			stage = "record-submission-evidence"
		}
		err = admissioncore.WithStage(stage, err)
	}
	return err
}

// annotateMissingDispatchStage adds only a fixed operation label to an
// otherwise opaque missing-file error. This keeps errors.Is(os.ErrNotExist)
// behavior intact while making the failing admission boundary diagnosable;
// no path, record bytes, or native diagnostic is included.
func annotateMissingDispatchStage(stage string, err error) error {
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fmt.Errorf("dispatch %s: %w", stage, err)
}
