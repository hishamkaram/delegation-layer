package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (fixture labelFilterCleanupFixture) stateRoot() string {
	return filepath.Dir(fixture.base)
}

func cleanupTestForeignLabel() string {
	return Identity{RootID: strings.Repeat("f", 32), TaskID: strings.Repeat("4", 32)}.Label()
}

// TestCompletedTaskCleanupObservesAbsenceBesideSharedLease proves that an
// already-absent label settles under the shared generation lease. A held
// shared lease would block the exclusive lease until the bound expires, so an
// exclusive observation fails this test with a deadline. The unrelated
// occupant of the saved numeric ID is never removed.
func TestCompletedTaskCleanupObservesAbsenceBesideSharedLease(t *testing.T) {
	fixture := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 10 * time.Second})
	fixture.install(t, cleanupTestRow{id: 3, label: cleanupTestForeignLabel(), state: StateEnded})
	held := supervisorLeaseTestHold(t, fixture.stateRoot(), WithSupervisorLease)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	disposition, err := fixture.client.AttemptCompletedTaskCleanup(ctx, fixture.identity)
	if disposition != CompletedTaskLogicalLabelAbsent || err != nil {
		t.Fatalf("absent cleanup beside a shared lease = %d, %v; want logical absence", disposition, err)
	}
	assertCleanupTestCommands(t, fixture, 1, false)
	if err = fixture.client.RemoveCompletedTask(ctx, fixture.identity); !errors.Is(err, ErrUnknown) || !bareCleanupMarker(err) {
		t.Fatalf("legacy absent cleanup beside a shared lease = %v, want bare %v", err, ErrUnknown)
	}
	assertCleanupTestCommands(t, fixture, 2, false)
	held.finish(t)
}

// TestCompletedTaskCleanupRemovalWaitsForExclusiveLease keeps removal fenced:
// a present ended row observed under the shared lease cannot be removed while
// another shared lease is held, and is removed after a fresh exclusive
// selection once that lease is released.
func TestCompletedTaskCleanupRemovalWaitsForExclusiveLease(t *testing.T) {
	fixture := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 10 * time.Second})
	fixture.install(t, cleanupTestRow{id: 3, label: fixture.identity.Label(), state: StateEnded})
	held := supervisorLeaseTestHold(t, fixture.stateRoot(), WithSupervisorLease)
	// The deadline only observes deliberate exclusive-lease contention.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	disposition, err := fixture.client.AttemptCompletedTaskCleanup(ctx, fixture.identity)
	cancel()
	for _, pending := range pendingHandles(err) {
		waitPending(t, pending)
	}
	if disposition != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("removal beside a shared lease = %d, %v; want exclusive-lease deadline", disposition, err)
	}
	assertCleanupTestCommands(t, fixture, 1, false)
	held.finish(t)
	if disposition, err = fixture.client.AttemptCompletedTaskCleanup(t.Context(), fixture.identity); disposition != CompletedTaskRemoved || err != nil {
		t.Fatalf("removal after lease release = %d, %v; want removed", disposition, err)
	}
	assertCleanupTestCommands(t, fixture, 3, true)
}

// TestCompletedTaskCleanupWaitsForExclusiveTransition keeps even an absent
// label behind a held generation transition: the shared observation issues no
// query until the exclusive owner releases.
func TestCompletedTaskCleanupWaitsForExclusiveTransition(t *testing.T) {
	fixture := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 10 * time.Second})
	fixture.install(t)
	held := supervisorLeaseTestHold(t, fixture.stateRoot(), WithBootstrapLock)
	// The deadline only observes deliberate lease contention.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	disposition, err := fixture.client.AttemptCompletedTaskCleanup(ctx, fixture.identity)
	cancel()
	if disposition != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup beside an exclusive transition = %d, %v; want lease deadline", disposition, err)
	}
	assertCleanupTestCommands(t, fixture, 0, false)
	held.finish(t)
}

