package app

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// This finite effects fixture owns only call observations. Domain fixtures
// separately prove canonical record checks and the authenticated stop protocol.
func TestManualMigrationUsesAcceptedPairWithoutInstallationDiscovery(t *testing.T) {
	root := newBareInspectionStore(t).Root
	accepted := installation.BootstrapPair{
		Client: filepath.Join(root, "accepted", "pueue"),
		Daemon: filepath.Join(root, "accepted", "pueued"),
	}
	lookup := installation.ExecutableLookup{
		InitialExecutable: filepath.Join(root, "unavailable-installation", "delegate"),
		Environment:       []string{}, AcceptedBootstrapPair: accepted,
	}
	before := supervisorfixture.SnapshotStore(t, root)
	client, daemon, err := migrationSupervisorPair(lookup)
	if err != nil || client != accepted.Client || daemon != accepted.Daemon {
		t.Fatalf("resident rediscovered installation instead of accepted pair: %s %s %v", client, daemon, err)
	}
	if after := supervisorfixture.SnapshotStore(t, root); !maps.Equal(before, after) {
		t.Fatal("accepted pair selection prepared target state")
	}
}

func TestManualMigrationRejectsPartialAcceptedPairWithoutFallback(t *testing.T) {
	root := newBareInspectionStore(t).Root
	for _, accepted := range []installation.BootstrapPair{
		{Client: filepath.Join(root, "client")},
		{Daemon: filepath.Join(root, "daemon")},
		{Client: "relative-client", Daemon: filepath.Join(root, "daemon")},
	} {
		_, _, err := migrationSupervisorPair(installation.ExecutableLookup{AcceptedBootstrapPair: accepted})
		if !errors.Is(err, pueue.ErrConfiguration) {
			t.Fatalf("invalid accepted pair fell back to discovery: %v", err)
		}
	}
}

func TestManualMigrationCLIStillSelectsCurrentInstallation(t *testing.T) {
	bundle := supervisorfixture.CanonicalTempDir(t)
	client, daemon := filepath.Join(bundle, "pueue"), filepath.Join(bundle, "pueued")
	writeRunnerFixture(t, client, "#!/bin/sh\nexit 0\n")
	writeRunnerFixture(t, daemon, "#!/bin/sh\nexit 0\n")
	selectedClient, selectedDaemon, err := migrationSupervisorPair(installation.ExecutableLookup{InitialExecutable: client, Environment: []string{}})
	if err != nil || selectedClient != client || selectedDaemon != daemon {
		t.Fatalf("CLI migration changed installation selection: %s %s %v", selectedClient, selectedDaemon, err)
	}
}

func TestManualMigrationReportUsesCanonicalPublicProjection(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		state supervisorMigrationState
		err   error
		json  string
	}{
		{"not initialized", supervisorMigrationState{notInitialized: true}, nil, `{"state":"not_initialized","legacy":"not_present","active":"none"}`},
		{"external", supervisorMigrationState{externalInspectionGroup: true}, nil, `{"state":"migrated","legacy":"not_managed","active":"unknown"}`},
		{"private idle", supervisorMigrationState{noInspectionGroup: true, activeWork: "none"}, nil, `{"state":"migrated","legacy":"not_present","active":"none"}`},
		{"private active", supervisorMigrationState{noInspectionGroup: true, activeWork: "present"}, nil, `{"state":"migrated","legacy":"not_present","active":"present"}`},
		{"recovered group", supervisorMigrationState{}, nil, `{"state":"migrated","legacy":"recovered","active":"none"}`},
		{"busy", supervisorMigrationState{}, pueue.ErrPrivateSupervisorMigrationBusy, `{"state":"blocked","legacy":"unknown","active":"present"}`},
		{"IO", supervisorMigrationState{}, os.ErrPermission, `{"state":"unavailable","legacy":"unknown","active":"unknown"}`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			result := manualSupervisorMigrationResult(fixture.state, fixture.err)
			report, err := infrastructureManualMigrationReport(result)
			if err != nil {
				t.Fatal(err)
			}
			data, err := task.MarshalCanonical(report)
			if err != nil || string(data) != fixture.json+"\n" {
				t.Fatalf("canonical public migration report changed: %s %v", data, err)
			}
			if fixture.err != nil && !errors.Is(result.err, fixture.err) {
				t.Fatal("report projection discarded original cause")
			}
		})
	}
}

func TestManualMigrationReportSurvivesLateCleanupFault(t *testing.T) {
	result := manualSupervisorMigrationResult(supervisorMigrationState{}, nil)
	report, err := infrastructureManualMigrationReport(result)
	if err != nil {
		t.Fatal(err)
	}
	projected := finishRoutedSupervisorMigration(newSupervisorCommandResponse("migrate"), report, os.ErrPermission)
	control := projected.response.SupervisorControl
	if projected.code != 1 || !errors.Is(projected.err, os.ErrPermission) || control.State != "migrated" || control.MigrationPhase != "complete" || control.NextAction != "stop_and_report" {
		t.Fatalf("CLI lost completed migration or made late cleanup retryable: %+v %v", control, projected.err)
	}
}

func TestRoutedManualMigrationReportPreservesCanonicalProjection(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		state supervisorMigrationState
		cause error
	}{
		{"not initialized", supervisorMigrationState{notInitialized: true}, nil},
		{"external group", supervisorMigrationState{externalInspectionGroup: true}, nil},
		{"private idle", supervisorMigrationState{noInspectionGroup: true, activeWork: "none"}, nil},
		{"private active", supervisorMigrationState{noInspectionGroup: true, activeWork: "present"}, nil},
		{"recovered group", supervisorMigrationState{}, nil},
		{"busy", supervisorMigrationState{}, pueue.ErrPrivateSupervisorMigrationBusy},
		{"IO", supervisorMigrationState{}, os.ErrPermission},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			canonical := manualSupervisorMigrationResult(fixture.state, fixture.cause)
			report, err := infrastructureManualMigrationReport(canonical)
			if err != nil {
				t.Fatal(err)
			}
			projected := finishRoutedSupervisorMigration(newSupervisorCommandResponse("migrate"), report, fixture.cause)
			want, err := task.MarshalCanonical(canonical.response.SupervisorControl)
			if err != nil {
				t.Fatal(err)
			}
			got, err := task.MarshalCanonical(projected.response.SupervisorControl)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) || projected.code != canonical.code {
				t.Fatalf("routed report changed public policy: got=%s want=%s code=%d want=%d", got, want, projected.code, canonical.code)
			}
			if fixture.cause != nil && !errors.Is(projected.err, fixture.cause) {
				t.Fatal("projection dropped original cause")
			}
		})
	}
}
