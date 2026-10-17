package control

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PM-O1: a busy-refused migration retains the published legacy generation,
// prepared journal and matching recovery alias. Readers hold the SH generation
// lease and observe that exact live generation without changing durable state.
// PM-O2: shutdown progress or foreign transition metadata blocks observation.
// PM-C1: observation never establishes migration completion or cleanup authority.
type preparedPrivateObservationFixture struct {
	stateRoot string
	legacy    *Client
	successor *Client
}

type preparedPrivateObservationReader struct {
	name string
	read func(context.Context, string, *Client) (task.SupervisorRef, error)
}

func preparedPrivateObservationReaders() []preparedPrivateObservationReader {
	return []preparedPrivateObservationReader{
		{name: "shared-probe", read: observeProbePreparedPrivateGeneration},
		{name: "current-resolver", read: observeResolvedPreparedPrivateGeneration},
	}
}

func observeProbePreparedPrivateGeneration(ctx context.Context, root string, client *Client) (binding task.SupervisorRef, resultErr error) {
	published, ready, err := ProbePrivateWithGate(ctx, client.Binding().ClientExecutable, client.Binding().DaemonExecutable, root, client.options, nil)
	if published != nil {
		defer func() { resultErr = errors.Join(resultErr, published.CloseAfterPending(resultErr)) }()
	}
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if !ready || published == nil {
		return task.SupervisorRef{}, errors.New("published generation was not observable")
	}
	return published.Binding(), nil
}

func observeResolvedPreparedPrivateGeneration(ctx context.Context, root string, client *Client) (task.SupervisorRef, error) {
	published, found, err := ResolvePrivateSupervisorSuccessorWithLock(ctx, root, client.Binding())
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if !found {
		return task.SupervisorRef{}, errors.New("published generation did not resolve")
	}
	return published, nil
}

func TestPublishedPrivateObservationAcceptsCompletedLegacy(t *testing.T) {
	fixture := newPreparedPrivateObservationPair(t)
	for _, reader := range preparedPrivateObservationReaders() {
		t.Run(reader.name, func(t *testing.T) {
			before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
			binding, err := readPreparedPrivateGeneration(t, fixture, fixture.legacy, reader)
			if err != nil || binding != fixture.legacy.Binding() {
				t.Errorf("completed published legacy observation: binding=%+v err=%v", binding, err)
			}
			assertPreparedPrivateObservationUnchanged(t, fixture, before)
		})
	}
}

func TestPreparedPrivateMigrationAllowsPublishedLegacyObservationAfterBusy(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	// Reopen through BindPrivate as a later status command does. BindPrivate's
	// exclusive setup finishes before either reader acquires its shared lease.
	legacy := fixture.legacy.Binding()
	readerClient := bindPreparedPrivateObservation(t, fixture.stateRoot, legacy.ClientExecutable, legacy.DaemonExecutable, fixture.legacy.options)
	if readerClient.Binding() != legacy {
		t.Fatal("BindPrivate did not reopen the exact published legacy generation")
	}
	for _, reader := range preparedPrivateObservationReaders() {
		t.Run(reader.name, func(t *testing.T) {
			before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
			binding, err := readPreparedPrivateGeneration(t, fixture, readerClient, reader)
			if err != nil {
				t.Errorf("live published legacy observation blocked after busy refusal: %v", err)
			} else if binding != legacy {
				t.Errorf("observation selected another generation: got=%+v want=%+v", binding, legacy)
			}
			assertPreparedPrivateObservationIncomplete(t, fixture)
			assertPreparedPrivateObservationUnchanged(t, fixture, before)
			assertBusyPreparedPrivateObservation(t, fixture)
		})
	}
}

func TestPreparedPrivateMigrationRejectsUnsafePublishedObservation(t *testing.T) {
	for _, fault := range []string{
		"shutdown-requested", "shutdown-dispatched", "legacy-stopped",
		"successor-binding", "unrelated-binding", "foreign-journal",
		"foreign-recovery", "corrupt-recovery-alias", "bootstrap-pending",
		"foreign-publication", "corrupt-publication", "foreign-daemon",
		"restart-pending", "alias-digest-mismatch",
	} {
		t.Run(fault, func(t *testing.T) {
			fixture := newBusyPreparedPrivateObservation(t)
			candidate := faultPreparedPrivateObservation(t, fixture, fault)
			before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
			for _, reader := range preparedPrivateObservationReaders() {
				t.Run(reader.name, func(t *testing.T) {
					if binding, err := readPreparedPrivateGeneration(t, fixture, candidate, reader); err == nil {
						t.Errorf("unsafe prepared generation was observable: %+v", binding)
					}
					assertPreparedPrivateObservationIncomplete(t, fixture)
					assertPreparedPrivateObservationUnchanged(t, fixture, before)
				})
			}
		})
	}
}

