package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PM-H1: retry after successor publication uses completed private history to
// retire the historical alias. Active successor work is not predecessor work;
// the self-alias queue authorization must not run on this historical path.
func TestPrivatePublishedSuccessorRetryRetiresHistoricalAlias(t *testing.T) {
	for _, changedLogical := range []bool{false, true} {
		for _, busy := range []bool{false, true} {
			for _, consumer := range []string{"generic", "explicit-migration"} {
				t.Run(historicalPrivateRetirementCase(changedLogical, busy, consumer), func(t *testing.T) {
					assertHistoricalPrivateRetirementConsumer(t, changedLogical, busy, true, consumer)
				})
			}
		}
	}
}

func TestPrivateHistoricalSameLogicalActiveFullProvenance(t *testing.T) {
	for _, consumer := range []string{"generic", "explicit-migration"} {
		t.Run(consumer, func(t *testing.T) {
			assertHistoricalPrivateRetirementConsumer(t, false, true, false, consumer)
		})
	}
}

func TestPrivateHistoricalRetirementPreservesCorruptHistory(t *testing.T) {
	for _, consumer := range []string{"generic", "explicit-migration"} {
		t.Run(consumer, func(t *testing.T) {
			fixture := newHistoricalPrivateRetirementFixture(t, false, true)
			writePreparedPrivateQueue(t, fixture, false)
			withPreparedPrivateFaultLock(t, fixture, func() {
				writePrivateSuccessorTestBytes(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)), []byte("{"))
			})
			before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
			authorizations := 0
			fixture.successor.options.PrivateMigrationAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) error {
				authorizations++
				return allowPrivateMigrationForTest(ctx, root, binding, jobs)
			}
			err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
				if consumer == "generic" {
					return RetirePrivateRecoveryBindingContext(ctx, fixture.stateRoot, fixture.successor.Binding())
				}
				return fixture.successor.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot)
			})
			if !errors.Is(err, ErrBinding) || authorizations != 0 {
				t.Errorf("corrupt history bypassed canonical authorization: queue authorizations=%d err=%v", authorizations, err)
			}
			assertPreparedPrivateObservationUnchanged(t, fixture, before)
		})
	}
}

func historicalPrivateRetirementCase(changedLogical, busy bool, consumer string) string {
	logical := "same-logical"
	if changedLogical {
		logical = "changed-logical"
	}
	queue := "drained-successor"
	if busy {
		queue = "active-successor"
	}
	return logical + "/" + queue + "/" + consumer
}

func assertHistoricalPrivateRetirementConsumer(t *testing.T, changedLogical, busy, daemonless bool, consumer string) {
	t.Helper()
	fixture := newHistoricalPrivateRetirementFixture(t, changedLogical, daemonless)
	writePreparedPrivateQueue(t, fixture, busy)
	paths := historicalPrivateRetirementRetainedPaths(fixture)
	before := snapshotPrivateTestFiles(t, paths)
	authorizations := 0
	fixture.successor.options.PrivateMigrationAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) error {
		authorizations++
		return allowPrivateMigrationForTest(ctx, root, binding, jobs)
	}
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		if consumer == "generic" {
			return RetirePrivateRecoveryBindingContext(ctx, fixture.stateRoot, fixture.successor.Binding())
		}
		return fixture.successor.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot)
	})
	if err != nil {
		t.Errorf("published successor could not retire completed historical alias: %v", err)
	}
	if authorizations != 0 {
		t.Errorf("historical alias was classified as a current self-alias: queue authorizations=%d", authorizations)
	}
	assertHistoricalPrivateAliasRetired(t, fixture)
	after := snapshotPrivateTestFiles(t, paths)
	for path, want := range before {
		if !bytes.Equal(after[path], want) {
			t.Errorf("historical alias retirement changed retained %s", filepath.Base(path))
		}
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func newHistoricalPrivateRetirementFixture(t *testing.T, changedLogical, daemonless bool) preparedPrivateObservationFixture {
	t.Helper()
	fixture := newPreparedPrivateObservationPair(t)
	persistPreparedPrivateLegacyAlias(t, fixture)
	withPreparedPrivateFaultLock(t, fixture, func() {
		if !daemonless {
			record, found, err := readPrivateRecoveryRecord(fixture.stateRoot)
			if err != nil || !found {
				t.Fatalf("read full-provenance historical alias: found=%t err=%v", found, err)
			}
			record.Supervisor = fixture.legacy.Binding()
			writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(fixture.stateRoot), record)
		}
		if changedLogical {
			binding := fixture.successor.Binding()
			data := append(readPrivateSupervisorFile(t, binding.ConfigPath), []byte("\n# completed logical migration\n")...)
			writePrivateSuccessorTestBytes(t, binding.ConfigPath, data)
			current, err := Bind(t.Context(), binding.ClientExecutable, binding.ConfigPath, fixture.successor.options)
			if err != nil {
				t.Fatal(err)
			}
			if err = attachPrivateDaemon(current, binding.DaemonExecutable); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := current.Close(); err != nil {
					t.Error(err)
				}
			})
			fixture.successor = current
		}
		seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, fixture.legacy.Binding(), fixture.successor.Binding())
		assertHistoricalPrivateAliasAuthority(t, fixture, changedLogical, daemonless)
	})
	if pending, err := PrivateSupervisorMigrationPending(fixture.stateRoot); err != nil || pending {
		t.Fatalf("historical crash fixture still has pending private intent: pending=%t err=%v", pending, err)
	}
	return fixture
}

