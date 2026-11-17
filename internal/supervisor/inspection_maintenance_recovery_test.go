package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestCooperativeInspectionSettlementIsBlockedWithoutHidingFaults(t *testing.T) {
	id := strings.Repeat("c", 32)
	lifetime := cooperativeInspectionBlockerLifetime(t, id)
	observation := inspection.MaintenanceObservation{Lifetime: taskdir.ControlLifetimeObservation{Records: lifetime, OwnerPresent: true}, SettlementEligible: true}
	required, err := inspectInspectionObservation(t.Context(), id, false, false, observation)
	if err != nil || !required {
		t.Fatalf("settlement guidance did not require canonical queue validation: required=%t err=%v", required, err)
	}
	_, err = inspectInspectionObservation(t.Context(), id, false, true, observation)
	var pending *MaintenanceError
	if !errors.As(err, &pending) || !MaintenanceBusy(err) || pending.Assessment.Readiness != "blocked" || pending.Assessment.FirstBlocker == nil || pending.Assessment.FirstBlocker.ReasonCode != "inspection_closure_unproven" || pending.Assessment.FirstBlocker.NextAction != "settle_inspection" {
		t.Fatalf("cooperative inspection did not report supported settlement blocker: %v", err)
	}
	if MaintenanceBusy(errors.Join(err, os.ErrPermission)) {
		t.Fatal("settlement blocker hid an independent evidence fault")
	}
	observation.Lifetime.OwnerPresent = false
	observation.SettlementEligible = false
	_, err = inspectInspectionObservation(t.Context(), id, false, true, observation)
	var fact *task.ControlFactError
	if MaintenanceBusy(err) || !errors.As(err, &fact) || !errors.Is(err, task.ErrEvidenceFault) || fact.ReasonCode != "inspection_exit_unobservable" {
		t.Fatalf("missing inspection owner was downgraded to settlement blocker: %v", err)
	}
}

func TestInspectionPreparationRetainsOnlyExactSettlementBlocker(t *testing.T) {
	settlement := auxiliaryMaintenancePending("inspection", strings.Repeat("c", 32), "inspection_closure_unproven", "settle_inspection")
	for _, test := range []struct {
		name  string
		cause error
		keep  bool
	}{
		{"eligible settlement", settlement, true},
		{"independent fault", errors.Join(settlement, os.ErrPermission), false},
		{"missing record", errors.Join(settlement, os.ErrNotExist), false},
		{"deadline recovery", auxiliaryMaintenancePending("inspection", strings.Repeat("c", 32), "recovery_required", "run_supervisor_ensure_owner"), false},
		{"unrelated busy", ErrRuntimeQuiescenceBusy, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if keep := inspectionSettlementPending(test.cause); keep != test.keep {
				t.Fatalf("keep settlement blocker=%t, want %t", keep, test.keep)
			}
		})
	}
}

func cooperativeInspectionBlockerLifetime(t *testing.T, id string) *taskdir.ControlLifetime {
	t.Helper()
	store := testsupport.NewInspectionStore(t)
	control, err := store.OpenInspectionContext(t.Context(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	deadline := time.Now().UTC().Add(time.Hour)
	lifetime, err := taskdir.NewControlLifetime(control, store.RootID, id, task.LifetimeOwnerInspection, task.ComputeSHA256([]byte("inspection settlement blocker")), deadline)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lifetime.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "inspection-blocker-plan", BootID: "inspection-blocker-boot", Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	_, err = lifetime.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopeID: "inspection-blocker-scope", BootID: "inspection-blocker-boot", Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	_, err = lifetime.RecordLifetimeWorkerContext(t.Context(), taskdir.LifetimeWorkerFacts{PID: 2101, ProcessStart: "inspection-blocker-worker", BootID: "inspection-blocker-boot"})
	if err != nil {
		t.Fatal(err)
	}
	return lifetime
}

func TestHistoricalRowlessInspectionPermitsMaintenanceWithoutDeletionAuthority(t *testing.T) {
	f := newCustodyCleanupFixture(t)
	row := f.endedInspection(t, "a", 4, inspection.Binding{})
	binding := testsupport.ContinuationSupervisorRef(f.store.Root, task.ComputeSHA256([]byte("later generation")), "unix:"+filepath.Join(f.store.Root, "runtime.sock"))
	before := testsupport.SnapshotStore(t, f.store.Root)
	// The old numeric ID can already belong to an unrelated ended history row.
	jobs := []pueue.Job{{ID: row.ID, State: pueue.StateEnded}}
	if err := CheckRuntimeQuiescence(t.Context(), f.store, binding, jobs); err != nil {
		t.Fatalf("validated rowless historical inspection blocked later generation: %v", err)
	}
	policy := NewCleanup(&scriptedInspectionEvidence{invalid: map[string]bool{strings.Repeat("a", 32): true}}, absentWorkspace)
	if err := policy.InspectEndedJob(t.Context(), f.store, f.legacy, row); err != nil {
		t.Fatalf("historical migration eligibility depended on generic native validator: %v", err)
	}
	plan, err := policy.AuthorizeSuccessfulCleanup(t.Context(), f.store.Root, f.supervisor, []pueue.Job{row})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.RemoveIDs) != 0 {
		t.Fatalf("maintenance eligibility granted row deletion: %v", plan.RemoveIDs)
	}
	if !reflect.DeepEqual(before, testsupport.SnapshotStore(t, f.store.Root)) {
		t.Fatal("historical maintenance observation changed evidence")
	}
}

func TestRuntimeInspectionBusyCannotHideLaterMissingEvidence(t *testing.T) {
	f := newCustodyCleanupFixture(t)
	f.unpreparedInspection(t, "a", 4, inspection.Binding{})
	f.unpreparedInspection(t, "b", 5, inspection.Binding{})
	first, err := inspection.LoadOperationContext(t.Context(), f.store, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := first.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	owner, err := first.TryAcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := owner.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	path := filepath.Join(f.store.Root, "inspections", strings.Repeat("b", 32))
	if err = os.Remove(filepath.Join(path, "request.json")); err != nil {
		t.Fatal(err)
	}
	candidates := runtimeCandidates{inspections: []string{strings.Repeat("a", 32), strings.Repeat("b", 32)}}
	err = inspectRuntimeCandidates(t.Context(), f.store, candidates, false, f.supervisor, nil)
	if !errors.Is(err, task.ErrLockBusy) || !errors.Is(err, os.ErrNotExist) || MaintenanceBusy(err) {
		t.Fatalf("later fault lost behind owner contention: %v", err)
	}
}
