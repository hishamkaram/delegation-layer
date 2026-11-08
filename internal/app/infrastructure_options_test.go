package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
	got := nativeInfrastructureCallbacks().ApplyTo(options)
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
	options := nativeInfrastructureCallbacks().ApplyTo(pueue.Options{})
	if options.PrivateMaintenanceLauncher != nil || options.ControlOwnerExecutable != "" {
		t.Fatal("default callbacks discovered a maintenance owner")
	}
	if options.SuccessfulCleanupAuthorizer == nil || options.PrivateTerminalEvidencePreparer == nil || options.PrivateTerminalCustodyReleaser == nil || options.PrivateMigrationAuthorizer == nil {
		t.Fatal("default callbacks omitted app policies")
	}
}
