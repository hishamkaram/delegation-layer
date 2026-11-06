package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProbeSavedPrivateCompatibleHealthyAliasRemainsExact(t *testing.T) {
	f := newPreparedPrivateObservationPair(t)
	persistPreparedPrivateLegacyAlias(t, f)
	before := snapshotSavedProbeTestState(t, f.stateRoot)
	called := 0
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.legacy.Binding(), f.legacy.options, func(context.Context) (string, string, error) { called++; return "", "", errors.New("unused lookup") })
	closeSavedProbeTestClient(t, client, err)
	if err != nil || client == nil || called != 0 || client.RuntimeBinding() != f.legacy.Binding() {
		t.Fatalf("healthy exact alias changed: client=%t calls=%d err=%v", client != nil, called, err)
	}
	assertSavedProbeTestUnchanged(t, f.stateRoot, before)
	assertPrivateDaemonStartCount(t, f.stateRoot, 1)
}

func TestProbeSavedPrivateCompatibleNeverUsesDThroughIncompleteGeneration(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "alias", true: "migration"}[pending], func(t *testing.T) {
			f := newPreparedPrivateObservationPair(t)
			if pending {
				writePreparedPrivateQueue(t, f, true)
				err := migratePreparedPrivateWriter(t, f, f.successor)
				if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
					t.Fatal(err)
				}
			} else {
				persistPreparedPrivateLegacyAlias(t, f)
			}
			binding := f.legacy.Binding()
			completed := completedPrivateSuccessorTestFixture{clientPath: binding.ClientExecutable, daemonPath: binding.DaemonExecutable}
			damageCompatibleProbePair(t, completed, "missing client")
			before := snapshotSavedProbeTestState(t, f.stateRoot)
			called := 0
			client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, binding, f.legacy.options, func(context.Context) (string, string, error) { called++; return "", "", nil })
			closeSavedProbeTestClient(t, client, err)
			if client != nil || err != nil || called != 0 {
				t.Fatalf("incomplete generation selected D: calls=%d err=%v", called, err)
			}
			assertSavedProbeTestUnchanged(t, f.stateRoot, before)
			assertPrivateDaemonStartCount(t, f.stateRoot, 1)
		})
	}
}

func TestProbeSavedPrivateCompatiblePairFaultIsNotUnavailable(t *testing.T) {
	for _, fault := range []string{"stat loop", "directory", "config"} {
		t.Run(fault, func(t *testing.T) {
			f := newCompletedPrivateSuccessorTestFixture(t, true)
			prepareCompatibleProbeOperationalFault(t, f, fault)
			called := 0
			client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) { called++; return "", "", nil })
			closeSavedProbeTestClient(t, client, err)
			if err == nil || client != nil || called != 0 {
				t.Fatalf("operational fault became D fallback: calls=%d err=%v", called, err)
			}
		})
	}
}