func assertHistoricalPrivateAliasAuthority(t *testing.T, fixture preparedPrivateObservationFixture, changedLogical, daemonless bool) {
	t.Helper()
	saved, found, err := ReadPrivateRecoveryBinding(fixture.stateRoot)
	want := fixture.legacy.Binding()
	if daemonless {
		want.DaemonExecutable, want.DaemonSHA256 = "", ""
	}
	if err != nil || !found || saved.DaemonExecutable != want.DaemonExecutable || saved.DaemonSHA256 != want.DaemonSHA256 {
		t.Fatalf("historical alias launch provenance changed: found=%t saved=%+v err=%v", found, saved, err)
	}
	wire := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot))
	wireOmitsDaemon := !bytes.Contains(wire, []byte(`"daemon_executable"`)) && !bytes.Contains(wire, []byte(`"daemon_sha256"`))
	if wireOmitsDaemon != daemonless {
		t.Fatalf("historical alias wire provenance differs: wantDaemonless=%t omitted=%t", daemonless, wireOmitsDaemon)
	}
	matches, err := PrivateRecoveryBindingMatches(fixture.stateRoot, fixture.successor.Binding())
	if err != nil || matches == changedLogical {
		t.Fatalf("logical-only match fact changed: changedLogical=%t matches=%t err=%v", changedLogical, matches, err)
	}
	connected, err := privateSupervisorSuccessorConnected(t.Context(), fixture.stateRoot, saved, fixture.successor.Binding())
	if err != nil || !connected {
		t.Fatalf("canonical completed history did not authorize alias: connected=%t err=%v", connected, err)
	}
	t.Logf("ALIAS_FACTS daemon_fields_omitted=%t matches=%t completed_history_connected=%t", daemonless, matches, connected)
}

func historicalPrivateRetirementRetainedPaths(fixture preparedPrivateObservationFixture) []string {
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	return []string{
		privateSupervisorBindingPath(fixture.stateRoot), privateDaemonIdentityPath(base),
		privateMigrationHistoryPath(base), PrivateConfigPath(fixture.stateRoot),
		filepath.Join(base, "test-status.json"), filepath.Join(base, "ready"),
		filepath.Join(base, "daemon-started"),
	}
}

func assertHistoricalPrivateAliasRetired(t *testing.T, fixture preparedPrivateObservationFixture) {
	t.Helper()
	if found, err := PrivateRecoveryBindingPresent(fixture.stateRoot); err != nil || found {
		t.Errorf("completed historical alias remains: found=%t err=%v", found, err)
	}
	if complete, err := PrivateSupervisorMigrationComplete(fixture.stateRoot, fixture.successor.Binding()); err != nil || !complete {
		t.Errorf("historical recovery did not complete the published successor: complete=%t err=%v", complete, err)
	}
}

