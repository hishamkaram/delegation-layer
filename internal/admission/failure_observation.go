package admission

import (
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
)

func WithInspectionFailureContext(err error, taskID string, deadline time.Time) error {
	if err == nil {
		return nil
	}
	state := "unknown"
	if !deadline.IsZero() {
		state = "open"
		if !time.Now().Before(deadline) {
			state = "expired"
		}
	}
	if errors.Is(err, inspection.ErrAdmissionExpired) {
		state = "expired"
	}
	return &InspectionFailureContextError{err: err, taskID: taskID, deadlineState: state}
}
