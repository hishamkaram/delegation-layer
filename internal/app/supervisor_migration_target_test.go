package app

import (
	"context"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestExplicitMigrationBusyPreservesSelectedInspectionSuccessor(t *testing.T) {
	scenario := newHistoricalInspectionMigrationScenario(t)
	fixture := scenario.fixture
	prepareInterruptedInspectionMigration(t, scenario, false)
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		pending, pendingErr := pueue.PrivateSupervisorMigrationPending(fixture.root)
		if pendingErr != nil {
			return pendingErr
		}
		if !pending {
			t.Fatal("busy migration lost its durable successor selection")
		}
		proposals, proposalErr := inspection.PreparedGroupMigrationSuccessors(ctx, fixture.store)
		if proposalErr != nil {
			return proposalErr
		}
		if len(proposals) != 2 {
			t.Fatalf("fixture must retain two distinct successor proposals: %d", len(proposals))
		}
		selected, selectErr := selectPreparedPrivateMigrationTarget(fixture.root, proposals)
		if selectErr != nil {
			return selectErr
		}
		if selected != scenario.successorGroup.Supervisor {
			t.Fatalf("busy migration selected a different proposal: %+v", selected)
		}
		retargetable, retargetErr := pueue.PrivateMigrationRetargetableWithLock(fixture.root)
		if retargetErr != nil {
			return retargetErr
		}
		if !retargetable {
			t.Fatal("busy migration advanced beyond prepared intent")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
