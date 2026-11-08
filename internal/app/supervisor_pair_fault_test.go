package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func pairFaultReason(err error) string {
	var recovery *pueue.PrivateRecoveryError
	if errors.As(err, &recovery) {
		return recovery.ReasonCode
	}
	return ""
}

// dispatchPairFault runs dispatch admission under the bootstrap lock and
// records the public dispatch failure projection.
func dispatchPairFault(t *testing.T, name string, run func(context.Context) (*pueue.Client, error), root string) error {
	t.Helper()
	var admitted *pueue.Client
	err := pueue.WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		var prepareErr error
		admitted, prepareErr = run(ctx)
		return prepareErr
	})
	settleSupervisorProbeTestClient(t, admitted, err)
	failure := dispatchFailure(admission.WithStage("start-supervisor", err), "not_created")
	t.Logf("%s dispatch: err=%t migration_required=%t reason=%q failure_code=%q next_action=%q", name, err != nil, errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired), pairFaultReason(err), failure.Code, failure.NextAction)
	return err
}

func migratePairFault(t *testing.T, name, root string, deps Dependencies, owner *componentSupervisorFixture) commandResult {
	t.Helper()
	result := supervisorMigrate(root, withSupervisorProbeRouting(t, deps, owner), newSupervisorTestResponse())
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "supervisor", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	control := result.response.SupervisorControl
	t.Logf("%s migrate: exit=%d err=%t reason=%q state=%q next_action=%q", name, result.code, result.err != nil, control.ReasonCode, control.State, control.NextAction)
	return result
}

