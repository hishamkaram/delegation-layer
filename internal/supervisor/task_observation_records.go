package supervisor

import "github.com/hishamkaram/delegation-layer/internal/pueue"

// TaskSupervisorObservation owns one returned client. MaintenanceDeferred is
// an invocation result, never permission for a later mutation. MaintenanceError
// retains a refused optional repair for reporting after fresh observation.
type TaskSupervisorObservation struct {
	Client              *pueue.Client
	MaintenanceDeferred bool
	MaintenanceError    error
}
