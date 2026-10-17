package workspaces

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// OperationRuntime supplies application-composed runner and supervisor edges.
// The coordinator owns deadlines, durable purposes, permits, and every acquired
// client, including a client returned with an error while a probe is pending.
type OperationRuntime interface {
	ManagedRunner() (string, error)
	PrepareManaged(context.Context, *taskdir.Store, string) (string, *pueue.Client, error)
	OpenSaved(context.Context, string, task.SupervisorRef) (*pueue.Client, error)
}

// InspectionRecovery applies admission's recovery policy to the inspection
// selected and validated by the workspace coordinator. Admission owns recovery
// leases; the coordinator owns whether the reservation may close. A nil port
// is an evidence fault when an existing inspection requires recovery.
type InspectionRecovery func(context.Context, *inspection.Operation) error

// OperationOptions carries immutable invocation configuration. The canonical
// supervisor timeout policy interprets ObservationTimeout, including its default.
type OperationOptions struct {
	Runtime            OperationRuntime
	ObservationTimeout time.Duration
	RecoverInspection  InspectionRecovery
}