func TestPreparedPrivateMigrationDoesNotAuthorizeTerminalMaintenance(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	run := newPrivateQueueMaintenanceRun(fixture.legacy, false)
	err := WithSupervisorLeaseContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		assertPreparedPrivateObservationIncomplete(t, fixture)
		return run.maintainLocked(ctx)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Errorf("prepared journal granted terminal maintenance authority: %v", err)
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

func readPreparedPrivateGeneration(t *testing.T, fixture preparedPrivateObservationFixture, candidate *Client, reader preparedPrivateObservationReader) (task.SupervisorRef, error) {
	t.Helper()
	var binding task.SupervisorRef
	err := WithSupervisorLeaseContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		var readErr error
		binding, readErr = reader.read(ctx, fixture.stateRoot, candidate)
		return readErr
	})
	return binding, err
}

func newPreparedPrivateObservationPair(t *testing.T) preparedPrivateObservationFixture {
	t.Helper()
	private := newPrivateSupervisorTestFixture(t)
	options := privateMigrationTestOptions(private.stateRoot, privateTestObservationTimeout)
	legacy := bindPreparedPrivateObservation(t, private.stateRoot, private.clientPath, private.daemonPath, options)
	successorPath := filepath.Join(filepath.Dir(private.daemonPath), "successor-pueued")
	writeExecutable(t, successorPath, privateDaemonFixture+"\n# prepared successor\n")
	successor := bindPreparedPrivateObservation(t, private.stateRoot, private.clientPath, successorPath, options)
	return preparedPrivateObservationFixture{stateRoot: private.stateRoot, legacy: legacy, successor: successor}
}

func bindPreparedPrivateObservation(t *testing.T, root, clientPath, daemonPath string, options Options) *Client {
	t.Helper()
	client, err := BindPrivate(t.Context(), clientPath, daemonPath, root, options)
	if err != nil {
		t.Fatalf("bind private observation fixture: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client
}

func newBusyPreparedPrivateObservation(t *testing.T) preparedPrivateObservationFixture {
	t.Helper()
	fixture := newPreparedPrivateObservationPair(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	writePrivateSuccessorTestBytes(t, filepath.Join(base, "test-status.json"), []byte(preparedPrivateObservationStatus))
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		return fixture.successor.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("fixture did not refuse active migration: %v", err)
	}
	assertBusyPreparedPrivateObservation(t, fixture)
	if readyErr := fixture.legacy.Ready(t.Context()); readyErr != nil {
		t.Fatalf("busy refusal left the legacy endpoint unavailable: %v", readyErr)
	}
	return fixture
}

func preparedPrivateObservationRecord(fixture preparedPrivateObservationFixture) privateMigrationRecord {
	return privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(fixture.legacy.Binding()),
		Successor:     privateDaemonIdentityForBinding(fixture.successor.Binding()),
	}
}

func assertBusyPreparedPrivateObservation(t *testing.T, fixture preparedPrivateObservationFixture) {
	t.Helper()
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	journal, found, err := loadPrivateMigrationRecord(base)
	want := preparedPrivateObservationRecord(fixture)
	if err != nil || !found || journal != want {
		t.Fatalf("busy refusal did not retain the exact pre-stop journal: found=%t journal=%+v err=%v", found, journal, err)
	}
	if !privateMigrationMayRetarget(journal) {
		t.Fatal("prepared journal crossed the canonical shutdown boundary")
	}
	recovery, found, err := ReadPrivateRecoveryBinding(fixture.stateRoot)
	if err != nil || !found || !task.SameSupervisorIdentity(recovery, fixture.legacy.Binding()) {
		t.Fatalf("busy refusal did not retain the matching recovery alias: found=%t recovery=%+v err=%v", found, recovery, err)
	}
	matching, err := PrivateRecoveryBindingMatches(fixture.stateRoot, fixture.legacy.Binding())
	if err != nil || !matching {
		t.Fatalf("busy refusal recovery alias does not match legacy: matching=%t err=%v", matching, err)
	}
	assertPreparedPrivatePublishedLegacy(t, fixture)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	assertPreparedPrivateObservationIncomplete(t, fixture)
}

func assertPreparedPrivatePublishedLegacy(t *testing.T, fixture preparedPrivateObservationFixture) {
	t.Helper()
	published, found, err := ReadPrivateSupervisorBinding(fixture.stateRoot)
	if err != nil || !found || published != fixture.legacy.Binding() {
		t.Fatalf("busy refusal replaced the published legacy binding: found=%t published=%+v err=%v", found, published, err)
	}
	identity, found, err := loadPrivateDaemonIdentity(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))
	if err != nil || !found || identity != privateDaemonIdentityForBinding(fixture.legacy.Binding()) {
		t.Fatalf("busy refusal replaced legacy daemon identity: found=%t identity=%+v err=%v", found, identity, err)
	}
}

