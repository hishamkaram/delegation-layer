package infrastructurehost

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func TestInfrastructureSupervisorOptionsUsesAcceptedSnapshots(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PROVIDER_KEY", "ambient-provider")
	artifacts := infrastructure.Artifacts{Runner: "/accepted/runner"}
	control := []string{"PATH=/control/bin", "CONTROL_BYTES=\xff"}
	maintenance := []string{"PATH=/maintenance/bin", "PROVIDER_KEY=accepted-provider", "PROVIDER_BYTES=\xfe"}
	launch := infrastructure.LaunchContext{
		ControlEnvironment: control, MaintenanceEnvironment: maintenance,
		SupervisorExecutable: filepath.Join(t.TempDir(), "selected", "pueue"),
	}
	options := infrastructureSupervisorOptions(artifacts, launch, testCallbacks())
	if options.RuntimeClientExecutable != launch.SupervisorExecutable {
		t.Fatalf("runtime client = %q, want accepted %q", options.RuntimeClientExecutable, launch.SupervisorExecutable)
	}
	if options.ControlOwnerExecutable != artifacts.Runner {
		t.Fatalf("control owner = %q", options.ControlOwnerExecutable)
	}
	if !slices.Equal(options.Environment, control) || !slices.Equal(options.PrivateMaintenanceEnvironment, maintenance) {
		t.Fatal("accepted environment bytes changed")
	}
	if options.DaemonStartGuard != nil {
		t.Fatal("options composition minted daemon start authority")
	}
	requireDefaultInfrastructureCallbacks(t, options)
	control[0], maintenance[0] = "CONTROL_MUTATED=1", "MAINTENANCE_MUTATED=1"
	if options.Environment[0] != "PATH=/control/bin" || options.PrivateMaintenanceEnvironment[0] != "PATH=/maintenance/bin" {
		t.Fatal("accepted snapshots alias options")
	}
	options.Environment[1], options.PrivateMaintenanceEnvironment[1] = "CONTROL_CHANGED=1", "MAINTENANCE_CHANGED=1"
	if control[1] != "CONTROL_BYTES=\xff" || maintenance[1] != "PROVIDER_KEY=accepted-provider" {
		t.Fatal("options mutated accepted snapshots")
	}
}

func TestInfrastructureSupervisorOptionsPreservesExplicitEmpty(t *testing.T) {
	t.Setenv("PATH", "/ambient/bin")
	t.Setenv("PROVIDER_KEY", "ambient-provider")
	options := infrastructureSupervisorOptions(infrastructure.Artifacts{}, infrastructure.LaunchContext{
		ControlEnvironment: []string{}, MaintenanceEnvironment: []string{},
	}, testCallbacks())
	if options.Environment == nil || len(options.Environment) != 0 || options.PrivateMaintenanceEnvironment == nil || len(options.PrivateMaintenanceEnvironment) != 0 {
		t.Fatal("explicit empty environments replaced")
	}
	if options.ControlOwnerExecutable != "" || options.RuntimeClientExecutable != "" || options.PrivateMaintenanceLauncher != nil {
		t.Fatal("empty accepted executable triggered ambient selection")
	}
}

func requireDefaultInfrastructureCallbacks(t *testing.T, options pueue.Options) {
	t.Helper()
	if options.PrivateMaintenanceLauncher == nil || options.SuccessfulCleanupAuthorizer == nil || options.PrivateTerminalEvidencePreparer == nil || options.PrivateTerminalCustodyReleaser == nil || options.PrivateMigrationAuthorizer == nil {
		t.Fatal("infrastructure composition omitted existing app callbacks")
	}
}
