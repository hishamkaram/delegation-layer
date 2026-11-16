package control

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestObservePrivateMigrationPhaseReadsValidatedProgress(t *testing.T) {
	for _, test := range []struct {
		name                           string
		requested, dispatched, stopped bool
		want                           PrivateMigrationPhase
	}{
		{"accepted", false, false, false, PrivateMigrationAccepted},
		{"requested", true, false, false, PrivateMigrationPredecessorShutdownRequested},
		{"dispatched", false, true, false, PrivateMigrationPredecessorShutdownDispatched},
		{"requested_dispatched", true, true, false, PrivateMigrationPredecessorShutdownDispatched},
		{"stopped", false, false, true, PrivateMigrationPredecessorStopped},
		{"requested_stopped", true, false, true, PrivateMigrationPredecessorStopped},
		{"dispatched_stopped", false, true, true, PrivateMigrationPredecessorStopped},
		{"all_flags", true, true, true, PrivateMigrationPredecessorStopped},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, bindings := newPrivateSuccessorTestState(t)
			record := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(bindings["A"]), Successor: privateDaemonIdentityForBinding(bindings["B"]), LegacyPID: 123, LegacyProcessStart: "fixture-native-start", LegacyShutdownRequested: test.requested, LegacyShutdownDispatched: test.dispatched, LegacyStopped: test.stopped}
			writePrivateSuccessorTestRecord(t, privateMigrationRecordPath(filepath.Join(root, privateSupervisorDirectory)), record)
			before := snapshotPrivateSuccessorTestState(t, root)
			phase, found, err := ObservePrivateMigrationPhase(root)
			if err != nil || !found || phase != test.want {
				t.Fatalf("migration phase: phase=%q found=%t want=%q err=%v", phase, found, test.want, err)
			}
			if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, root)) {
				t.Fatal("passive migration phase observation changed state")
			}
		})
	}
}

func TestObservePrivateMigrationPhasePreservesMissingRootAndJournal(t *testing.T) {
	root := filepath.Join(canonicalTemp(t), "missing-root")
	if phase, found, err := ObservePrivateMigrationPhase(root); err != nil || found || phase != "" {
		t.Fatalf("missing root became migration state: phase=%q found=%t err=%v", phase, found, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive observer created missing root: %v", err)
	}
	root, _ = newPrivateSuccessorTestState(t)
	if err := os.Remove(filepath.Join(root, privateSupervisorDirectory, "bootstrap.lock")); err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, root)
	if phase, found, err := ObservePrivateMigrationPhase(root); err != nil || found || phase != "" {
		t.Fatalf("missing journal became migration state: phase=%q found=%t err=%v", phase, found, err)
	}
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, root)) {
		t.Fatal("passive observer created a lock or journal")
	}
}

func TestObservePrivateMigrationPhaseRejectsInvalidRoot(t *testing.T) {
	for _, root := range []string{"", "relative-root", "/not-clean/../root"} {
		phase, found, err := ObservePrivateMigrationPhase(root)
		var recovery *PrivateRecoveryError
		if phase != "" || found || !errors.Is(err, ErrConfiguration) || errors.Is(err, ErrPrivateSupervisorMigrationRequired) || errors.As(err, &recovery) {
			t.Fatalf("invalid root changed configuration contract: root=%q phase=%q found=%t err=%v", root, phase, found, err)
		}
	}
}

func TestObservePrivateMigrationPhasePreservesStoreFaults(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		cause   error
	}{
		{"malformed JSON", "}", ErrBinding},
		{"incomplete record", `{"schema_version":1}`, ErrBinding},
		{"unsupported schema", "", task.ErrUnsupportedSchema},
		{"invalid identity", "", ErrBinding},
		{"unreadable journal", "", ErrConfiguration},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := materializePrivateMigrationReaderFault(t, test.name, test.payload)
			before := snapshotPrivateSuccessorTestState(t, root)
			phase, found, err := ObservePrivateMigrationPhase(root)
			assertMigrationPhaseReadFault(t, phase, found, err, test.cause)
			if test.name == "malformed JSON" {
				var syntax *json.SyntaxError
				if !errors.As(err, &syntax) {
					t.Fatalf("migration observation lost JSON cause: %v", err)
				}
			}
			if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, root)) {
				t.Fatal("faulted passive observation changed persisted state")
			}
		})
	}
}

func TestObservePrivateMigrationPhaseRejectsImpossibleProcessState(t *testing.T) {
	root, bindings := newPrivateSuccessorTestState(t)
	record := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(bindings["A"]), Successor: privateDaemonIdentityForBinding(bindings["B"]), LegacyStopped: true}
	writePrivateSuccessorTestRecord(t, privateMigrationRecordPath(filepath.Join(root, privateSupervisorDirectory)), record)
	before := snapshotPrivateSuccessorTestState(t, root)
	phase, found, err := ObservePrivateMigrationPhase(root)
	assertMigrationPhaseReadFault(t, phase, found, err, ErrBinding)
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, root)) {
		t.Fatal("invalid process observation repaired its journal")
	}
}

func assertMigrationPhaseReadFault(t *testing.T, phase PrivateMigrationPhase, found bool, err, cause error) {
	t.Helper()
	var recovery *PrivateRecoveryError
	if phase != "" || found || !errors.As(err, &recovery) || recovery.ReasonCode != "state_unreadable" || !errors.Is(err, cause) || !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("migration phase read lost persisted fault: phase=%q found=%t err=%v", phase, found, err)
	}
}
