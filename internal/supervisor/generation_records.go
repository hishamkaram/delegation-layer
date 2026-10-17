package supervisor

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Generation is a value derived from the canonical journals. Pueue's completed lifecycle journal selects Active; the
// inspection journal can only identify an exact edge that still needs its
// publication completed.
type Generation struct {
	Active           task.SupervisorRef
	Found            bool
	PrivateAuthority bool
	GroupHead        inspection.GroupRequestRecord
	GroupPresent     bool
	GroupMigrated    bool
	GroupCleanupDue  bool
}

type privateSuccessorResolver func(context.Context, string, task.SupervisorRef) (task.SupervisorRef, bool, error)

type privateSupervisorAuthority struct {
	Active      task.SupervisorRef
	Found       bool
	Initialized bool
}