// PM-W1 compatibility: prepared intent is the private owner's write-ahead
// target authority for interrupted application migration. It conveys neither
// shutdown permission nor strict completion while the queue is Busy.
func TestPrivateBusyIntentRetainsUniqueTargetAuthority(t *testing.T) {
	fixture := newPreparedPrivateObservationPair(t)
	writePreparedPrivateQueue(t, fixture, true)
	if err := migratePreparedPrivateWriter(t, fixture, fixture.successor); !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("migration was not refused as Busy: %v", err)
	}
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		return assertPrivateBusyTargetAuthority(t, ctx, fixture)
	})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	pending, found, err := loadPrivateMigrationRecord(base)
	if err != nil || !found || pending != preparedPrivateObservationRecord(fixture) {
		t.Errorf("Busy did not preserve exact target intent with no PID/shutdown authority: found=%t pending=%+v err=%v", found, pending, err)
	}
	assertPreparedPrivatePublishedLegacy(t, fixture)
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	for _, reader := range preparedPrivateObservationReaders() {
		if binding, err := readPreparedPrivateGeneration(t, fixture, fixture.legacy, reader); err != nil || binding != fixture.legacy.Binding() {
			t.Errorf("Busy intent blocked live Legacy via %s: %v", reader.name, err)
		}
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

func assertPrivateBusyTargetAuthority(t *testing.T, ctx context.Context, fixture preparedPrivateObservationFixture) error {
	t.Helper()
	recorded := fixture.successor.Binding()
	firstProposal := recorded
	firstProposal.DaemonExecutable += "-earlier"
	firstProposal.DaemonSHA256 = task.ComputeSHA256([]byte("earlier prepared daemon"))
	for _, proposal := range []task.SupervisorRef{firstProposal, recorded} {
		matches, err := PrivateMigrationTargetsWithLock(fixture.stateRoot, proposal)
		if err != nil || matches != (proposal == recorded) {
			t.Errorf("private intent lost unique recorded target: recorded=%t matches=%t err=%v", proposal == recorded, matches, err)
		}
	}
	retargetable, err := PrivateMigrationRetargetableWithLock(fixture.stateRoot)
	if err != nil || !retargetable {
		t.Errorf("Busy intent lost pre-stop retarget authority: retargetable=%t err=%v", retargetable, err)
	}
	client, err := PrepareRecordedPrivateMigrationWithLock(ctx, fixture.stateRoot, recorded, fixture.successor.options)
	if err != nil {
		t.Errorf("Busy intent cannot reopen its recorded target: %v", err)
	}
	if client != nil {
		return client.CloseAfterPending(err)
	}
	return nil
}

func TestPrivateSelfAliasWithUnconnectedHistoryStillRequiresQuiescence(t *testing.T) {
	fixture := newPreparedPrivateObservationPair(t)
	persistPreparedPrivateLegacyAlias(t, fixture)
	writePreparedPrivateQueue(t, fixture, true)
	withPreparedPrivateFaultLock(t, fixture, func() {
		appendUnconnectedPrivateTestHistory(t, fixture)
	})
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	if err := migratePreparedPrivateWriter(t, fixture, fixture.legacy); !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
		t.Errorf("unconnected history bypassed self-alias queue authorization: %v", err)
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

func TestPrivateHistoricalMigrationRetriesPartialAliasWithoutQueueAuthorization(t *testing.T) {
	for _, missing := range []string{"config", "record"} {
		t.Run(missing, func(t *testing.T) {
			assertPrivateHistoricalPartialAliasRetirement(t, missing)
		})
	}
}

func assertPrivateHistoricalPartialAliasRetirement(t *testing.T, missing string) {
	t.Helper()
	fixture := newHistoricalPrivateRetirementFixture(t, false, true)
	writePreparedPrivateQueue(t, fixture, true)
	withPreparedPrivateFaultLock(t, fixture, func() {
		path := privateRecoveryConfigPath(fixture.stateRoot)
		if missing == "record" {
			path = privateRecoveryRecordPath(fixture.stateRoot)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	})
	paths := historicalPrivateRetirementRetainedPaths(fixture)
	before := snapshotPrivateTestFiles(t, paths)
	authorizations := 0
	fixture.successor.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
		authorizations++
		return errors.New("historical partial cleanup must not authorize the current queue")
	}
	if err := migratePreparedPrivateWriter(t, fixture, fixture.successor); err != nil || authorizations != 0 {
		t.Errorf("partial historical alias was not retired through completed history: queue authorizations=%d err=%v", authorizations, err)
	}
	assertHistoricalPrivateAliasRetired(t, fixture)
	for _, path := range []string{privateRecoveryRecordPath(fixture.stateRoot), privateRecoveryConfigPath(fixture.stateRoot)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("partial historical artifact remained at %s: %v", filepath.Base(path), err)
		}
	}
	after := snapshotPrivateTestFiles(t, paths)
	for path, want := range before {
		if !bytes.Equal(after[path], want) {
			t.Errorf("partial historical cleanup changed retained %s", filepath.Base(path))
		}
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestPrivateOrphanWithoutHistoryCannotUseQuiescentAuthorization(t *testing.T) {
	for _, consumer := range []string{"generic", "explicit-migration"} {
		t.Run(consumer, func(t *testing.T) {
			fixture := newPreparedPrivateObservationPair(t)
			persistPreparedPrivateLegacyAlias(t, fixture)
			withPreparedPrivateFaultLock(t, fixture, func() {
				if err := os.Remove(privateRecoveryRecordPath(fixture.stateRoot)); err != nil {
					t.Fatal(err)
				}
			})
			before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
			authorizations := 0
			fixture.legacy.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
				authorizations++
				return nil
			}
			err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
				if consumer == "generic" {
					return RetirePrivateRecoveryBindingContext(ctx, fixture.stateRoot, fixture.legacy.Binding())
				}
				return fixture.legacy.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot)
			})
			if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || authorizations != 0 {
				t.Errorf("orphan without history bypassed generic guard: queue authorizations=%d err=%v", authorizations, err)
			}
			assertPreparedPrivateObservationUnchanged(t, fixture, before)
		})
	}
}

