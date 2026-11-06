package infrastructure

import (
	"context"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOwnerManualReportSurvivesLateBusyWithoutDisposition(t *testing.T) {
	report := ManualMigrationReport{State: ManualMigrationMigrated, Legacy: ManualMigrationLegacyRecovered, Active: ManualMigrationActiveNone}
	f := newOwnerServerFixture(t, &ownerServerTestHost{manual: func(context.Context, Operation, LaunchContext, ResidentGuards, DaemonWaitOwner) (Disposition, ManualMigrationReport, error) {
		return Disposition{}, report, task.ErrLockBusy
	}})
	f.start()
	owner := f.request(t, ownerStatus).Owner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, f.entry.journal), owner, PreparationRequest{Operation: InitialOperation{Kind: InitialMigrateOwn}})
	if err != nil || outcome.Condition != PreparationFault || outcome.Report != report || outcome.Disposition != (Disposition{}) {
		t.Fatalf("late busy erased acknowledged manual report: outcome=%v err=%v", outcome, err)
	}
}

func TestOwnerManualSuccessRequiresReport(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{manual: func(context.Context, Operation, LaunchContext, ResidentGuards, DaemonWaitOwner) (Disposition, ManualMigrationReport, error) {
		return Disposition{Kind: DispositionWarmReuse}, ManualMigrationReport{}, nil
	}})
	f.start()
	owner := f.request(t, ownerStatus).Owner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, f.entry.journal), owner, PreparationRequest{Operation: InitialOperation{Kind: InitialMigrateOwn}})
	if err != nil || outcome.Condition != PreparationFault || !outcome.Failure.IndependentFault {
		t.Fatalf("manual success omitted required report: outcome=%v err=%v", outcome, err)
	}
	reply := ownerServerReply(f.request(t, ownerMigrateOwn), ownerOK)
	reply.Target = originRecord{Root: f.root, RootID: reply.RootID}
	reply.Startup = owner
	reply.Disposition = Disposition{Kind: DispositionWarmReuse}
	if err = reply.validate(); err == nil {
		t.Fatal("manual OK frame without report accepted")
	}
}
