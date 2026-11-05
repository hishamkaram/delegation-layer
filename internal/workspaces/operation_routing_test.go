package workspaces

import (
	"context"
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type sourceRecordingOperationRuntime struct {
	unavailableOperationRuntime
	store  *taskdir.Store
	source *infrastructure.SavedBindingSource
}

func (r sourceRecordingOperationRuntime) OpenSaved(_ context.Context, store *taskdir.Store, binding task.SupervisorRef, source infrastructure.SavedBindingSource) (*pueue.Client, error) {
	if store != r.store {
		return nil, errors.New("workspace routing replaced actual Store")
	}
	if err := source.ValidateBinding(binding); err != nil {
		return nil, err
	}
	*r.source = source
	return nil, errOperationFixtureRuntimeUnavailable
}

func TestWorkspaceRecoveryRoutesExactPersistedRequest(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	store, request := fixture.recovery.Store, fixture.operation.Request()
	var source infrastructure.SavedBindingSource
	runtime := sourceRecordingOperationRuntime{store: store, source: &source}
	_, err := ObserveCheckpoint(t.Context(), store, OperationOptions{Runtime: runtime}, request.OperationID)
	if !errors.Is(err, errOperationFixtureRuntimeUnavailable) {
		t.Fatalf("workspace recovery did not reach source-aware runtime: %v", err)
	}
	if source.Kind != infrastructure.SavedBindingWorkspaceRequest || source.RootID != request.RootID || source.OwnerID != request.OperationID || source.RecordID != "" {
		t.Fatal("workspace recovery lost immutable request locator")
	}
	if err = source.ValidateBinding(request.Supervisor); err != nil {
		t.Fatal(err)
	}
}