func assertPreparedPrivateObservationIncomplete(t *testing.T, fixture preparedPrivateObservationFixture) {
	t.Helper()
	complete, err := PrivateSupervisorMigrationComplete(fixture.stateRoot, fixture.legacy.Binding())
	if err != nil || complete {
		t.Errorf("prepared or conflicting transition became complete: complete=%t err=%v", complete, err)
	}
}

func assertPreparedPrivateObservationUnchanged(t *testing.T, fixture preparedPrivateObservationFixture, before map[string]privateSuccessorTestFile) {
	t.Helper()
	after := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	if !maps.Equal(before, after) {
		for path, expected := range before {
			if observed, found := after[path]; !found || observed != expected {
				t.Errorf("shared-lease observation changed state-root artifact %s", strings.TrimPrefix(path, fixture.stateRoot))
			}
		}
		for path := range after {
			if _, found := before[path]; !found {
				t.Errorf("shared-lease observation created state-root artifact %s", strings.TrimPrefix(path, fixture.stateRoot))
			}
		}
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func faultPreparedPrivateObservation(t *testing.T, fixture preparedPrivateObservationFixture, fault string) *Client {
	t.Helper()
	root := fixture.stateRoot
	switch fault {
	case "shutdown-requested", "shutdown-dispatched", "legacy-stopped":
		withPreparedPrivateFaultLock(t, fixture, func() { setPreparedPrivateShutdownFlag(t, fixture, fault) })
	case "successor-binding":
		publishPreparedPrivateTestBinding(t, fixture, fixture.successor)
		return fixture.successor
	case "unrelated-binding":
		path := filepath.Join(filepath.Dir(root), "unrelated-pueued")
		writeExecutable(t, path, privateDaemonFixture+"\n# unrelated pair\n")
		unrelated := bindPreparedPrivateObservation(t, root, fixture.legacy.Binding().ClientExecutable, path, fixture.legacy.options)
		publishPreparedPrivateTestBinding(t, fixture, unrelated)
		return unrelated
	default:
		withPreparedPrivateFaultLock(t, fixture, func() { faultPreparedPrivateTransitionMetadata(t, fixture, fault) })
	}
	return fixture.legacy
}

func faultPreparedPrivateTransitionMetadata(t *testing.T, fixture preparedPrivateObservationFixture, fault string) {
	t.Helper()
	root := fixture.stateRoot
	base := filepath.Join(root, privateSupervisorDirectory)
	switch fault {
	case "foreign-journal":
		record, found, err := loadPrivateMigrationRecord(base)
		if err != nil || !found {
			t.Fatalf("read prepared journal: found=%t err=%v", found, err)
		}
		foreign := record
		foreign.Legacy.SHA256 = strings.Repeat("e", 64)
		if err = replacePrivateMigrationRecord(base, record, foreign); err != nil {
			t.Fatal(err)
		}
	case "foreign-recovery", "corrupt-recovery-alias", "alias-digest-mismatch":
		faultPreparedPrivateRecoveryMetadata(t, root, fault)
	case "restart-pending":
		target := fixture.legacy.Binding()
		target.DaemonExecutable += "-relocated"
		if err := publishPrivatePairRestartIntent(root, privatePairRestartIntent{
			Predecessor: fixture.legacy.Binding(), Target: target,
		}); err != nil {
			t.Fatal(err)
		}
	case "bootstrap-pending":
		if err := markPrivateBootstrapPending(root); err != nil {
			t.Fatal(err)
		}
	case "foreign-publication":
		writePrivateSuccessorTestRecord(t, privateIdentityPublicationPath(root), privateIdentityPublicationRecord{
			SchemaVersion: task.SchemaVersion, Binding: fixture.successor.Binding(),
		})
	case "corrupt-publication":
		writePrivateSuccessorTestBytes(t, privateIdentityPublicationPath(root), []byte("{"))
	case "foreign-daemon":
		writePrivateSuccessorTestRecord(t, privateDaemonIdentityPath(base), privateDaemonIdentityForBinding(fixture.successor.Binding()))
	default:
		t.Fatalf("unknown observation fault %q", fault)
	}
}

func faultPreparedPrivateRecoveryMetadata(t *testing.T, root, fault string) {
	t.Helper()
	switch fault {
	case "foreign-recovery":
		record, found, err := readPrivateRecoveryBinding(root)
		if err != nil || !found {
			t.Fatalf("read recovery record: found=%t err=%v", found, err)
		}
		// Recovery aliases match logical identity, not daemon provenance.
		record.Supervisor.ConfigDigest = strings.Repeat("e", 64)
		writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(root), record)
	case "corrupt-recovery-alias":
		data := readPrivateSupervisorFile(t, privateRecoveryConfigPath(root))
		writePrivateSuccessorTestBytes(t, privateRecoveryConfigPath(root), append(data, []byte("\n# changed recovery config\n")...))
	case "alias-digest-mismatch":
		record, found, err := readPrivateRecoveryBinding(root)
		if err != nil || !found {
			t.Fatalf("read recovery record: found=%t err=%v", found, err)
		}
		data := append(readPrivateSupervisorFile(t, record.ConfigPath), []byte("\n# valid changed alias\n")...)
		record.ConfigSHA256 = task.ComputeSHA256(data)
		writePrivateSuccessorTestBytes(t, record.ConfigPath, data)
		writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(root), record)
	default:
		t.Fatalf("unknown recovery fault %q", fault)
	}
}

