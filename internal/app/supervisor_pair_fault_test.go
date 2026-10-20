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
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// pairFaultRecords are the private generation records and the fake daemon's
// start log. A refused mutation leaves every one byte-identical.
var pairFaultRecords = []string{
	"supervisor.identity.json", "daemon.identity.json", "bootstrap.pending",
	"migration.json", "migration-history.json", "identity.pending.json", "daemon-started",
}

// pairFaultFile is one record's presence and exact bytes.
type pairFaultFile struct {
	present bool
	data    string
}

func capturePairFaultWitness(t *testing.T, base string) map[string]pairFaultFile {
	t.Helper()
	witness := make(map[string]pairFaultFile, len(pairFaultRecords))
	for _, name := range pairFaultRecords {
		data, err := os.ReadFile(filepath.Join(base, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			witness[name] = pairFaultFile{}
		case err != nil:
			t.Fatal(err)
		default:
			witness[name] = pairFaultFile{present: true, data: string(data)}
		}
	}
	return witness
}

func assertPairFaultWitness(t *testing.T, base string, before map[string]pairFaultFile) {
	t.Helper()
	after := capturePairFaultWitness(t, base)
	for _, name := range pairFaultRecords {
		if after[name] != before[name] {
			t.Fatalf("refused pair recovery changed %s (present %t -> %t)", name, before[name].present, after[name].present)
		}
	}
}

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

func migratePairFault(t *testing.T, name, root string, deps Dependencies) commandResult {
	t.Helper()
	result := supervisorMigrate(root, deps, newSupervisorTestResponse())
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "supervisor", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	control := result.response.SupervisorControl
	t.Logf("%s migrate: exit=%d err=%t reason=%q state=%q next_action=%q", name, result.code, result.err != nil, control.ReasonCode, control.State, control.NextAction)
	return result
}

func appendPairFaultBytes(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("\n# corrupted present member\n"); err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAdmissionRefusesCorruptPresentPublishedPair pairs the permitted
// observation of a complete published generation whose present client bytes
// changed with refused repair: admission reports migration-required and no
// record, binding, or daemon start changes.
func TestAdmissionRefusesCorruptPresentPublishedPair(t *testing.T) {
	store, deps, current := newSupervisorProbeTestFixture(t)
	binding := current.Binding()
	base := filepath.Dir(binding.ConfigPath)
	appendPairFaultBytes(t, binding.ClientExecutable)
	before := capturePairFaultWitness(t, base)
	err := dispatchPairFault(t, "corrupt present", func(ctx context.Context) (*pueue.Client, error) {
		return supervisor.PreparePrivateAdmissionTransition(ctx, deps.InitialSupervisorExecutable, store, deps.SupervisorOptions)
	}, store.Root)
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("corrupt present published pair was admitted: %v", err)
	}
	assertPairFaultWitness(t, base, before)
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
			before := capturePairFaultWitness(t, base)
			err := dispatchPairFault(t, "unreadable "+record, func(ctx context.Context) (*pueue.Client, error) {
				return supervisor.PreparePrivateAdmissionTransition(ctx, deps.InitialSupervisorExecutable, store, deps.SupervisorOptions)
			}, store.Root)
			if pairFaultReason(err) != "state_unreadable" {
				t.Fatalf("unreadable %s was not reported as state_unreadable: %v", record, err)
			}
			assertPairFaultWitness(t, base, before)
		})
	}
}

// writePairRestartIntent records a restart from the published pair to its
// retained copy and returns the retained client path.
func writePairRestartIntent(t *testing.T, root string, original task.SupervisorRef) string {
	t.Helper()
	client, daemon, err := supervisor.InstallStateSupervisorPairPaths(t.Context(), root, original.ClientExecutable, original.DaemonExecutable)
	if err != nil {
		t.Fatal(err)
	}
	target := original
	target.ClientExecutable, target.DaemonExecutable = client, daemon
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
	return client
}