// TestAdmissionReportsUnreadablePairRecords refuses repair while a generation
// record cannot be read, reporting state_unreadable without mutation.
func TestAdmissionReportsUnreadablePairRecords(t *testing.T) {
	for _, record := range []string{"daemon.identity.json", "migration.json"} {
		t.Run(record, func(t *testing.T) {
			store, deps, current := newSupervisorProbeTestFixture(t)
			base := filepath.Dir(current.Binding().ConfigPath)
			if err := os.WriteFile(filepath.Join(base, record), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := testsupport.CapturePairFaultWitness(t, base)
			err := dispatchPairFault(t, "unreadable "+record, func(ctx context.Context) (*pueue.Client, error) {
				return supervisor.PreparePrivateAdmissionTransition(ctx, deps.supervisorLookup(), store, deps.SupervisorOptions)
			}, store.Root)
			if pairFaultReason(err) != "state_unreadable" {
				t.Fatalf("unreadable %s was not reported as state_unreadable: %v", record, err)
			}
			testsupport.AssertPairFaultWitness(t, base, before)
		})
	}
}

// writePairRestartIntent records a restart to a missing selected installation
// and returns that installation's client path.
func writePairRestartIntent(t *testing.T, root string, original task.SupervisorRef) string {
	t.Helper()
	client := filepath.Join(testsupport.CanonicalTempDir(t), "missing-accepted", "pueue")
	target := original
	target.ClientExecutable, target.DaemonExecutable = client, filepath.Join(filepath.Dir(client), "pueued")
	target.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	target.ClientSHA256, target.DaemonSHA256 = "", ""
	writePairRestartIntentTo(t, original, target)
	return client
}

// writePairRestartIntentTo seeds the exact predecessor and target of a
// historical restart without launching or publishing the target.
func writePairRestartIntentTo(t *testing.T, original, target task.SupervisorRef) {
	t.Helper()
	record := struct {
		SchemaVersion int `json:"schema_version"`
		Restart       struct {
			Predecessor task.SupervisorRef `json:"predecessor"`
			Target      task.SupervisorRef `json:"target"`
		} `json:"restart"`
	}{SchemaVersion: task.SchemaVersion}
	record.Restart.Predecessor, record.Restart.Target = original, target
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(filepath.Dir(original.ConfigPath), "bootstrap.pending"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestExplicitMigrationReportsUnreadableRecordBesideRestartIntent keeps an
// unreadable migration record visible beside a recorded restart intent. It is
// never a clean transition deferral, and no restart or record change occurs.
func TestExplicitMigrationReportsUnreadableRecordBesideRestartIntent(t *testing.T) {
	store, deps, current, owner := newOwnedSupervisorProbeTestFixture(t)
	original := current.Binding()
	base := filepath.Dir(original.ConfigPath)
	writePairRestartIntent(t, store.Root, original)
	if err := os.WriteFile(filepath.Join(base, "migration.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := testsupport.CapturePairFaultWitness(t, base)
	result := migratePairFault(t, "restart intent with unreadable migration record", store.Root, deps, owner)
	if result.err == nil || result.response.SupervisorControl.ReasonCode != "state_unreadable" {
		t.Fatalf("unreadable migration record was suppressed: %+v %v", result.response.SupervisorControl, result.err)
	}
	testsupport.AssertPairFaultWitness(t, base, before)
}

// TestPendingHistoricalRestartStopsPublicConsumersBeforePairSelection keeps
// an installed but never-started B authoritative after A's supported shutdown.
// The future C transport exists, but its missing daemon must not be resolved.
func TestPendingHistoricalRestartStopsPublicConsumersBeforePairSelection(t *testing.T) {
	fixture := newHistoricalSupervisorFixturePaths(t)
	prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	bindHistoricalSupervisorFixture(t, fixture)
	ctx, cancel := supervisor.ControlContextWithParent(t.Context(), fixture.options)
	defer cancel()
	waits := fixture.component.daemonWaits()
	if len(waits) != 1 {
		t.Fatalf("predecessor daemon Wait count = %d, want one actual Start", len(waits))
	}
	if err := fixture.component.stopDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
	if err := awaitComponentDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
	target := fixture.predecessor
	target.ClientExecutable, target.DaemonExecutable = fixture.clientB, fixture.daemonB
	writePairRestartIntentTo(t, fixture.predecessor, target)
	deps := fixture.dependencies()
	deps.InitialSupervisorExecutable = filepath.Join(testsupport.CanonicalTempDir(t), "installation-c", "pueue")
	pid := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	writeRunnerFixture(t, deps.InitialSupervisorExecutable, historicalSupervisorClientScript("C", fixture.readyPath, pid, fixture.statusPath, fixture.commandLog))
	base := filepath.Dir(fixture.predecessor.ConfigPath)
	before := testsupport.CapturePairFaultWitness(t, base)
	configBefore := readSupervisorProbeTestFile(t, fixture.predecessor.ConfigPath)
	startsBefore := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	if string(startsBefore) != "A\n" {
		t.Fatalf("historical restart fixture started a target: %q", startsBefore)
	}
	consumers := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "explicit migration", run: func(t *testing.T) {
			result := migratePairFault(t, "present nonready historical restart", fixture.root, deps, fixture.component)
			if result.code != 1 || result.err == nil || result.response.SupervisorControl.ReasonCode != "transition_pending" {
				t.Fatalf("migration displaced pending historical restart: code=%d control=%+v err=%v", result.code, result.response.SupervisorControl, result.err)
			}
		}},
		{name: "admission", run: func(t *testing.T) {
			err := dispatchPairFault(t, "present nonready historical restart", func(ctx context.Context) (*pueue.Client, error) {
				return supervisor.PreparePrivateAdmissionTransition(ctx, deps.supervisorLookup(), fixture.store, fixture.options)
			}, fixture.root)
			if pairFaultReason(err) != "transition_pending" {
				t.Fatalf("admission displaced pending historical restart: %v", err)
			}
		}},
	}
	for _, consumer := range consumers {
		t.Run(consumer.name, func(t *testing.T) {
			consumer.run(t)
			testsupport.AssertPairFaultWitness(t, base, before)
			if after := readSupervisorProbeTestFile(t, fixture.predecessor.ConfigPath); !bytes.Equal(after, configBefore) {
				t.Fatal("pending historical restart changed config bytes")
			}
			if after := readSupervisorProbeTestFile(t, fixture.daemonStartLog); !bytes.Equal(after, startsBefore) {
				t.Fatalf("pending historical restart started another daemon: %q", after)
			}
		})
	}
}

// removePublishedDaemonBehindRetainedPair removes the selected installed
// daemon while its published endpoint stays live.
func removePublishedDaemonBehindRetainedPair(t *testing.T, root string, original task.SupervisorRef) {
	t.Helper()
	if err := os.Remove(original.DaemonExecutable); err != nil {
		t.Fatal(err)
	}
}

// TestLivePublishedPairWithMissingMemberKeepsConsumerPolicy observes a live
// endpoint whose published daemon member is missing. Admission reports
// published_pair_unavailable_live; explicit migration keeps its existing
// policy of continuing past that live state. Neither restarts the daemon.
func TestLivePublishedPairWithMissingMemberKeepsConsumerPolicy(t *testing.T) {
	t.Run("admission", func(t *testing.T) {
		store, deps, current := newSupervisorProbeTestFixture(t)
		base := filepath.Dir(current.Binding().ConfigPath)
		removePublishedDaemonBehindRetainedPair(t, store.Root, current.Binding())
		installed := filepath.Join(testsupport.CanonicalTempDir(t), "installation-c")
		deps.InitialSupervisorExecutable = filepath.Join(installed, "pueue")
		writeSupervisorProbeExecutables(t, store.RootID, deps.InitialSupervisorExecutable, filepath.Join(installed, "pueued"))
		before := testsupport.CapturePairFaultWitness(t, base)
		err := dispatchPairFault(t, "live missing member", func(ctx context.Context) (*pueue.Client, error) {
			return supervisor.PreparePrivateAdmissionTransition(ctx, deps.supervisorLookup(), store, deps.SupervisorOptions)
		}, store.Root)
		if pairFaultReason(err) != "published_pair_unavailable_live" {
			t.Fatalf("live endpoint with a missing member was not reported: %v", err)
		}
		testsupport.AssertPairFaultWitness(t, base, before)
	})
	t.Run("explicit migration", func(t *testing.T) {
		store, deps, current, owner := newOwnedSupervisorProbeTestFixture(t)
		base := filepath.Dir(current.Binding().ConfigPath)
		removePublishedDaemonBehindRetainedPair(t, store.Root, current.Binding())
		installed := filepath.Join(testsupport.CanonicalTempDir(t), "installation-c")
		deps.InitialSupervisorExecutable = filepath.Join(installed, "pueue")
		writeSupervisorProbeExecutables(t, store.RootID, deps.InitialSupervisorExecutable, filepath.Join(installed, "pueued"))
		result := migratePairFault(t, "live missing member", store.Root, deps, owner)
		if result.response.SupervisorControl.ReasonCode == "published_pair_unavailable_live" {
			t.Fatalf("explicit migration stopped at the live endpoint: %+v %v", result.response.SupervisorControl, result.err)
		}
		if starts, err := os.ReadFile(filepath.Join(base, "daemon-started")); err != nil || string(starts) != "started\n" {
			t.Fatalf("explicit migration restarted the live daemon: %q %v", starts, err)
		}
	})
}

// writeConflictingLaunchWitness records a launch witness that names a daemon
// outside the published installation.
func writeConflictingLaunchWitness(t *testing.T, root string, original task.SupervisorRef) {
	t.Helper()
	witness := historicalPrivateDaemonIdentityFor(original)
	witness.Executable += "-elsewhere"
	data, err := task.MarshalCanonical(witness)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(filepath.Dir(original.ConfigPath), "daemon.identity.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPairIdentityConflictStopsEveryConsumer keeps a reconciliation fault
// visible to admission and explicit migration alike: neither consumer's
// deferral policy may absorb an operational error, and nothing mutates.
func TestPairIdentityConflictStopsEveryConsumer(t *testing.T) {
	t.Run("admission", func(t *testing.T) {
		store, deps, current := newSupervisorProbeTestFixture(t)
		base := filepath.Dir(current.Binding().ConfigPath)
		writeConflictingLaunchWitness(t, store.Root, current.Binding())
		before := testsupport.CapturePairFaultWitness(t, base)
		err := dispatchPairFault(t, "identity conflict", func(ctx context.Context) (*pueue.Client, error) {
			return supervisor.PreparePrivateAdmissionTransition(ctx, deps.supervisorLookup(), store, deps.SupervisorOptions)
		}, store.Root)
		if pairFaultReason(err) != "identity_conflict" {
			t.Fatalf("identity conflict was not reported: %v", err)
		}
		testsupport.AssertPairFaultWitness(t, base, before)
	})
	t.Run("explicit migration", func(t *testing.T) {
		store, deps, current, owner := newOwnedSupervisorProbeTestFixture(t)
		base := filepath.Dir(current.Binding().ConfigPath)
		writeConflictingLaunchWitness(t, store.Root, current.Binding())
		before := testsupport.CapturePairFaultWitness(t, base)
		result := migratePairFault(t, "identity conflict", store.Root, deps, owner)
		if result.err == nil || result.response.SupervisorControl.ReasonCode != "identity_conflict" {
			t.Fatalf("explicit migration absorbed an identity conflict: %+v %v", result.response.SupervisorControl, result.err)
		}
		testsupport.AssertPairFaultWitness(t, base, before)
	})
}
