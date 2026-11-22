package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The existing store/alias fixture owns finite authored control commands only.
// Positive process identity and retained Wait are DATA/protocol inputs; no
// native predecessor closure, daemon birth or completed Wait is fabricated.
func TestUpgradeMigrationRetryUsesInstalledRecoveryTransport(t *testing.T) {
	for _, scenario := range []upgradeRetryTransportScenario{
		{name: "acknowledged shutdown"},
		{name: "requested before command Start", requested: true, wantShutdowns: 1},
		{name: "legacy dispatched without acknowledgement", legacyDispatched: true, wantStatuses: 1, wantShutdowns: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) { testUpgradeMigrationRetryTransport(t, scenario) })
	}
}

type upgradeRetryTransportScenario struct {
	name                        string
	requested, legacyDispatched bool
	wantStatuses, wantShutdowns int32
}

func prepareUpgradeRetryTransportFixture(t *testing.T, requested bool) (*upgradeAliasReplayFixture, privateMigrationRecord) {
	t.Helper()
	f := newUpgradeAliasReplayFixture(t, false)
	f.options.ControlOwnerExecutable = os.Args[0]
	f.options.ObservationTimeout = DefaultObservationTimeout
	f.options.PrivateTerminalEventRunner = privateInstalledCallbackForTest(t, f.stored.root)
	f.options.DaemonWaitOwner = upgradeSettlementFixture{owned: true}
	const predecessorPID = 123
	const predecessorStart = "fixture-recorded-predecessor"
	f.options.processObserver = func(pid int) (privateProcessObservation, error) {
		if pid != predecessorPID {
			return privateProcessObservation{}, ErrBinding
		}
		return privateProcessObservation{Present: true, StartToken: predecessorStart}, nil
	}
	writeUpgradeAliasReplayRows(t, f.stored.base, 0)
	if err := os.MkdirAll(filepath.Join(f.stored.base, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateSuccessorTestBytes(t, filepath.Join(f.stored.base, "run", "pueue.pid"), []byte("123\n"))
	var removed atomic.Bool
	f.options.Observer = func(event CommandEvent) {
		f.observeCommand(event)
		if requested && event.Stage == "entry" && slices.Contains(event.Argv, "shutdown") && removed.CompareAndSwap(false, true) {
			if err := os.Remove(f.stored.client.Binding().ClientExecutable); err != nil {
				t.Error(err)
			}
		}
	}
	authorized, err := f.advance(t)
	if err != nil || authorized.Phase != UpgradeAuthorized {
		t.Fatalf("fixture did not authorize its empty predecessor: phase=%s err=%v", authorized.Phase, err)
	}
	selector := func(context.Context) (string, string, error) { return f.currentClient, f.currentDaemon, nil }
	first, firstErr := completeNativeUpgrade(t, f.pin, f.accepted.OperationID, f.options, selector)
	record := assertUpgradeRetryTransportPrepared(t, f, requested, first, firstErr)
	for _, path := range []string{f.stored.client.Binding().ClientExecutable, f.stored.client.Binding().DaemonExecutable} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return f, record
}

func testUpgradeMigrationRetryTransport(t *testing.T, scenario upgradeRetryTransportScenario) {
	t.Helper()
	f, record := prepareUpgradeRetryTransportFixture(t, scenario.requested)
	if scenario.legacyDispatched {
		record = seedLegacyDispatchedAliasFixture(t, f, record)
	}
	// A clean shutdown response may precede closure while the endpoint is ready.
	writePrivateSuccessorTestBytes(t, filepath.Join(f.stored.base, "ready"), nil)
	var statuses, shutdowns atomic.Int32
	f.options.Observer = func(event CommandEvent) {
		f.observeCommand(event)
		if event.Stage != "started" {
			return
		}
		for command, count := range map[string]*atomic.Int32{"status": &statuses, "shutdown": &shutdowns} {
			if slices.Contains(event.Argv, command) {
				count.Add(1)
				if len(event.Argv) < 4 || event.Argv[0] != f.currentClient || event.Argv[1] != "-c" || event.Argv[2] != privateRecoveryConfigPath(f.stored.root) {
					t.Errorf("retry %s did not use the installed client and authenticated alias: %v", command, event.Argv)
				}
			}
		}
	}
	before := upgradeRetryTransportAuthority(t, f)
	observed, retryErr := completeNativeUpgrade(t, f.pin, f.accepted.OperationID, f.options, func(context.Context) (string, string, error) {
		t.Error("accepted migration reselected its target during predecessor observation")
		return "", "", ErrConfiguration
	})
	if !errors.Is(retryErr, ErrPrivateUpgradePending) || errors.Is(retryErr, os.ErrNotExist) || observed.Phase != UpgradeMigrating {
		logUpgradeRetryTransportCause(t, retryErr, f.stored.client.Binding().ClientExecutable)
		t.Errorf("live predecessor retry did not retain safe pending through installed transport: phase=%s err=%v", observed.Phase, retryErr)
	}
	if statuses.Load() != scenario.wantStatuses || shutdowns.Load() != scenario.wantShutdowns {
		t.Errorf("retry changed required protocol work: status=%d want=%d shutdown=%d want=%d", statuses.Load(), scenario.wantStatuses, shutdowns.Load(), scenario.wantShutdowns)
	}
	assertUpgradeRetryTransportFences(t, f, record, before)
}

func seedLegacyDispatchedAliasFixture(t *testing.T, f *upgradeAliasReplayFixture, prepared privateMigrationRecord) privateMigrationRecord {
	t.Helper()
	if !prepared.LegacyShutdownRequested || !prepared.LegacyShutdownDispatched || !prepared.LegacyShutdownAcknowledged || prepared.LegacyStopped {
		t.Fatal("historical producer fixture lacks a clean dispatched predecessor input")
	}
	legacy := prepared
	legacy.LegacyShutdownAcknowledged = false
	data, err := marshalPrivateMigrationRecord(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"legacy_shutdown_acknowledged"`)) {
		t.Fatal("historical producer bytes contain the new acknowledgement field")
	}
	// Establish old-producer input before reconciliation. This fixture byte seed
	// is not a current writer transition: the production writer forbids ACK loss.
	writePrivateSuccessorTestBytes(t, privateMigrationRecordPath(f.stored.base), data)
	loaded, found, err := loadPrivateMigrationRecord(f.stored.base)
	if err != nil || !found || loaded != legacy {
		t.Fatalf("historical dispatched bytes did not retain the exact alias migration: found=%t err=%v", found, err)
	}
	return loaded
}

func assertUpgradeRetryTransportPrepared(t *testing.T, f *upgradeAliasReplayFixture, requested bool, observed PrivateUpgradeObservation, firstErr error) privateMigrationRecord {
	t.Helper()
	if requested {
		if !errors.Is(firstErr, os.ErrNotExist) {
			t.Fatalf("fixture did not fail the qualified shutdown before Start: %v", firstErr)
		}
	} else if !errors.Is(firstErr, ErrPrivateUpgradePending) {
		t.Fatalf("fixture did not retain its modeled predecessor Wait: %v", firstErr)
	}
	record, found, err := loadPrivateMigrationRecord(f.stored.base)
	if err != nil || !found {
		t.Fatalf("first producer did not retain its migration: found=%t err=%v", found, err)
	}
	assertUpgradeRetryTransportPreparedPhase(t, record, requested, observed.Phase)
	alias, present, err := readValidatedPrivateRecoveryAlias(f.stored.root)
	if err != nil || !present || alias.record.Supervisor != f.stored.client.Binding() || !alias.matches(f.stored.client.Binding()) {
		t.Fatalf("first producer did not persist a valid immutable predecessor alias: present=%t err=%v", present, err)
	}
	config := readPrivateSupervisorFile(t, f.stored.client.Binding().ConfigPath)
	if task.ComputeSHA256(config) == f.stored.client.Binding().ConfigDigest || record.UpgradeGeneration.Target.ConfigDigest != task.ComputeSHA256(config) {
		t.Fatal("fixture did not produce the legitimate callback config replacement")
	}
	return record
}

func assertUpgradeRetryTransportPreparedPhase(t *testing.T, record privateMigrationRecord, requested bool, phase UpgradePhase) {
	t.Helper()
	if !record.LegacyShutdownRequested || record.LegacyShutdownDispatched == requested || record.LegacyShutdownAcknowledged == requested || record.LegacyStopped || record.UpgradeGeneration.AcceptedStart || phase != UpgradeMigrating {
		t.Fatalf("first producer did not retain its exact native fence: record=%+v phase=%s", record, phase)
	}
}

func upgradeRetryTransportAuthority(t *testing.T, f *upgradeAliasReplayFixture) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, path := range []string{f.stored.client.Binding().ConfigPath, privateBootstrapRecordPath(f.stored.root), privateSupervisorBindingPath(f.stored.root), filepath.Join(f.stored.base, "daemon.identity.json"), privateRecoveryRecordPath(f.stored.root), privateRecoveryConfigPath(f.stored.root)} {
		files[path] = readPrivateSupervisorFile(t, path)
	}
	return files
}

func assertUpgradeRetryTransportFences(t *testing.T, f *upgradeAliasReplayFixture, before privateMigrationRecord, authority map[string][]byte) {
	t.Helper()
	after, found, err := loadPrivateMigrationRecord(f.stored.base)
	if err != nil || !found {
		t.Fatalf("retry lost its canonical migration: found=%t err=%v", found, err)
	}
	if !after.LegacyShutdownAcknowledged {
		t.Error("retry did not retain its clean shutdown acknowledgement")
	}
	assertUpgradeRetryTransportJournalUnclosed(t, before, after)
	if f.starts.Load() != 0 {
		t.Fatal("unclosed predecessor permitted daemon Start")
	}
	for path, data := range authority {
		if !bytes.Equal(data, readPrivateSupervisorFile(t, path)) {
			t.Fatalf("retry replaced canonical binding, callback config or recovery alias: %s", filepath.Base(path))
		}
	}
	assertPrivateTestPathMissing(t, privateMigrationHistoryPath(f.stored.base))
	assertPrivateTestPathMissing(t, filepath.Join(f.stored.base, "daemon-started"))
}

func assertUpgradeRetryTransportJournalUnclosed(t *testing.T, before, after privateMigrationRecord) {
	t.Helper()
	if after.LegacyStopped || after.UpgradeGeneration.AcceptedStart || after.UpgradeGeneration.SuccessorPID != 0 || !after.LegacyShutdownRequested || before.LegacyShutdownDispatched && !after.LegacyShutdownDispatched {
		t.Error("live predecessor retry invented native closure or successor authority")
	}
	if after.Legacy != before.Legacy || after.Successor != before.Successor || after.LegacyPID != before.LegacyPID || after.LegacyProcessStart != before.LegacyProcessStart || after.UpgradeGeneration != before.UpgradeGeneration {
		t.Error("shutdown retry changed immutable predecessor, target or upgrade ownership")
	}
}

func logUpgradeRetryTransportCause(t *testing.T, cause error, removedClient string) {
	t.Helper()
	var pathErr *os.PathError
	if errors.As(cause, &pathErr) {
		t.Logf("retry cause type=%T op=%s removed_legacy_client=%t ENOENT=%t configuration=%t binding=%t migration_required=%t", pathErr, pathErr.Op, pathErr.Path == removedClient, errors.Is(pathErr, os.ErrNotExist), errors.Is(cause, ErrConfiguration), errors.Is(cause, ErrBinding), errors.Is(cause, ErrPrivateSupervisorMigrationRequired))
	}
}

// Refusal is only endpoint unavailability; the still-present process and its
// retained Wait prevent closure even after the daemon stops listening.
func TestUpgradeDispatchedRetryWaitsForUnavailableLivePredecessor(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "refused"}[refused], func(t *testing.T) {
			f, pending := upgradeUnclosedMigrationFixture(t, true)
			f.client.options.DaemonWaitOwner = upgradeSettlementFixture{owned: true}
			f.client.options.processObserver = func(int) (privateProcessObservation, error) {
				return privateProcessObservation{Present: true, StartToken: pending.LegacyProcessStart}, nil
			}
			if refused {
				listenRefusedPrivateTestSocket(t, strings.TrimPrefix(f.client.Binding().Endpoint, "unix:"))
			}
			before := snapshotExecutionTargetTree(t, f.base)
			err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
				observed, err := observeDispatchedPrivateMigrationShutdown(ctx, f.base, f.client.Binding(), pending, f.client.options)
				if observed != pending {
					t.Error("unavailable live predecessor changed shutdown authority")
				}
				return err
			})
			if !errors.Is(err, ErrPrivateUpgradePending) || errors.Is(err, ErrPrivateEndpointRefused) || errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
				t.Fatalf("unavailable live predecessor did not reach conservative Wait: %v", err)
			}
			assertExecutionTargetTreeUnchanged(t, f.base, before)
		})
	}
}

func TestDispatchedCompactShutdownUsesConfigAliasWithoutExecutables(t *testing.T) {
	f := newCompactRefusedPrivateEndpointFixture(t)
	saved := f.client.Binding()
	if err := f.client.PersistPrivateRecoveryBindingContext(t.Context(), f.stateRoot); err != nil {
		t.Fatal(err)
	}
	// The alias owner saved the exact config before a valid primary rewrite.
	data := append(readPrivateSupervisorFile(t, saved.ConfigPath), []byte("\n# primary configuration replaced\n")...)
	writePrivateSuccessorTestBytes(t, saved.ConfigPath, data)
	removeUpgradeAliasFixtureFile(t, saved.ClientExecutable)
	removeUpgradeAliasFixtureFile(t, saved.DaemonExecutable)
	base := filepath.Dir(saved.ConfigPath)
	target := saved
	target.ConfigDigest = task.ComputeSHA256(data)
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(saved), Successor: privateDaemonIdentityForBinding(target), LegacyPID: 123, LegacyProcessStart: "absent-predecessor", LegacyShutdownRequested: true, LegacyShutdownDispatched: true}
	if err := publishPrivateMigrationStateContext(t.Context(), base, pending, f.client.options.PrivateGenerationGuard); err != nil {
		t.Fatal(err)
	}
	options := f.client.options
	options.processObserver = func(int) (privateProcessObservation, error) { return privateProcessObservation{}, nil }
	options.Observer = func(event CommandEvent) { t.Errorf("native closure executed a CLI: %v", event.Argv) }
	err := WithExistingBootstrapLockContext(t.Context(), f.stateRoot, func(ctx context.Context) error {
		observed, err := observeDispatchedPrivateMigrationShutdown(ctx, base, saved, pending, options)
		if err == nil && !observed.LegacyStopped {
			t.Error("absent predecessor did not record closure")
		}
		return err
	})
	if err != nil {
		t.Fatalf("config-authenticated compact closure required primary config or executable: %v", err)
	}
	f.assertSocketRemoved(t)
}
