package control

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PM-W1: write-ahead intent selects the durable successor before the queue
// authorizer runs. Busy retains that target and the live Legacy without PID
// capture, shutdown flags, or strict completion; shared observation stays ready.
func TestPrivateMigrationBusyRetainsPreparedTargetWithoutShutdown(t *testing.T) {
	fixture := newPreparedPrivateObservationPair(t)
	writePreparedPrivateQueue(t, fixture, true)
	err := migratePreparedPrivateWriter(t, fixture, fixture.successor)
	if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("active migration was not refused: %v", err)
	}
	assertBusyPreparedPrivateObservation(t, fixture)
	matching, err := PrivateRecoveryBindingMatches(fixture.stateRoot, fixture.legacy.Binding())
	if err != nil || !matching {
		t.Fatalf("Busy did not retain matching recovery alias: matching=%t err=%v", matching, err)
	}
	assertPreparedPrivatePublishedLegacy(t, fixture)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	assertPreparedPrivateObservationIncomplete(t, fixture)
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	for _, reader := range preparedPrivateObservationReaders() {
		if binding, err := readPreparedPrivateGeneration(t, fixture, fixture.legacy, reader); err != nil || binding != fixture.legacy.Binding() {
			t.Errorf("prepared Busy observation failed via %s: binding=%+v err=%v", reader.name, binding, err)
		}
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

// PM-W2: explicit migration by the exact published generation may retire its
// own quiescent alias without fabricated history. The public remover cannot.
func TestPrivateSameGenerationMigrationRetiresOnlyQuiescentAlias(t *testing.T) {
	for _, busy := range []bool{true, false} {
		t.Run(map[bool]string{true: "busy", false: "drained"}[busy], func(t *testing.T) {
			fixture := newPreparedPrivateObservationPair(t)
			persistPreparedPrivateLegacyAlias(t, fixture)
			writePreparedPrivateQueue(t, fixture, busy)
			before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
			err := migratePreparedPrivateWriter(t, fixture, fixture.legacy)
			if busy {
				if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
					t.Errorf("same-generation migration did not authorize active queue: %v", err)
				}
				assertPreparedPrivateObservationUnchanged(t, fixture, before)
			} else {
				if err != nil {
					t.Errorf("quiescent same-generation alias could not retire: %v", err)
				}
				assertPreparedPrivateAliasRetired(t, fixture)
			}
			assertPreparedPrivateIntentAbsent(t, fixture)
			assertPreparedPrivatePublishedLegacy(t, fixture)
			assertPreparedPrivateNoHistory(t, fixture)
			assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
		})
	}
}

func TestPrivateGenericRetirementKeepsSameGenerationHistoryGuard(t *testing.T) {
	fixture := newPreparedPrivateObservationPair(t)
	persistPreparedPrivateLegacyAlias(t, fixture)
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		return RetirePrivateRecoveryBindingContext(ctx, fixture.stateRoot, fixture.legacy.Binding())
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("generic retirement bypassed completed history: %v", err)
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

// PM-W3: the migration owner can withdraw only its exact pre-stop Legacy
// intent when the explicit writer selects Legacy. No reader deletes intent.
func TestPrivateLegacyWriterWithdrawsPreparedIntent(t *testing.T) {
	for _, busy := range []bool{true, false} {
		t.Run(map[bool]string{true: "busy", false: "drained"}[busy], func(t *testing.T) {
			fixture := newBusyPreparedPrivateObservation(t)
			writePreparedPrivateQueue(t, fixture, busy)
			aliasBefore := snapshotPrivateTestFiles(t, []string{privateRecoveryRecordPath(fixture.stateRoot), privateRecoveryConfigPath(fixture.stateRoot)})
			err := migratePreparedPrivateWriter(t, fixture, fixture.legacy)
			if busy {
				if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
					t.Errorf("prepared legacy writer did not authorize active queue: %v", err)
				}
				assertPreparedPrivateAliasFilesUnchanged(t, fixture, aliasBefore)
			} else {
				if err != nil {
					t.Errorf("prepared legacy intent could not be withdrawn: %v", err)
				}
				assertPreparedPrivateAliasRetired(t, fixture)
			}
			assertPreparedPrivateIntentAbsent(t, fixture)
			assertPreparedPrivatePublishedLegacy(t, fixture)
			assertPreparedPrivateNoHistory(t, fixture)
			assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
		})
	}
}

