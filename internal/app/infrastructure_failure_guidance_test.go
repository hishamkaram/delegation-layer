package app

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func TestInfrastructureIncompatibleCannotEnterMigrationLoop(t *testing.T) {
	for _, cause := range []error{
		infrastructure.ErrOwnerIncompatible,
		errors.Join(infrastructure.ErrOwnerIncompatible, pueue.ErrPrivateSupervisorMigrationRequired),
		errors.Join(infrastructure.ErrOwnerIncompatible, pueue.ErrPrivateSupervisorMigrationBusy),
	} {
		for _, record := range []string{"not_created", "created"} {
			failure := dispatchFailure(cause, record)
			if failure.Code != "supervisor_owner_incompatible" || failure.NextAction != "stop_and_report" || failure.RecoveryCommand != "" {
				t.Fatalf("incompatible owner entered migration/status loop: %+v", failure)
			}
			if dispatchFailureMessage(failure.Code) == "" {
				t.Fatal("incompatible owner has no public guidance")
			}
		}
	}
}