// TestExplicitMigrationReportsUnreadableRecordBesideRestartIntent keeps an
// unreadable migration record visible beside a recorded restart intent. It is
// never a clean transition deferral, and no restart or record change occurs.
func TestExplicitMigrationReportsUnreadableRecordBesideRestartIntent(t *testing.T) {
	store, deps, current := newSupervisorProbeTestFixture(t)
	original := current.Binding()
	base := filepath.Dir(original.ConfigPath)
	writePairRestartIntent(t, store.Root, original)
	if err := os.WriteFile(filepath.Join(base, "migration.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := capturePairFaultWitness(t, base)
	result := migratePairFault(t, "restart intent with unreadable migration record", store.Root, deps)
	if result.err == nil || result.response.SupervisorControl.ReasonCode != "state_unreadable" {
		t.Fatalf("unreadable migration record was suppressed: %+v %v", result.response.SupervisorControl, result.err)
	}
	assertPairFaultWitness(t, base, before)
}

// TestCorruptPresentPublishedPairMigrationOutcome records the explicit
// migration consumer for a complete generation whose present client bytes
// changed.
func TestCorruptPresentPublishedPairMigrationOutcome(t *testing.T) {
	store, deps, current := newSupervisorProbeTestFixture(t)
	binding := current.Binding()
	appendPairFaultBytes(t, binding.ClientExecutable)
	result := migratePairFault(t, "corrupt present", store.Root, deps)
	if result.err == nil || !errors.Is(result.err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("corrupt present published pair migrated: %+v %v", result.response.SupervisorControl, result.err)
	}
}

// removePublishedDaemonBehindRetainedPair installs the retained pair and then
// removes the published daemon while its endpoint stays live.
func removePublishedDaemonBehindRetainedPair(t *testing.T, root string, original task.SupervisorRef) {
	t.Helper()
	if _, _, err := supervisor.InstallStateSupervisorPairPaths(t.Context(), root, original.ClientExecutable, original.DaemonExecutable); err != nil {
		t.Fatal(err)
	}
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
		before := capturePairFaultWitness(t, base)
		err := dispatchPairFault(t, "live missing member", func(ctx context.Context) (*pueue.Client, error) {
			return supervisor.PreparePrivateAdmissionTransition(ctx, deps.InitialSupervisorExecutable, store, deps.SupervisorOptions)
		}, store.Root)
		if pairFaultReason(err) != "published_pair_unavailable_live" {
			t.Fatalf("live endpoint with a missing member was not reported: %v", err)
		}
		assertPairFaultWitness(t, base, before)
	})
	t.Run("explicit migration", func(t *testing.T) {
		store, deps, current := newSupervisorProbeTestFixture(t)
		base := filepath.Dir(current.Binding().ConfigPath)
		removePublishedDaemonBehindRetainedPair(t, store.Root, current.Binding())
		result := migratePairFault(t, "live missing member", store.Root, deps)
		if result.response.SupervisorControl.ReasonCode == "published_pair_unavailable_live" {
			t.Fatalf("explicit migration stopped at the live endpoint: %+v %v", result.response.SupervisorControl, result.err)
		}
		if starts, err := os.ReadFile(filepath.Join(base, "daemon-started")); err != nil || string(starts) != "started\n" {
			t.Fatalf("explicit migration restarted the live daemon: %q %v", starts, err)
		}
	})
}

// writeConflictingLaunchWitness installs the retained pair and records a
// launch witness that names neither the published nor the retained daemon.
func writeConflictingLaunchWitness(t *testing.T, root string, original task.SupervisorRef) {
	t.Helper()
	if _, _, err := supervisor.InstallStateSupervisorPairPaths(t.Context(), root, original.ClientExecutable, original.DaemonExecutable); err != nil {
		t.Fatal(err)
	}
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
		before := capturePairFaultWitness(t, base)
		err := dispatchPairFault(t, "identity conflict", func(ctx context.Context) (*pueue.Client, error) {
			return supervisor.PreparePrivateAdmissionTransition(ctx, deps.InitialSupervisorExecutable, store, deps.SupervisorOptions)
		}, store.Root)
		if pairFaultReason(err) != "identity_conflict" {
			t.Fatalf("identity conflict was not reported: %v", err)
		}
		assertPairFaultWitness(t, base, before)
	})
	t.Run("explicit migration", func(t *testing.T) {
		store, deps, current := newSupervisorProbeTestFixture(t)
		base := filepath.Dir(current.Binding().ConfigPath)
		writeConflictingLaunchWitness(t, store.Root, current.Binding())
		before := capturePairFaultWitness(t, base)
		result := migratePairFault(t, "identity conflict", store.Root, deps)
		if result.err == nil || result.response.SupervisorControl.ReasonCode != "identity_conflict" {
			t.Fatalf("explicit migration absorbed an identity conflict: %+v %v", result.response.SupervisorControl, result.err)
		}
		assertPairFaultWitness(t, base, before)
	})
}
