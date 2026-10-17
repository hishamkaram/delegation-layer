package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var errOperationFixtureRuntimeUnavailable = errors.New("fixture runtime unavailable")

// This boundary failure lets release recovery prove that the durable release
// precedes removal preparation, without depending on an installed runner.
type unavailableOperationRuntime struct{}

func (unavailableOperationRuntime) ManagedRunner() (string, error) {
	return "", errOperationFixtureRuntimeUnavailable
}

func (unavailableOperationRuntime) PrepareManaged(context.Context, *taskdir.Store, string) (string, *pueue.Client, error) {
	return "", nil, errOperationFixtureRuntimeUnavailable
}

func (unavailableOperationRuntime) OpenSaved(context.Context, string, task.SupervisorRef) (*pueue.Client, error) {
	return nil, errOperationFixtureRuntimeUnavailable
}