func publishPreparedPrivateTestBinding(t *testing.T, fixture preparedPrivateObservationFixture, candidate *Client) {
	t.Helper()
	withPreparedPrivateFaultLock(t, fixture, func() {
		if err := publishPrivateDaemonIdentity(filepath.Join(fixture.stateRoot, privateSupervisorDirectory), candidate.Binding()); err != nil {
			t.Fatal(err)
		}
	})
}

func withPreparedPrivateFaultLock(t *testing.T, fixture preparedPrivateObservationFixture, mutate func()) {
	t.Helper()
	if err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(context.Context) error {
		mutate()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func setPreparedPrivateShutdownFlag(t *testing.T, fixture preparedPrivateObservationFixture, flag string) {
	t.Helper()
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	record, found, err := loadPrivateMigrationRecord(base)
	if err != nil || !found {
		t.Fatalf("read prepared journal: found=%t err=%v", found, err)
	}
	updated := record
	updated.LegacyPID = 123
	updated.LegacyProcessStart = "fixture:123"
	switch flag {
	case "shutdown-requested":
		updated.LegacyShutdownRequested = true
	case "shutdown-dispatched":
		updated.LegacyShutdownDispatched = true
	case "legacy-stopped":
		updated.LegacyStopped = true
	default:
		t.Fatalf("unknown shutdown flag %q", flag)
	}
	if err = replacePrivateMigrationRecord(base, record, updated); err != nil {
		t.Fatal(err)
	}
	if privateMigrationMayRetarget(updated) {
		t.Fatal("canonical migration owner allowed retargeting after a shutdown flag")
	}
	matches, err := PrivateMigrationTargetsWithLock(fixture.stateRoot, fixture.successor.Binding())
	if err != nil || !matches {
		t.Fatalf("shutdown progress lost its fixed successor: matches=%t err=%v", matches, err)
	}
}

const preparedPrivateObservationStatus = `{
  "tasks": {"7": {
    "id": 7, "created_at": "2026-09-13T20:00:00.123456789Z",
    "original_command": "runner --root /tmp/task", "command": "runner --root /tmp/task",
    "path": "/tmp", "envs": {}, "group": "default", "dependencies": [], "priority": 0,
    "label": "delegate:active", "status": {"Running": {
      "enqueued_at": "2026-09-13T20:00:00.123456789Z", "start": "2026-09-13T20:00:00.123456789Z"
    }}
  }},
  "groups": {"default": {"status": "Running", "parallel_tasks": 0}}
}`
