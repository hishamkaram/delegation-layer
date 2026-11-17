package taskdir

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ReadAdmissionOnlyContext classifies cancellation guidance, never closure or
// launch permission. Storage retains the existing admission/run exclusions and
// any existing lifetime owner; this observation creates no files or locks.
func (td *TaskDir) ReadAdmissionOnlyContext(ctx context.Context) (admissionOnly bool, resultErr error) {
	if td == nil || ctx == nil {
		return false, task.ErrInvalidPermit
	}
	resultErr = td.withTaskObservationExclusionContext(ctx, false, true, func() error {
		if _, _, _, _, err := td.loadAndValidatePreparedSetContext(ctx); err != nil {
			return err
		}
		if _, err := td.ReadSubmission(); err != nil {
			if task.RecordAbsent(err) {
				return nil
			}
			return err
		}
		absent, err := td.taskRecordsAbsentContext(ctx, "supervisor.ref.json", "outcome.json", "result.txt", "publish.reject")
		if err != nil || !absent {
			return err
		}
		admissionOnly, err = td.unlaunchedTaskEvidenceContext(ctx)
		return err
	})
	if resultErr != nil {
		admissionOnly = false
	}
	return admissionOnly, resultErr
}