// TestCompletedTaskCleanupReselectsUnderExclusiveLease changes the exact label
// rows after the shared observation returned an ended row. Only the fresh
// exclusive selection decides, so none of these interleavings removes a row.
func TestCompletedTaskCleanupReselectsUnderExclusiveLease(t *testing.T) {
	for _, testCase := range []struct {
		name string
		next func(label string) []cleanupTestRow
		want CompletedTaskCleanup
	}{
		{name: "label removed", want: CompletedTaskLogicalLabelAbsent, next: func(string) []cleanupTestRow { return nil }},
		{name: "saved ID reused", want: CompletedTaskLogicalLabelAbsent, next: func(string) []cleanupTestRow {
			return []cleanupTestRow{{id: 3, label: cleanupTestForeignLabel(), state: StateEnded}}
		}},
		{name: "label duplicated", next: func(label string) []cleanupTestRow {
			return []cleanupTestRow{{id: 3, label: label, state: StateEnded}, {id: 4, label: label, state: StateEnded}}
		}},
		{name: "label at another ID", next: func(label string) []cleanupTestRow {
			return []cleanupTestRow{{id: 5, label: label, state: StateEnded}}
		}},
		{name: "row running", want: CompletedTaskCleanupDeferred, next: func(label string) []cleanupTestRow {
			return []cleanupTestRow{{id: 3, label: label, state: StateRunning}}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 10 * time.Second})
			fixture.install(t, cleanupTestRow{id: 3, label: fixture.identity.Label(), state: StateEnded})
			fixture.installAfterFirstQuery(t, testCase.next(fixture.identity.Label())...)
			disposition, err := fixture.client.AttemptCompletedTaskCleanup(t.Context(), fixture.identity)
			if disposition != testCase.want || (testCase.want == 0) != errors.Is(err, ErrUnknown) || (testCase.want != 0 && err != nil) {
				t.Fatalf("reselected cleanup = %d, %v; want disposition %d", disposition, err, testCase.want)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.base, "label-status-next")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("staged label rows were not served after the shared observation: %v", statErr)
			}
			assertCleanupTestCommands(t, fixture, 2, false)
		})
	}
}

// TestCompletedTaskCleanupSharedQueryTimeoutRetainsLease keeps a timed-out
// shared observation operational: the unfinished query retains its shared
// lease, so no exclusive owner enters until that command completes.
func TestCompletedTaskCleanupSharedQueryTimeoutRetainsLease(t *testing.T) {
	fixture := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 100 * time.Millisecond})
	fixture.install(t)
	writePrivateTestFile(t, filepath.Join(fixture.base, "status-hold"), "")
	disposition, err := fixture.client.AttemptCompletedTaskCleanup(t.Context(), fixture.identity)
	pending := pendingHandles(err)
	if disposition != 0 || len(pending) == 0 {
		t.Fatalf("timed-out shared observation = %d, %v; want a pending operational error", disposition, err)
	}
	supervisorLeaseTestBlocked(t, fixture.stateRoot(), WithBootstrapLock)
	for _, handle := range pending {
		waitPending(t, handle)
	}
	supervisorLeaseTestEnters(t, fixture.stateRoot(), WithBootstrapLock)
	assertCleanupTestCommands(t, fixture, 1, false)
}

// TestCompletedTaskCleanupProvisionsMissingBootstrapLockExclusively keeps a
// root without a published bootstrap lock on the original exclusive path,
// which provisions the canonical lock before it observes the label.
func TestCompletedTaskCleanupProvisionsMissingBootstrapLockExclusively(t *testing.T) {
	fixture := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 10 * time.Second})
	fixture.install(t)
	lockPath := filepath.Join(fixture.base, "bootstrap.lock")
	removePrivateTestPath(t, lockPath)
	disposition, err := fixture.client.AttemptCompletedTaskCleanup(t.Context(), fixture.identity)
	if disposition != CompletedTaskLogicalLabelAbsent || err != nil {
		t.Fatalf("cleanup without a published bootstrap lock = %d, %v; want logical absence", disposition, err)
	}
	info, statErr := os.Lstat(lockPath)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("exclusive cleanup did not provision the canonical bootstrap lock: %v", statErr)
	}
	assertCleanupTestCommands(t, fixture, 1, false)
}

