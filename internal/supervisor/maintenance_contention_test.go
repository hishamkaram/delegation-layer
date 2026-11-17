package supervisor_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

func TestMaintenanceAssessmentKeepsHeldTaskOwnerBlocked(t *testing.T) {
	store, td, _ := lifecyclefixture.NewAppTestTask(t, false)
	t.Cleanup(func() { lifecyclefixture.CloseAppTestTask(t, store, td) })
	lifecyclefixture.PrepareAppBudgetRecords(t, td)
	lifecyclefixture.RecordRecoverableLifetime(t, td, "held-task-boot")
	owner, err := td.TryAcquireLifetimeOwnerContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	assertHeldOwnerMaintenance(t, store, "task", td.TaskID)
}

func TestMaintenanceAssessmentKeepsHeldInspectionOwnerBlocked(t *testing.T) {
	store, auxiliary := runtimeAuxiliaryFixture(t, "inspection", time.Now().Add(time.Minute))
	operation, ok := auxiliary.(*inspection.Operation)
	if !ok {
		t.Fatal("inspection fixture returned another operation family")
	}
	owner, err := operation.TryAcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	assertHeldOwnerMaintenance(t, store, "inspection", operation.Request().IdentityID())
}

func assertHeldOwnerMaintenance(t *testing.T, store *taskdir.Store, kind, id string) {
	t.Helper()
	if err := pueue.WithBootstrapLockContext(t.Context(), store.Root, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	before := testsupport.SnapshotStore(t, store.Root)
	var assessment supervisor.MaintenanceAssessment
	err := pueue.WithExistingBootstrapLockContext(t.Context(), store.Root, func(locked context.Context) error {
		var observeErr error
		assessment, observeErr = supervisor.NewCleanup(nil, nil).AssessMaintenance(locked, store, runtimeTestBinding(store), nil)
		return observeErr
	})
	if !supervisor.MaintenanceBusy(err) || !errors.Is(err, task.ErrLockBusy) || assessment.Readiness != "blocked" || assessment.ActiveWork != "none" || assessment.FirstBlocker == nil {
		t.Fatalf("held %s owner became a hard observation fault: assessment=%+v error=%v", kind, assessment, err)
	}
	blocker := assessment.FirstBlocker
	if blocker.Kind != kind || blocker.ReasonCode != "owner_busy" || blocker.NextAction != "wait_for_legacy_work" {
		t.Fatalf("held owner lost precise wait guidance: %+v", blocker)
	}
	if (kind == "task" && blocker.TaskID != id) || (kind == "inspection" && blocker.OperationID != id) {
		t.Fatalf("held owner lost its resource identity: %+v", blocker)
	}
	if after := testsupport.SnapshotStore(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("passive contention assessment changed task or inspection evidence")
	}
}