func TestPrivateSameGenerationMigrationRejectsForeignAlias(t *testing.T) {
	for _, fault := range []string{"foreign-recovery", "corrupt-recovery-alias", "alias-digest-mismatch"} {
		t.Run(fault, func(t *testing.T) {
			fixture := newPreparedPrivateObservationPair(t)
			persistPreparedPrivateLegacyAlias(t, fixture)
			faultPreparedPrivateObservation(t, fixture, fault)
			before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
			if err := migratePreparedPrivateWriter(t, fixture, fixture.legacy); err == nil {
				t.Error("migration retired conflicting alias or daemon")
			}
			assertPreparedPrivateObservationUnchanged(t, fixture, before)
		})
	}
}

func TestPrivateMigrationPublishesTargetBeforeQueueAuthorization(t *testing.T) {
	fixture := newPreparedPrivateObservationPair(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	authorizationFailure := errors.New("injected queue authorization refusal")
	authorizerCalled := false
	fixture.successor.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
		authorizerCalled = true
		pending, found, err := loadPrivateMigrationRecord(base)
		if err != nil || !found || pending != preparedPrivateObservationRecord(fixture) {
			t.Errorf("queue authorization ran without exact target intent: found=%t pending=%+v err=%v", found, pending, err)
		}
		return authorizationFailure
	}
	if err := migratePreparedPrivateWriter(t, fixture, fixture.successor); !errors.Is(err, authorizationFailure) {
		t.Fatalf("queue authorization failure was not preserved: %v", err)
	}
	if !authorizerCalled {
		t.Fatal("migration did not reach queue authorization")
	}
	assertBusyPreparedPrivateObservation(t, fixture)
	assertPreparedPrivateNoHistory(t, fixture)
	if err := fixture.legacy.Ready(t.Context()); err != nil {
		t.Fatalf("authorization refusal stopped predecessor: %v", err)
	}
	fixture.successor.options.PrivateMigrationAuthorizer = allowPrivateMigrationForTest
	if err := migratePreparedPrivateWriter(t, fixture, fixture.successor); err != nil {
		t.Fatalf("prepared authorization refusal was not retryable: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func migratePreparedPrivateWriter(t *testing.T, fixture preparedPrivateObservationFixture, writer *Client) error {
	t.Helper()
	return WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		return writer.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot)
	})
}

func persistPreparedPrivateLegacyAlias(t *testing.T, fixture preparedPrivateObservationFixture) {
	t.Helper()
	withPreparedPrivateFaultLock(t, fixture, func() {
		if err := fixture.legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
			t.Fatal(err)
		}
		// Released aliases can omit launch provenance; the independently
		// published daemon record still owns the physical generation.
		record, found, err := readPrivateRecoveryBinding(fixture.stateRoot)
		if err != nil || !found {
			t.Fatalf("read legacy alias: found=%t err=%v", found, err)
		}
		record.Supervisor.DaemonExecutable, record.Supervisor.DaemonSHA256 = "", ""
		writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(fixture.stateRoot), record)
	})
}

func assertPreparedPrivateAliasFilesUnchanged(t *testing.T, fixture preparedPrivateObservationFixture, before map[string][]byte) {
	t.Helper()
	after := snapshotPrivateTestFiles(t, []string{privateRecoveryRecordPath(fixture.stateRoot), privateRecoveryConfigPath(fixture.stateRoot)})
	for path, want := range before {
		if string(after[path]) != string(want) {
			t.Errorf("Busy withdrawal changed recovery alias %s", filepath.Base(path))
		}
	}
}

func writePreparedPrivateQueue(t *testing.T, fixture preparedPrivateObservationFixture, busy bool) {
	t.Helper()
	data := []byte(`{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}`)
	if busy {
		data = []byte(preparedPrivateObservationStatus)
	}
	writePrivateSuccessorTestBytes(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), data)
}

func assertPreparedPrivateIntentAbsent(t *testing.T, fixture preparedPrivateObservationFixture) {
	t.Helper()
	if pending, err := PrivateSupervisorMigrationPending(fixture.stateRoot); err != nil || pending {
		t.Errorf("unexpected migration intent: pending=%t err=%v", pending, err)
	}
}

func assertPreparedPrivateNoHistory(t *testing.T, fixture preparedPrivateObservationFixture) {
	t.Helper()
	if _, found, err := loadPrivateMigrationHistory(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)); err != nil || found {
		t.Errorf("same-generation operation fabricated migration history: found=%t err=%v", found, err)
	}
}

func assertPreparedPrivateAliasRetired(t *testing.T, fixture preparedPrivateObservationFixture) {
	t.Helper()
	if present, err := PrivateRecoveryBindingPresent(fixture.stateRoot); err != nil || present {
		t.Errorf("quiescent alias still present: present=%t err=%v", present, err)
	}
	if complete, err := PrivateSupervisorMigrationComplete(fixture.stateRoot, fixture.legacy.Binding()); err != nil || !complete {
		t.Errorf("retired same-generation migration did not complete: complete=%t err=%v", complete, err)
	}
}