// TestCompletedTaskCleanupUnsafeBootstrapLockIsNotProvisioned refuses an
// unprotected bootstrap lock without falling back to provisioning, repair, or
// a label query.
func TestCompletedTaskCleanupUnsafeBootstrapLockIsNotProvisioned(t *testing.T) {
	fixture := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 10 * time.Second})
	fixture.install(t)
	lockPath := filepath.Join(fixture.base, "bootstrap.lock")
	if err := os.Chmod(lockPath, 0o644); err != nil {
		t.Fatal(err)
	}
	disposition, err := fixture.client.AttemptCompletedTaskCleanup(t.Context(), fixture.identity)
	if disposition != 0 || !errors.Is(err, ErrConfiguration) || errors.Is(err, ErrPrivateSupervisorNotInitialized) {
		t.Fatalf("cleanup with an unprotected bootstrap lock = %d, %v; want configuration refusal", disposition, err)
	}
	if info, statErr := os.Lstat(lockPath); statErr != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("refused cleanup changed the unprotected bootstrap lock: %v", statErr)
	}
	assertCleanupTestCommands(t, fixture, 0, false)
}

// TestCompletedTaskCleanupUnreadableGenerationRefusesAbsence keeps the
// canonical generation guard ahead of the shared label observation: an
// unreadable published binding is a refusal, never logical absence.
func TestCompletedTaskCleanupUnreadableGenerationRefusesAbsence(t *testing.T) {
	fixture := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 10 * time.Second})
	fixture.install(t)
	bindingPath := privateSupervisorBindingPath(fixture.stateRoot())
	writePrivateTestFile(t, bindingPath, "{")
	disposition, err := fixture.client.AttemptCompletedTaskCleanup(t.Context(), fixture.identity)
	if disposition != 0 || !errors.Is(err, ErrBinding) || errors.Is(err, ErrUnknown) {
		t.Fatalf("cleanup with an unreadable generation = %d, %v; want binding refusal", disposition, err)
	}
	if err = fixture.client.RemoveCompletedTask(t.Context(), fixture.identity); !errors.Is(err, ErrBinding) || errors.Is(err, ErrUnknown) {
		t.Fatalf("legacy cleanup with an unreadable generation = %v; want binding refusal", err)
	}
	if got := string(readPrivateSupervisorFile(t, bindingPath)); got != "{" {
		t.Fatalf("refused cleanup changed the published binding: %q", got)
	}
	assertCleanupTestCommands(t, fixture, 0, false)
}

// opaqueUninitializedTestError claims the refusal sentinel through its own Is
// method without wrapping it, so only its identity differs from the sentinel.
type opaqueUninitializedTestError struct{}

func (opaqueUninitializedTestError) Error() string { return "opaque uninitialized fault" }

func (opaqueUninitializedTestError) Is(target error) bool {
	return target == ErrPrivateSupervisorNotInitialized
}

// TestPrivateLeaseUninitializedRequiresBareUnenteredRefusal admits only the
// shared lease's bare refusal before callback entry; callback, wrapped,
// joined, and opaque matching faults never reach the exclusive provisioning
// path.
func TestPrivateLeaseUninitializedRequiresBareUnenteredRefusal(t *testing.T) {
	fault := errors.New("lease release fault")
	for _, testCase := range []struct {
		name    string
		entered bool
		err     error
		want    bool
	}{
		{name: "bare refusal", err: ErrPrivateSupervisorNotInitialized, want: true},
		{name: "callback refusal", entered: true, err: ErrPrivateSupervisorNotInitialized},
		{name: "wrapped refusal", err: fmt.Errorf("observe: %w", ErrPrivateSupervisorNotInitialized)},
		{name: "joined fault", err: errors.Join(ErrPrivateSupervisorNotInitialized, fault)},
		{name: "opaque custom Is", err: opaqueUninitializedTestError{}},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "configuration", err: ErrConfiguration},
		{name: "no error"},
	} {
		if got := privateLeaseUninitialized(testCase.entered, testCase.err); got != testCase.want {
			t.Errorf("%s: privateLeaseUninitialized = %t, want %t", testCase.name, got, testCase.want)
		}
	}
}