func TestPrivateChangedSelfAliasConfigRejectsBeforeQueueAuthorization(t *testing.T) {
	fixture := newPreparedPrivateObservationPair(t)
	writePreparedPrivateQueue(t, fixture, false)
	persistPreparedPrivateLegacyAlias(t, fixture)
	withPreparedPrivateFaultLock(t, fixture, func() {
		faultPreparedPrivateRecoveryMetadata(t, fixture.stateRoot, "alias-digest-mismatch")
	})
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	authorizations := 0
	fixture.legacy.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
		authorizations++
		return nil
	}
	if err := migratePreparedPrivateWriter(t, fixture, fixture.legacy); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || authorizations != 0 {
		t.Errorf("changed current alias config reached queue authorization: queue authorizations=%d err=%v", authorizations, err)
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

func TestPrivateFullSelfAliasKeepsTrivialHistoryConnectionWithActiveQueue(t *testing.T) {
	fixture := newPreparedPrivateObservationPair(t)
	persistPreparedPrivateLegacyAlias(t, fixture)
	writePreparedPrivateQueue(t, fixture, true)
	withPreparedPrivateFaultLock(t, fixture, func() {
		record, found, err := readPrivateRecoveryRecord(fixture.stateRoot)
		if err != nil || !found {
			t.Fatalf("read self-alias record: found=%t err=%v", found, err)
		}
		record.Supervisor = fixture.legacy.Binding()
		writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(fixture.stateRoot), record)
		appendUnconnectedPrivateTestHistory(t, fixture)
		connected, err := privateSupervisorSuccessorConnected(t.Context(), fixture.stateRoot, record.Supervisor, fixture.legacy.Binding())
		if err != nil || !connected {
			t.Fatalf("baseline trivial current history connection changed: connected=%t err=%v", connected, err)
		}
	})
	authorizations := 0
	fixture.legacy.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
		authorizations++
		return errors.New("history-authorized self-alias must not authorize the current queue")
	}
	if err := migratePreparedPrivateWriter(t, fixture, fixture.legacy); err != nil || authorizations != 0 {
		t.Errorf("canonical trivial history connection was tightened: queue authorizations=%d err=%v", authorizations, err)
	}
	assertPreparedPrivateAliasRetired(t, fixture)
	assertPreparedPrivatePublishedLegacy(t, fixture)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func appendUnconnectedPrivateTestHistory(t *testing.T, fixture preparedPrivateObservationFixture) {
	t.Helper()
	older, later := fixture.legacy.Binding(), fixture.successor.Binding()
	older.ConfigDigest = task.ComputeSHA256([]byte("unconnected older config"))
	later.ConfigDigest = task.ComputeSHA256([]byte("unconnected later config"))
	if err := appendPrivateMigrationHistory(filepath.Join(fixture.stateRoot, privateSupervisorDirectory), privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(older), Successor: privateDaemonIdentityForBinding(later),
		LegacyPID: 101, LegacyStopped: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateQuiescentCallbackPreservesBusyErrorWithoutMutation(t *testing.T) {
	fixture := newPreparedPrivateObservationPair(t)
	writePreparedPrivateQueue(t, fixture, false)
	persistPreparedPrivateLegacyAlias(t, fixture)
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	refusal := errors.New("injected application migration refusal")
	authorizations := 0
	fixture.legacy.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
		authorizations++
		return errors.Join(ErrPrivateSupervisorMigrationBusy, refusal)
	}
	err := migratePreparedPrivateWriter(t, fixture, fixture.legacy)
	if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) || !errors.Is(err, refusal) || authorizations != 1 {
		t.Errorf("quiescent callback error lost classification or cause: queue authorizations=%d err=%v", authorizations, err)
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}
