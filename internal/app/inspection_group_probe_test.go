package app

import (
	"context"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestEstablishedInspectionGroupSharesSupervisorGeneration(t *testing.T) {
	store, _, supervisor := newSupervisorProbeTestFixture(t)
	group, err := inspection.OpenGroup(store, supervisor.Binding())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := pueue.QueueSnapshot{Groups: map[string]pueue.Group{
		group.Name(): {Status: "Running", ParallelTasks: 1},
	}}
	if observeErr := group.Observe(snapshot); observeErr != nil {
		t.Fatal(observeErr)
	}
	if closeErr := group.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	leaseDone := make(chan error, 1)
	leaseContext, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() {
		leaseDone <- pueue.WithSupervisorLease(leaseContext, store.Root, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	t.Cleanup(func() {
		close(release)
		if leaseErr := <-leaseDone; leaseErr != nil {
			t.Error(leaseErr)
		}
	})
	select {
	case <-locked:
	case <-time.After(10 * time.Second):
		t.Fatal("fixture did not acquire shared supervisor lease")
	}
	if ensureErr := ensureInspectionGroup(store, supervisor, 5*time.Second); ensureErr != nil {
		t.Fatalf("healthy group readiness required an exclusive transition: %v", ensureErr)
	}
	if ensureErr := ensureInspectionGroupWithoutMigration(store, supervisor, 5*time.Second); ensureErr != nil {
		t.Fatalf("same-generation continuation required an exclusive transition: %v", ensureErr)
	}
}
