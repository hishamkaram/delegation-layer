package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	guardianfixture "github.com/hishamkaram/delegation-layer/internal/guardian/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestLifetimeGuardianWithoutPlacementDoesNotCreateCustody(t *testing.T) {
	for name, newStore := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			closure, ok := s.records.(guardian.ClosureRecords)
			if !ok {
				t.Fatalf("%T is not a closure record family", s.records)
			}
			fixture := guardianfixture.NewRecordFixture(t, s.rootID, s.taskID, s.ownerKind, s.deadline, s.records, s.acquire, nil)
			fixture.Hello(t)
			registration, _ := readCustodyContractRegistration(t, s)
			if registration.CustodyPlacement != nil {
				t.Fatal("new lifetime unexpectedly selected a custody placement")
			}
			fixture.CloseTask(t)
			fixture.ReleaseWorker()
			if err := fixture.WaitRun(t); err != nil {
				t.Fatal(err)
			}
			if err := releaseLifetimeCustody(t.Context(), closure); err != nil {
				t.Fatal(err)
			}
			if pending, err := lifetimeCustodyPending(t.Context(), closure); err != nil || pending {
				t.Fatalf("unplaced lifetime cleanup = %t, %v", pending, err)
			}
			if _, err := os.Lstat(filepath.Join(s.store.Root, ".custody")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("new lifetime created a custody pool: %v", err)
			}
		})
	}
}

// Legacy registrations still release their saved views only after closure,
// including when guardian execution itself returned an error.
func TestLegacyCustodyReleasesOnlyAfterGuardianClosure(t *testing.T) {
	for name, newStore := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			closure, ok := s.records.(guardian.ClosureRecords)
			if !ok {
				t.Fatalf("%T is not a closure record family", s.records)
			}
			placement := newCustodyTestPlacement(t)
			fixture := guardianfixture.NewRecordFixture(t, s.rootID, s.taskID, s.ownerKind, s.deadline, s.records, s.acquire,
				func(options *guardian.Options) {
					options.SelectPlacement = func(context.Context) (*task.LifetimeCustodyPlacement, error) { return placement, nil }
				})
			fixture.Hello(t)
			registration, _ := readCustodyContractRegistration(t, s)
			bindCustodyTestView(t, registration)
			if err := releaseLifetimeCustody(t.Context(), closure); err != nil || !custodyTestPending(t, registration) {
				t.Fatalf("release before closure: %v; want view retained", err)
			}
			fixture.CloseTask(t)
			fixture.ReleaseWorker()
			if err := fixture.WaitRun(t); err != nil {
				t.Fatal(err)
			}
			requireGuardianReleaseAfterRunError(t, s, closure, registration)
		})
	}
}

func requireGuardianReleaseAfterRunError(t *testing.T, s custodyContractStore, closure guardian.ClosureRecords, registration *task.LifetimeRegistrationRecord) {
	t.Helper()
	var failed *guardian.Guardian
	wantErr := failed.Run(context.Background())
	parsed := runnerArguments{TaskID: s.taskID, Inspection: true}
	if err := runGuardianWithWorkspaceCompletion(failed, lifetimeGuardianRunState{}, s.store, parsed, Dependencies{}); err == nil || err.Error() != wantErr.Error() || !custodyTestPending(t, registration) {
		t.Fatalf("run without closure records: error %v, want %v and no release", err, wantErr)
	}
	if err := runGuardianWithWorkspaceCompletion(failed, lifetimeGuardianRunState{closure: closure}, s.store, parsed, Dependencies{}); err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("failed run error %v, want unchanged %v", err, wantErr)
	}
	if custodyTestPending(t, registration) {
		t.Fatal("guardian did not release custody after a proven closure")
	}
}