func prepareCompatibleProbeOperationalFault(t *testing.T, f completedPrivateSuccessorTestFixture, fault string) {
	t.Helper()
	if fault == "config" {
		path := filepath.Join(f.stateRoot, privateSupervisorDirectory, "pueue.yml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { writePrivateSuccessorTestBytes(t, path, data) })
		writePrivateSuccessorTestBytes(t, path, []byte("not: [yaml"))
		return
	}
	damageCompatibleProbePair(t, f, "missing client")
	if fault == "stat loop" {
		if err := os.Symlink(f.clientPath, f.clientPath); err != nil {
			t.Fatal(err)
		}
	} else {
		makePrivateTestDirectory(t, f.clientPath)
	}
	t.Cleanup(func() { removePrivateTestPath(t, f.clientPath) })
}

func TestProbeSavedPrivateTaskObservationReadsEquivalentAliasAfterPrimaryRetarget(t *testing.T) {
	for _, edit := range []struct {
		name  string
		alias func(preparedPrivateObservationFixture) func([]byte) ([]byte, error)
	}{
		{name: "reserialized", alias: reserializedPreparedAlias},
		{name: "comment-only", alias: func(preparedPrivateObservationFixture) func([]byte) ([]byte, error) { return commentedPreparedAlias }},
	} {
		t.Run(edit.name, func(t *testing.T) {
			f, identity, _ := newDriftedCompatibleAliasProbe(t)
			rewritePreparedRecoveryAlias(t, f, edit.alias(f))
			before := snapshotSavedProbeTestState(t, f.stateRoot)
			called := 0
			client, probeErr := ProbeSavedPrivateTaskObservation(t.Context(), f.stateRoot, f.legacy.Binding(), f.legacy.options, func(context.Context) (string, string, error) {
				called++
				return "", "", errors.New("same-generation alias must not select another installation")
			})
			t.Cleanup(func() { closeSavedProbeTestClient(t, client, probeErr) })
			if probeErr != nil || client == nil || called != 0 {
				t.Fatalf("validated equivalent alias was not ready after primary retarget: client=%t selector=%d err=%v", client != nil, called, probeErr)
			}
			if client.Binding() != f.legacy.Binding() || client.RuntimeBinding().ConfigPath != privateRecoveryConfigPath(f.stateRoot) || client.RuntimeBinding().Endpoint != f.legacy.Binding().Endpoint || client.RuntimeBinding().ResolvedConfigSHA256 != f.legacy.Binding().ResolvedConfigSHA256 {
				t.Fatalf("alias probe changed the saved logical binding or transport: saved=%+v runtime=%+v", client.Binding(), client.RuntimeBinding())
			}
			observation, err := client.Reconcile(t.Context(), identity)
			if err != nil || !observation.Matched || observation.NumericTaskID != 42 || observation.State != StateQueued {
				t.Fatalf("validated alias lost its queued saved task: observation=%+v err=%v", observation, err)
			}
			assertDriftedCompatibleAliasStaysReadOnly(t, f, client)
			assertSavedProbeTestUnchanged(t, f.stateRoot, before)
			assertPrivateDaemonStartCount(t, f.stateRoot, 1)
		})
	}
}

// Compose the existing finite daemon, actual Wait owner, canonical migration
// and alias writers. Only the primary endpoint changes; the saved alias keeps
// the old effective config and the migration stops at its real busy authorizer.
func newDriftedCompatibleAliasProbe(t *testing.T) (preparedPrivateObservationFixture, Identity, func()) {
	t.Helper()
	private := newPrivateSupervisorTestFixture(t)
	options := privateMigrationTestOptions(private.stateRoot, privateTestObservationTimeout)
	waitOwner, join := ownPrivateRecoveryDaemonWaits(t, private.stateRoot)
	options.DaemonWaitOwner = waitOwner
	options.WaitForPendingClose = true
	legacy := bindPrivateSetupForTest(t, private.stateRoot, private.clientPath, private.daemonPath, options)
	ownPrivateMigrationTestClient(t, legacy)
	configPath := legacy.Binding().ConfigPath
	err := WithBootstrapLockContext(t.Context(), private.stateRoot, func(ctx context.Context) error {
		if err := legacy.PersistPrivateRecoveryBindingContext(ctx, private.stateRoot); err != nil {
			return err
		}
		data := readPrivateSupervisorFile(t, configPath)
		socket := strings.TrimPrefix(legacy.Binding().Endpoint, "unix:")
		changed, err := configWithSocketPath(data, filepath.Join(filepath.Dir(socket), "n"))
		if err != nil {
			return err
		}
		return os.WriteFile(configPath, changed, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(filepath.Dir(private.daemonPath), "alias-probe-successor-pueued")
	writeExecutable(t, daemon, privateDaemonFixture)
	successor := preparePrivateCandidateForTest(t, private.stateRoot, private.clientPath, daemon, legacy.options)
	f := preparedPrivateObservationFixture{stateRoot: private.stateRoot, legacy: legacy, successor: successor}
	identity := writeCompletedPrivateRemovalTask(t, private.stateRoot)
	writeStatusFor(t, &fakeSupervisor{statusPath: filepath.Join(private.stateRoot, privateSupervisorDirectory, "test-status.json")}, identity.RootID, identity.TaskID, 42, StateQueued)
	if err = migratePreparedPrivateWriter(t, f, successor); !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("fixture did not stop the canonical retargeted migration before shutdown: %v", err)
	}
	assertBusyPreparedPrivateObservation(t, f)
	return f, identity, join
}

func assertDriftedCompatibleAliasStaysReadOnly(t *testing.T, f preparedPrivateObservationFixture, client *Client) {
	t.Helper()
	binding := f.legacy.Binding()
	called := 0
	general, generalErr := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, binding, f.legacy.options, func(context.Context) (string, string, error) {
		called++
		return "", "", errors.New("general probe must not select through an incomplete alias")
	})
	closeSavedProbeTestClient(t, general, generalErr)
	if general != nil || generalErr != nil || called != 0 {
		t.Fatalf("task observation relaxed general readiness: client=%t selector=%d err=%v", general != nil, called, generalErr)
	}
	for _, reader := range preparedPrivateObservationReaders() {
		if observed, readErr := readPreparedPrivateGeneration(t, f, f.legacy, reader); readErr == nil {
			t.Fatalf("strict %s reader accepted a reserialized alias: %+v", reader.name, observed)
		}
	}
	err := WithBootstrapLockContext(t.Context(), f.stateRoot, func(ctx context.Context) error {
		_, mutationErr := client.SetPrivateDefaultParallelTasksWithBootstrapLock(ctx, f.stateRoot, 0)
		return mutationErr
	})
	if !errors.Is(err, ErrPrivateSupervisorTransitionIncomplete) {
		t.Fatalf("alias read gained default-group mutation authority: %v", err)
	}
}

func TestProbeSavedPrivateTaskObservationOlderBindingCannotUseEquivalentSuccessorAlias(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	primaryBefore := readPrivateSupervisorFile(t, f.successor.ConfigPath)
	published, err := BindSaved(t.Context(), f.successor, f.options)
	if err != nil {
		closeSavedProbeTestClient(t, published, err)
		t.Fatal(err)
	}
	ownPrivateMigrationTestClient(t, published)
	if err = WithBootstrapLockContext(t.Context(), f.stateRoot, func(ctx context.Context) error {
		return published.PersistPrivateRecoveryBindingContext(ctx, f.stateRoot)
	}); err != nil {
		t.Fatal(err)
	}
	alias := preparedPrivateObservationFixture{stateRoot: f.stateRoot, legacy: published}
	rewritePreparedRecoveryAlias(t, alias, reserializedPreparedAlias(alias))
	if !slices.Equal(primaryBefore, readPrivateSupervisorFile(t, f.successor.ConfigPath)) {
		t.Fatal("alias fixture changed the completed successor's primary config")
	}
	before := snapshotSavedProbeTestState(t, f.stateRoot)
	called := 0
	client, err := ProbeSavedPrivateTaskObservation(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) {
		called++
		return "", "", errors.New("older binding must not select through successor alias equivalence")
	})
	closeSavedProbeTestClient(t, client, err)
	if client != nil || err != nil || called != 0 {
		t.Fatalf("older saved binding gained successor alias observation or a hard refusal: client=%t selector=%d err=%v", client != nil, called, err)
	}
	assertSavedProbeTestUnchanged(t, f.stateRoot, before)
	assertPrivateDaemonStartCount(t, f.stateRoot, 2)
}

func TestProbeSavedPrivateTaskObservationAliasSnapshotHoldsSharedLease(t *testing.T) {
	f, _, _ := newDriftedCompatibleAliasProbe(t)
	rewritePreparedRecoveryAlias(t, f, commentedPreparedAlias)
	before := snapshotSavedProbeTestState(t, f.stateRoot)
	lockPath := filepath.Join(f.stateRoot, privateSupervisorDirectory, "bootstrap.lock")
	var snapshots atomic.Int32
	options := f.legacy.options
	options.Observer = func(event CommandEvent) {
		if event.Stage != "entry" || !slices.Contains(event.Argv, "status") || !slices.Contains(event.Argv, privateRecoveryConfigPath(f.stateRoot)) {
			return
		}
		// Probe the actual lock during the alias snapshot: EX must fail while
		// another SH succeeds. The helper releases every acquired native FD.
		state, err := privateLockStateOf(t.Context(), lockPath)
		if err != nil || state != privateLockShared {
			t.Errorf("alias snapshot escaped its shared generation owner: state=%s err=%v", state, err)
		}
		snapshots.Add(1)
	}
	client, err := ProbeSavedPrivateTaskObservation(t.Context(), f.stateRoot, f.legacy.Binding(), options, func(context.Context) (string, string, error) {
		return "", "", errors.New("same-generation alias snapshot must not select another pair")
	})
	closeSavedProbeTestClient(t, client, err)
	if err != nil || client == nil || snapshots.Load() != 1 {
		t.Fatalf("probe did not observe the alias under its actual snapshot lease: client=%t snapshots=%d err=%v", client != nil, snapshots.Load(), err)
	}
	if state, lockErr := privateLockStateOf(t.Context(), lockPath); lockErr != nil || state != privateLockFree {
		t.Fatalf("alias snapshot retained its generation lease after return: state=%s err=%v", state, lockErr)
	}
	assertSavedProbeTestUnchanged(t, f.stateRoot, before)
	assertPrivateDaemonStartCount(t, f.stateRoot, 1)
}

func TestProbeSavedPrivateTaskObservationRefusesUnsafeDriftedAlias(t *testing.T) {
	for _, fault := range []string{"shutdown-requested", "shutdown-dispatched", "legacy-stopped", "unreadable-journal", "stopped-endpoint", "inequivalent-alias"} {
		t.Run(fault, func(t *testing.T) {
			f, _, join := newDriftedCompatibleAliasProbe(t)
			rewritePreparedRecoveryAlias(t, f, commentedPreparedAlias)
			switch fault {
			case "shutdown-requested", "shutdown-dispatched", "legacy-stopped":
				faultPreparedPrivateObservation(t, f, fault)
			case "unreadable-journal":
				withPreparedPrivateFaultLock(t, f, func() {
					writePrivateSuccessorTestBytes(t, privateMigrationRecordPath(filepath.Join(f.stateRoot, privateSupervisorDirectory)), []byte("{"))
				})
			case "stopped-endpoint":
				join()
				removePrivateTestDaemonState(t, f.stateRoot)
			case "inequivalent-alias":
				rewritePreparedRecoveryAlias(t, f, configWithFileEditMode)
			default:
				t.Fatalf("unknown alias fault %q", fault)
			}
			before := snapshotSavedProbeTestState(t, f.stateRoot)
			called := 0
			client, err := ProbeSavedPrivateTaskObservation(t.Context(), f.stateRoot, f.legacy.Binding(), f.legacy.options, func(context.Context) (string, string, error) { called++; return "", "", nil })
			closeSavedProbeTestClient(t, client, err)
			if client != nil || called != 0 {
				t.Fatalf("unsafe %s alias became ready or selected another installation: client=%t selector=%d err=%v", fault, client != nil, called, err)
			}
			if fault == "unreadable-journal" && !errors.Is(err, ErrBinding) {
				t.Fatalf("unreadable journal lost its binding failure: %v", err)
			}
			assertSavedProbeTestUnchanged(t, f.stateRoot, before)
			assertPrivateDaemonStartCount(t, f.stateRoot, 1)
		})
	}
}
