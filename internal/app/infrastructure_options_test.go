package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInfrastructureSupervisorOptionsUsesAcceptedSnapshots(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PROVIDER_KEY", "ambient-provider")
	artifacts := infrastructure.Artifacts{Runner: "/accepted/runner", Client: "/accepted/client", Daemon: "/accepted/daemon"}
	control := []string{"PATH=/control/bin", "CONTROL_BYTES=\xff"}
	maintenance := []string{"PATH=/maintenance/bin", "PROVIDER_KEY=accepted-provider", "PROVIDER_BYTES=\xfe"}
	launch := infrastructure.LaunchContext{ControlEnvironment: control, MaintenanceEnvironment: maintenance}
	options := infrastructureSupervisorOptions(artifacts, launch)
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
	})
	if options.Environment == nil || len(options.Environment) != 0 || options.PrivateMaintenanceEnvironment == nil || len(options.PrivateMaintenanceEnvironment) != 0 {
		t.Fatal("explicit empty environments replaced")
	}
	if options.ControlOwnerExecutable != "" || options.PrivateMaintenanceLauncher != nil {
		t.Fatal("empty accepted runner triggered ambient selection")
	}
}

func requireDefaultInfrastructureCallbacks(t *testing.T, options pueue.Options) {
	t.Helper()
	if options.PrivateMaintenanceLauncher == nil || options.SuccessfulCleanupAuthorizer == nil || options.PrivateTerminalEvidencePreparer == nil || options.PrivateTerminalCustodyReleaser == nil || options.PrivateMigrationAuthorizer == nil {
		t.Fatal("infrastructure composition omitted existing app callbacks")
	}
}

func TestDefaultSupervisorCallbacksPreservesInjectedOptions(t *testing.T) {
	var calls [6]int
	options := pueue.Options{
		ObservationTimeout: 17 * time.Second, WaitForPendingClose: true,
		ControlOwnerExecutable: "/injected/owner", Environment: []string{"CONTROL=1"}, PrivateMaintenanceEnvironment: []string{"MAINTENANCE=2"},
		PrivateMaintenanceLauncher: func(context.Context, string, string, []string) error { calls[0]++; return nil },
		SuccessfulCleanupAuthorizer: func(context.Context, string, task.SupervisorRef, []pueue.Job) (pueue.PrivateQueueCleanupPlan, error) {
			calls[1]++
			return pueue.PrivateQueueCleanupPlan{}, nil
		},
		PrivateTerminalEvidencePreparer: func(context.Context, string, task.SupervisorRef, []pueue.Job) error { calls[2]++; return nil },
		PrivateTerminalCustodyReleaser: func(context.Context, string, task.SupervisorRef, pueue.PrivateTerminalCustodyScope) error {
			calls[3]++
			return nil
		},
		PrivateMigrationAuthorizer: func(context.Context, string, task.SupervisorRef, []pueue.Job) error { calls[4]++; return nil },
		Observer:                   func(pueue.CommandEvent) { calls[5]++ },
	}
	got := withDefaultSupervisorCallbacks(options)
	if got.ObservationTimeout != options.ObservationTimeout || !got.WaitForPendingClose || got.ControlOwnerExecutable != options.ControlOwnerExecutable || !slices.Equal(got.Environment, options.Environment) || !slices.Equal(got.PrivateMaintenanceEnvironment, options.PrivateMaintenanceEnvironment) {
		t.Fatal("default callbacks changed injected options")
	}
	invokeInjectedSupervisorCallbacks(t, got)
	if calls != [6]int{1, 1, 1, 1, 1, 1} {
		t.Fatalf("custom callback calls = %v", calls)
	}
}

func invokeInjectedSupervisorCallbacks(t *testing.T, options pueue.Options) {
	t.Helper()
	ctx := t.Context()
	if err := options.PrivateMaintenanceLauncher(ctx, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := options.SuccessfulCleanupAuthorizer(ctx, "", task.SupervisorRef{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := options.PrivateTerminalEvidencePreparer(ctx, "", task.SupervisorRef{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := options.PrivateTerminalCustodyReleaser(ctx, "", task.SupervisorRef{}, pueue.PrivateTerminalCustodyScope{}); err != nil {
		t.Fatal(err)
	}
	if err := options.PrivateMigrationAuthorizer(ctx, "", task.SupervisorRef{}, nil); err != nil {
		t.Fatal(err)
	}
	options.Observer(pueue.CommandEvent{})
}

func TestDefaultSupervisorCallbacksLeavesMaintenanceUnsetWithoutOwner(t *testing.T) {
	options := withDefaultSupervisorCallbacks(pueue.Options{})
	if options.PrivateMaintenanceLauncher != nil || options.ControlOwnerExecutable != "" {
		t.Fatal("default callbacks discovered a maintenance owner")
	}
	if options.SuccessfulCleanupAuthorizer == nil || options.PrivateTerminalEvidencePreparer == nil || options.PrivateTerminalCustodyReleaser == nil || options.PrivateMigrationAuthorizer == nil {
		t.Fatal("default callbacks omitted app policies")
	}
}
