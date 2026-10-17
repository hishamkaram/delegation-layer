package admission

import (
	"errors"
)

func (e *StageError) Error() string { return e.err.Error() }

func (e *StageError) Unwrap() error { return e.err }

func (e *InspectionFailureContextError) Error() string { return e.err.Error() }

func (e *InspectionFailureContextError) Unwrap() error { return e.err }

func WithStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	var staged *StageError
	if errors.As(err, &staged) {
		return err
	}
	return &StageError{stage: stage, err: err}
}

func (e *StageError) Stage() string                            { return e.stage }
func (e *InspectionFailureContextError) TaskID() string        { return e.taskID }
func (e *InspectionFailureContextError) DeadlineState() string { return e.deadlineState }
