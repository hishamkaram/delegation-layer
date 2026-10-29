package sqlitestore

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func claimed(t *testing.T, store *Store, claim contract.Claim) {
	t.Helper()
	result, err := store.ClaimAndArm(testContext(t), claim)
	if err != nil || result != (contract.ClaimResult{Outcome: contract.ClaimNewlyCommitted, TaskID: claim.TaskID}) {
		t.Fatalf("claim %+v = %+v, %v", claim, result, err)
	}
}

func testObservation(taskID string, observedAt int64) contract.OccurrenceObservation {
	return contract.OccurrenceObservation{TaskID: taskID, Observation: contract.Observation{
		ObservedAt: at(observedAt), Admission: task.AdmissionAdmitted, Liveness: task.LivenessRunning, Publication: task.PublicationPending,
	}}
}

func testRelease(taskID string, releasedAt int64) contract.OccurrenceRelease {
	return contract.OccurrenceRelease{TaskID: taskID, Release: contract.Release{ReleasedAt: at(releasedAt), Kind: contract.ReleaseClosed, Ref: testClosureSHA256}}
}

func requireClaimRefused(t *testing.T, store *Store, claim contract.Claim, want error) {
	t.Helper()
	result, err := store.ClaimAndArm(testContext(t), claim)
	if !errors.Is(err, want) || result != (contract.ClaimResult{}) {
		t.Fatalf("claim %+v = %+v, %v; want zero ClaimUnknown and %v", claim, result, err, want)
	}
}

func TestClaimAndArmGrantsOnlyItsOwnNewCommit(t *testing.T) {
	store, _ := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	claimed(t, store, testClaim(testScheduleID, testTaskID, 1_000, 2_000))
	existing := contract.ClaimResult{Outcome: contract.ClaimExisting, TaskID: testTaskID}
	for _, retry := range []contract.Claim{
		testClaim(testScheduleID, testTaskID, 1_000, 2_000),
		testClaim(testScheduleID, otherTaskID, 1_000, 2_000),
	} {
		if result, err := store.ClaimAndArm(testContext(t), retry); err != nil || result != existing {
			t.Fatalf("repeat claim %+v = %+v, %v; want %+v", retry, result, err, existing)
		}
	}
	requireClaimRefused(t, store, testClaim(testScheduleID, otherTaskID, 2_000, 3_000), contract.ErrConflict)
	if err := store.Release(testContext(t), testRelease(testTaskID, 2_500)); err != nil {
		t.Fatal(err)
	}
	requireClaimRefused(t, store, testClaim(testScheduleID, otherTaskID, 1_000, 3_000), contract.ErrConflict)
	requireClaimRefused(t, store, testClaim(otherSchedule, otherTaskID, 1_000, 3_000), fs.ErrNotExist)
	createSchedule(t, store, otherSchedule)
	requireClaimRefused(t, store, testClaim(otherSchedule, testTaskID, 1_000, 3_000), contract.ErrConflict)
	pause := contract.StateChange{ScheduleID: otherSchedule, From: contract.StateEnabled, To: contract.StatePaused}
	if err := store.ChangeState(testContext(t), pause); err != nil {
		t.Fatal(err)
	}
	requireClaimRefused(t, store, testClaim(otherSchedule, otherTaskID, 1_000, 3_000), contract.ErrConflict)
	claimed(t, store, testClaim(testScheduleID, otherTaskID, 2_000, 3_000))
	requireSchedule(t, store, contract.StateEnabled, 3_000)
	if _, err := store.ClaimAndArm(testContext(t), testClaim(testScheduleID, otherTaskID, 3_000, 3_000)); err == nil {
		t.Fatal("claim with due at the cursor accepted")
	}
}

func TestObservationReplacesOnlyAStrictlyNewerOne(t *testing.T) {
	store, _ := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	claimed(t, store, testClaim(testScheduleID, testTaskID, 1_000, 2_000))
	stored := testObservation(testTaskID, 3_000)
	if err := store.RecordObservation(testContext(t), stored); err != nil {
		t.Fatal(err)
	}
	committed := contract.Observation{
		Admission: task.AdmissionAdmitted, Liveness: task.LivenessEnded,
		Publication: task.PublicationCommitted, OutcomeSHA256: testOutcomeSHA256, FaultCode: "late_reader",
	}
	for _, observedAt := range []int64{3_000, 2_999} {
		committed.ObservedAt = at(observedAt)
		if err := store.RecordObservation(testContext(t), contract.OccurrenceObservation{TaskID: testTaskID, Observation: committed}); err != nil {
			t.Fatalf("stale observation at %d: %v", observedAt, err)
		}
		requireObservation(t, store, stored.Observation)
	}
	committed.ObservedAt = at(3_001)
	if err := store.RecordObservation(testContext(t), contract.OccurrenceObservation{TaskID: testTaskID, Observation: committed}); err != nil {
		t.Fatal(err)
	}
	requireObservation(t, store, committed)
	if err := store.RecordObservation(testContext(t), testObservation(otherTaskID, 4_000)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing occurrence err=%v, want fs.ErrNotExist", err)
	}
}

func requireObservation(t *testing.T, store *Store, want contract.Observation) {
	t.Helper()
	occurrence, err := store.GetOccurrence(testContext(t), testTaskID)
	if err != nil || occurrence.Observation == nil || !reflect.DeepEqual(*occurrence.Observation, want) {
		t.Fatalf("observation %+v, %v; want %+v", occurrence.Observation, err, want)
	}
}

