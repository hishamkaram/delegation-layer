package supervisor

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestContinuationPairSelectionPreservesCurrentInventory(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	current := parentMigrationPair(t, fixture, "current-C")
	selection := AdmissionSelection{SavedSupervisor: &fixture.legacy, ContinuationSource: source}
	client, daemon, err := AdmissionSupervisorExecutablePair(selection, fixture.store.Root, AdmissionOptions{})
	if err != nil || client != current.ClientExecutable || daemon != current.DaemonExecutable {
		t.Fatalf("continuation selected historical A over current C: %v", err)
	}
	before := parentMigrationTree(t, fixture.store.Root)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = pueue.WithSupervisorLeaseContext(ctx, fixture.store.Root, func(locked context.Context) error {
		return prepareContinuationStateSupervisorPair(locked, fixture.store.Root, client, daemon)
	})
	if err != nil {
		t.Fatalf("unchanged continuation inventory required bootstrap EX: %v", err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("warm continuation changed inventory bytes, modes or timestamps")
	}
}

func TestContinuationPairPublicationPreservesInterveningOwner(t *testing.T) {
	for _, initiallyPresent := range []bool{true, false} {
		t.Run(map[bool]string{true: "C then D", false: "absent then D"}[initiallyPresent], func(t *testing.T) {
			exerciseContinuationPairInterleaving(t, initiallyPresent)
		})
	}
}

func exerciseContinuationPairInterleaving(t *testing.T, initiallyPresent bool) {
	t.Helper()
	fixture, source := newWarmContinuationFixture(t)
	if initiallyPresent {
		parentMigrationPair(t, fixture, "inventory-C")
	} else if err := os.Remove(filepath.Join(fixture.store.Root, ".supervisor", stateSupervisorPairRecordName)); err != nil {
		t.Fatal(err)
	}
	selectedClient, selectedDaemon, err := AdmissionSupervisorExecutablePair(AdmissionSelection{SavedSupervisor: &fixture.legacy, ContinuationSource: source}, fixture.store.Root, AdmissionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A real canonical publication wins after this invocation selected C (or
	// the published pair without inventory) and before staging that selection.
	newest := parentMigrationPair(t, fixture, "inventory-D")
	before := parentMigrationTree(t, fixture.store.Root)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = pueue.WithSupervisorLeaseContext(ctx, fixture.store.Root, func(locked context.Context) error {
		return prepareContinuationStateSupervisorPair(locked, fixture.store.Root, selectedClient, selectedDaemon)
	})
	if err != nil {
		t.Fatalf("continuation tried EX to replace concurrent D: %v", err)
	}
	client, daemon, found, err := ResolveStateSupervisorPair(filepath.Join(fixture.store.Root, ".supervisor"))
	if err != nil || !found || client != newest.ClientExecutable || daemon != newest.DaemonExecutable {
		t.Fatalf("continuation replaced intervening canonical D: %v", err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("continuation modified intervening owner's inventory")
	}
}

func TestContinuationPairSelectionRefusesInvalidExistingPointer(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	path := filepath.Join(fixture.store.Root, ".supervisor", stateSupervisorPairRecordName)
	if err := os.WriteFile(path, []byte("invalid pointer"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := parentMigrationTree(t, fixture.store.Root)
	_, _, err := AdmissionSupervisorExecutablePair(AdmissionSelection{SavedSupervisor: &fixture.legacy, ContinuationSource: source}, fixture.store.Root, AdmissionOptions{})
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid existing pointer became saved-pair fallback: %v", err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("invalid pointer refusal changed inventory")
	}
}

func TestContinuationPairAbsentInventoryKeepsSavedFallback(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	path := filepath.Join(fixture.store.Root, ".supervisor", stateSupervisorPairRecordName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(path), "supervisor.identity.json")); err != nil {
		t.Fatal(err)
	}
	client, daemon, err := AdmissionSupervisorExecutablePair(AdmissionSelection{SavedSupervisor: &fixture.legacy, ContinuationSource: source}, fixture.store.Root, AdmissionOptions{})
	if err != nil || client != fixture.legacy.ClientExecutable || daemon != fixture.legacy.DaemonExecutable {
		t.Fatalf("absent inventory lost saved-pair fallback: %v", err)
	}
	if err = prepareContinuationStateSupervisorPair(t.Context(), fixture.store.Root, client, daemon); err != nil {
		t.Fatal(err)
	}
	currentClient, currentDaemon, found, err := ResolveStateSupervisorPair(filepath.Dir(path))
	if err != nil || !found || currentClient != client || currentDaemon != daemon {
		t.Fatalf("still-absent inventory did not stage selected saved pair: %v", err)
	}
}

func TestContinuationPairRefusesUnavailableInterveningInventory(t *testing.T) {
	for _, missing := range []string{"member", "directory"} {
		t.Run(missing, func(t *testing.T) { exerciseUnavailableContinuationInventory(t, missing) })
	}
}

func exerciseUnavailableContinuationInventory(t *testing.T, missing string) {
	t.Helper()
	fixture, source := newWarmContinuationFixture(t)
	parentMigrationPair(t, fixture, "selected-C")
	client, daemon, err := AdmissionSupervisorExecutablePair(AdmissionSelection{SavedSupervisor: &fixture.legacy, ContinuationSource: source}, fixture.store.Root, AdmissionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	newest := parentMigrationPair(t, fixture, "intervening-D")
	pointer := filepath.Join(fixture.store.Root, ".supervisor", stateSupervisorPairRecordName)
	before, err := os.ReadFile(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if missing == "member" {
		err = os.Remove(newest.ClientExecutable)
	} else {
		err = os.RemoveAll(filepath.Dir(newest.ClientExecutable))
	}
	if err != nil {
		t.Fatal(err)
	}
	err = prepareContinuationStateSupervisorPair(t.Context(), fixture.store.Root, client, daemon)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unavailable existing inventory became replacement permission: %v", err)
	}
	after, readErr := os.ReadFile(pointer)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("unavailable intervening D pointer was overwritten: %v", readErr)
	}
}

func TestContinuationPairIgnoresUnusedVanishedFallback(t *testing.T) {
	fixture, _ := newWarmContinuationFixture(t)
	client, daemon := fixture.legacy.ClientExecutable, fixture.legacy.DaemonExecutable
	newest := parentMigrationPair(t, fixture, "intervening-D")
	if err := os.Remove(client); err != nil {
		t.Fatal(err)
	}
	before := parentMigrationTree(t, fixture.store.Root)
	if err := prepareContinuationStateSupervisorPair(t.Context(), fixture.store.Root, client, daemon); err != nil {
		t.Fatalf("unused vanished fallback blocked valid inventory D: %v", err)
	}
	actualClient, actualDaemon, found, err := ResolveStateSupervisorPair(filepath.Join(fixture.store.Root, ".supervisor"))
	if err != nil || !found || actualClient != newest.ClientExecutable || actualDaemon != newest.DaemonExecutable {
		t.Fatalf("unused fallback replaced current D: %v", err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("unused fallback changed valid inventory")
	}
}

func TestContinuationAbsentInventoryPrefersPublishedPair(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	removeContinuationInventory(t, fixture)
	before := parentMigrationTree(t, fixture.store.Root)
	client, daemon, err := AdmissionSupervisorExecutablePair(AdmissionSelection{SavedSupervisor: &fixture.legacy, ContinuationSource: source}, fixture.store.Root, AdmissionOptions{})
	if err != nil || client != fixture.target.ClientExecutable || daemon != fixture.target.DaemonExecutable {
		t.Fatalf("absent inventory selected historical A instead of published C: %v", err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("published pair selection changed target")
	}
}

func removeContinuationInventory(t *testing.T, fixture parentMigrationFixture) {
	t.Helper()
	if err := os.Remove(filepath.Join(fixture.store.Root, ".supervisor", stateSupervisorPairRecordName)); err != nil {
		t.Fatal(err)
	}
}

func TestContinuationAbsentInventoryKeepsLegacyPublishedPaths(t *testing.T) {
	for _, changed := range []string{"unchanged", "client", "daemon"} {
		t.Run(changed, func(t *testing.T) { exerciseLegacyPublishedContinuationPair(t, changed) })
	}
}

func exerciseLegacyPublishedContinuationPair(t *testing.T, changed string) {
	t.Helper()
	fixture, source := newWarmContinuationFixture(t)
	removeContinuationInventory(t, fixture)
	client, daemon := statePairStagingSources(t, "legacy published client", "legacy published daemon")
	published := fixture.target
	published.ClientExecutable, published.DaemonExecutable = client, daemon
	published.ClientSHA256 = task.ComputeSHA256([]byte("legacy published client"))
	published.DaemonSHA256 = task.ComputeSHA256([]byte("legacy published daemon"))
	writeContinuationFixtureRecord(t, filepath.Join(fixture.store.Root, ".supervisor", "supervisor.identity.json"), struct {
		SchemaVersion int                `json:"schema_version"`
		Binding       task.SupervisorRef `json:"binding"`
	}{task.SchemaVersion, published})
	if changed != "unchanged" {
		path := client
		if changed == "daemon" {
			path = daemon
		}
		if err := os.WriteFile(path, []byte("changed legacy executable"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	before := parentMigrationTree(t, fixture.store.Root)
	actualClient, actualDaemon, err := AdmissionSupervisorExecutablePair(AdmissionSelection{SavedSupervisor: &fixture.legacy, ContinuationSource: source}, fixture.store.Root, AdmissionOptions{})
	if err != nil || actualClient != client || actualDaemon != daemon {
		t.Fatalf("legacy published %s paths lost recovery compatibility: (%q,%q,%v)", changed, actualClient, actualDaemon, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("legacy published pair verification changed target")
	}
}

func TestContinuationAbsentInventoryRefusesInvalidPublication(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	removeContinuationInventory(t, fixture)
	if err := os.WriteFile(filepath.Join(fixture.store.Root, ".supervisor", "supervisor.identity.json"), []byte("invalid binding"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := parentMigrationTree(t, fixture.store.Root)
	_, _, err := AdmissionSupervisorExecutablePair(AdmissionSelection{SavedSupervisor: &fixture.legacy, ContinuationSource: source}, fixture.store.Root, AdmissionOptions{})
	if err == nil || !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatalf("invalid published C became historical fallback or changed target: %v", err)
	}
}

func TestContinuationAbsentInventoryMissingPublishedPairUsesCanonicalDiscovery(t *testing.T) {
	for _, missing := range []string{"client", "daemon", "both"} {
		t.Run(missing, func(t *testing.T) {
			fixture, source := newWarmContinuationFixture(t)
			removeContinuationInventory(t, fixture)
			paths := map[string][]string{
				"client": {fixture.target.ClientExecutable},
				"daemon": {fixture.target.DaemonExecutable},
				"both":   {fixture.target.ClientExecutable, fixture.target.DaemonExecutable},
			}
			for _, path := range paths[missing] {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			client, daemon := statePairStagingSources(t, "canonical fallback client", "canonical fallback daemon")
			before := parentMigrationTree(t, fixture.store.Root)
			selectedClient, selectedDaemon, err := AdmissionSupervisorExecutablePair(AdmissionSelection{SavedSupervisor: &fixture.legacy, ContinuationSource: source}, fixture.store.Root, AdmissionOptions{InitialExecutable: client})
			if err != nil || selectedClient != client || selectedDaemon != daemon {
				t.Fatalf("missing published %s selected historical A instead of canonical discovery: (%q,%q,%v)", missing, selectedClient, selectedDaemon, err)
			}
			if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
				t.Fatal("canonical candidate selection changed target")
			}
		})
	}
}

func TestContinuationAbsentInventoryRefusesDamagedManagedPublishedPair(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	removeContinuationInventory(t, fixture)
	if err := os.WriteFile(fixture.target.ClientExecutable, []byte("damaged managed client"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := parentMigrationTree(t, fixture.store.Root)
	_, _, err := AdmissionSupervisorExecutablePair(AdmissionSelection{SavedSupervisor: &fixture.legacy, ContinuationSource: source}, fixture.store.Root, AdmissionOptions{})
	if !errors.Is(err, pueue.ErrBinding) || !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatalf("damaged managed published C became fallback or changed target: %v", err)
	}
}
