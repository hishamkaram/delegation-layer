package tasklifecycle

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// FenceScheduledTaskContext proves admission permanently closed for a created,
// never-started task. The caller must first complete Status and settle all prior
// Dispatch/control invocations, excluding any live dispatch marker for this ID.
// A current submission/receipt, live owner or execution evidence remains blocked.
// The reference is the canonical SHA-256 of the validated permanent refusal
// request (or outcome winner), never synthetic termination or a new proof file.
// A false result or any error must retain the schedule's reservation.
func FenceScheduledTaskContext(ctx context.Context, td *taskdir.TaskDir) (proven bool, referenceSHA256 string, err error) {
	return td.FenceUnlaunchedTaskContext(ctx)
}