func TestReleaseIsOneImmutableFact(t *testing.T) {
	store, _ := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	claimed(t, store, testClaim(testScheduleID, testTaskID, 1_000, 2_000))
	release := testRelease(testTaskID, 3_000)
	if err := store.Release(testContext(t), release); err != nil {
		t.Fatal(err)
	}
	repeat := release
	repeat.Release.ReleasedAt = at(9_000)
	if err := store.Release(testContext(t), repeat); err != nil {
		t.Fatalf("idempotent repeat: %v", err)
	}
	different := release
	different.Release = contract.Release{ReleasedAt: at(3_000), Kind: contract.ReleaseNotCreated, Ref: "admission_refused"}
	if err := store.Release(testContext(t), different); !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("different release err=%v, want ErrConflict", err)
	}
	occurrence, err := store.GetOccurrence(testContext(t), testTaskID)
	if err != nil || occurrence.Release == nil || !reflect.DeepEqual(*occurrence.Release, release.Release) {
		t.Fatalf("stored release %+v, %v; want %+v", occurrence.Release, err, release.Release)
	}
	if err := store.Release(testContext(t), testRelease(otherTaskID, 3_000)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing occurrence err=%v, want fs.ErrNotExist", err)
	}
	if _, err := store.GetOccurrence(testContext(t), otherTaskID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing get err=%v, want fs.ErrNotExist", err)
	}
}

func TestActiveOccurrencesPageOnlyHeldReservations(t *testing.T) {
	store, _ := initializedStore(t)
	third := "33333333333333333333333333333333"
	tasks := map[string]string{testScheduleID: testTaskID, otherSchedule: otherTaskID, third: "cccccccccccccccccccccccccccccccc"}
	for _, id := range []string{testScheduleID, otherSchedule, third} {
		createSchedule(t, store, id)
		claimed(t, store, testClaim(id, tasks[id], 1_000, 2_000))
	}
	if err := store.Release(testContext(t), testRelease(otherTaskID, 3_000)); err != nil {
		t.Fatal(err)
	}
	first, err := store.ActiveOccurrences(testContext(t), contract.ActivePage{Limit: 1})
	if err != nil || !first.More || len(first.Occurrences) != 1 || first.Occurrences[0].TaskID != testTaskID {
		t.Fatalf("first page %+v, %v", first, err)
	}
	second, err := store.ActiveOccurrences(testContext(t), contract.ActivePage{AfterScheduleID: testScheduleID, Limit: 1})
	if err != nil || second.More || len(second.Occurrences) != 1 || second.Occurrences[0].ScheduleID != third {
		t.Fatalf("second page %+v, %v", second, err)
	}
}

func TestStoredOccurrenceTuplesAreValidatedNeverCoerced(t *testing.T) {
	cases := map[string]struct{ trigger, statement string }{
		"partial observation":  {statement: "UPDATE occurrences SET observed_at_unix_nanos = 1"},
		"facts without one":    {statement: "UPDATE occurrences SET fault_code = 'stray'"},
		"empty fault code":     {statement: "UPDATE occurrences SET observed_at_unix_nanos = 1, admission = 2, liveness = 1, publication = 1, fault_code = ''"},
		"wrapping admission":   {statement: "UPDATE occurrences SET observed_at_unix_nanos = 1, admission = 258, liveness = 1, publication = 1"},
		"unknown liveness":     {statement: "UPDATE occurrences SET observed_at_unix_nanos = 1, admission = 2, liveness = 7, publication = 1"},
		"outcome missing":      {statement: "UPDATE occurrences SET observed_at_unix_nanos = 1, admission = 2, liveness = 2, publication = 2"},
		"partial release":      {statement: "UPDATE occurrences SET released_at_unix_nanos = 1"},
		"unknown release kind": {statement: "UPDATE occurrences SET released_at_unix_nanos = 1, release_kind = 'lost', release_ref = 'x'"},
		"invalid task id":      {trigger: "occurrences_identity_immutable", statement: "UPDATE occurrences SET task_id = '" + testTaskID[:31] + "G'"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store, root := initializedStore(t)
			createSchedule(t, store, testScheduleID)
			claimed(t, store, testClaim(testScheduleID, testTaskID, 1_000, 2_000))
			if tc.trigger != "" {
				bypassTrigger(t, root, tc.trigger, "PRAGMA ignore_check_constraints = ON", tc.statement)
			} else {
				bypassChecks(t, root, tc.statement)
			}
			if tc.trigger == "" {
				if occurrence, err := store.GetOccurrence(testContext(t), testTaskID); !errors.Is(err, task.ErrInvariantFault) {
					t.Fatalf("get %+v, err=%v; want ErrInvariantFault", occurrence, err)
				}
			} else if page, err := store.ActiveOccurrences(testContext(t), contract.ActivePage{Limit: 1}); !errors.Is(err, task.ErrInvariantFault) {
				t.Fatalf("active page %+v, err=%v; want ErrInvariantFault", page, err)
			}
			history, err := store.History(testContext(t), contract.HistoryPage{ScheduleID: testScheduleID, Limit: 1})
			if !errors.Is(err, task.ErrInvariantFault) {
				t.Fatalf("history %+v, err=%v; want ErrInvariantFault", history, err)
			}
		})
	}
}
