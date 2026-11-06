package workspaces

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestCheckpointCanceledObserverKeepsOwnedRecoveryContext(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openOrdinaryCheckpoint(t, store, strings.Repeat("e", 32), time.Now().Add(-time.Second))
	deadline := operation.Deadline()
	observer, cancel := context.WithCancel(t.Context())
	cancel()
	description, err := ObserveCheckpoint(observer, store, OperationOptions{}, operation.Request().OperationID)
	if err != nil || description.ID != operation.Request().OperationID || description.State != workspace.CheckpointUnavailable {
		t.Fatalf("canceled observer canceled owned checkpoint recovery: %+v, %v", description, err)
	}
	if operation.Deadline() != deadline {
		t.Fatal("observation renewed the stored helper deadline")
	}
}
