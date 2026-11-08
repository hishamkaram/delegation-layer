package infrastructurehost

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestInfrastructureRoutingSelectedOwnerDoesNotOpenUnavailableOrigin(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	missingOrigin := filepath.Join(supervisorfixture.CanonicalTempDir(t), "absent-origin")
	origin, err := infrastructure.NewOrigin(missingOrigin, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	environment, err := infrastructure.WithOrigin([]string{}, origin)
	if err != nil || len(environment) != 1 {
		t.Fatalf("encoding the original locator: %v", err)
	}
	_, raw, _ := strings.Cut(environment[0], "=")
	t.Setenv(infrastructure.OriginEnvironment, raw)
	invocation := CaptureInvocation(t.Context())
	if invocation.err != nil {
		t.Fatal(invocation.err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected := infrastructure.StartupReference{Sequence: 3, Epoch: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64)}
	resources := &infrastructureRoutingResources{target: store}
	defer func() {
		if closeErr := resources.finish(nil); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	routing := Routing{invocation: invocation}
	request := infrastructure.LaunchContext{
		Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS,
		ControlEnvironment: []string{}, MaintenanceEnvironment: []string{}, SupervisorExecutable: filepath.Join(store.Root, "pueue"),
	}
	selection, host, err := routing.selectPreparation(ctx, pin, request, infrastructure.InitialOperation{})
	if err != nil {
		t.Fatalf("target selection opened unavailable origin A before observing B: %v", err)
	}
	selection.owner = selected // Data from the independently tested authenticated probe.
	got, err := routing.establish(ctx, resources, pin, selection, host)
	if err != nil || got.Owner != selected || got.InitialResultFor != (infrastructure.StartupReference{}) || resources.origin != nil || resources.attempt != nil {
		t.Fatalf("an already selected owner depended on A or minted a replay selector: owner=%+v error=%v", got, err)
	}
	if _, err = os.Lstat(missingOrigin); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected-owner contact provisioned origin A: %v", err)
	}
}

func TestInfrastructureRoutingProbeDiagnosticPreservesConsumerPolicy(t *testing.T) {
	resources := &infrastructureRoutingResources{}
	for _, probe := range []error{os.ErrNotExist, infrastructure.ErrOwnerIncompatible} {
		for _, fixture := range []struct {
			name               string
			cause              error
			pending, uncertain bool
			maintenance        bool
		}{
			{"pending", task.ErrLockBusy, true, false, false},
			{"unknown", pueue.ErrUnknown, false, true, false},
			{"in-flight", pueue.ErrInFlight, false, true, false},
			{"transition", pueue.ErrPrivateSupervisorTransitionIncomplete, false, false, true},
			{"storage fault", os.ErrPermission, false, false, false},
			{"uncertainty with concrete fault", errors.Join(pueue.ErrUnknown, os.ErrPermission), false, false, false},
		} {
			t.Run(probe.Error()+"/"+fixture.name, func(t *testing.T) {
				got := resources.finishPreparation(fixture.cause, probe)
				if tasklifecycle.IsPureCollectionPendingError(got) != fixture.pending || tasklifecycle.IsPureSupervisorUncertaintyError(got) != fixture.uncertain || supervisor.TaskMaintenanceCompletionRefused(got) != fixture.maintenance {
					t.Fatalf("diagnostic probe changed actual consumer policy: %v", got)
				}
				if !errors.Is(got, fixture.cause) || errors.Is(got, probe) || !strings.Contains(got.Error(), probe.Error()) {
					t.Fatalf("probe diagnostic hid the actual cause or became a classified cause: %v", got)
				}
			})
		}
	}
	if err := resources.finishPreparation(nil, infrastructure.ErrOwnerBusy); err != nil {
		t.Fatalf("successful canonical establishment kept an obsolete status fault: %v", err)
	}
}

func TestInfrastructureRoutingProbeDiagnosticRetainsActualCleanupOwner(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	cause := &infrastructureRoutingCleanupError{resources: &infrastructureRoutingResources{target: store}, cause: os.ErrPermission}
	got := (&infrastructureRoutingResources{}).finishPreparation(cause, os.ErrNotExist)
	if !taskdir.CleanupRetainsStore(got, store) || !errors.Is(got, os.ErrPermission) {
		t.Fatalf("probe diagnostic lost the actual cleanup owner: %v", got)
	}
}

func TestInfrastructureRoutingManualMigrationSkipsOwnerProbe(t *testing.T) {
	host := newInfrastructureEstablishmentHost("/absent-install/delegate-run", infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn})
	var unavailableReader *taskdir.ReadOnlyStore
	selected, err := host.probePreparationOwner(t.Context(), unavailableReader)
	if err != nil || selected.probeErr != nil || selected.owner != (infrastructure.StartupReference{}) {
		t.Fatalf("manual migration probed an owner: selected=%+v error=%v", selected, err)
	}
}

func TestManualMigrationHelperMetadataFaultsDoNotRefresh(t *testing.T) {
	base := supervisorfixture.CanonicalTempDir(t)
	plain := filepath.Join(base, "non-executable")
	if err := os.WriteFile(plain, []byte("present file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		path string
		want error
	}{
		{"present non-executable", plain, pueue.ErrConfiguration},
		{"ENOTDIR", filepath.Join(plain, "delegate-run"), syscall.ENOTDIR},
		{"invalid selected path", "relative-runner", pueue.ErrConfiguration},
	} {
		t.Run(test.name, func(t *testing.T) {
			effects, owner := newManualMigrationEffects(t)
			effects.artifacts[owner.Owner] = infrastructure.Artifacts{Runner: test.path}
			_, err := requestInfrastructurePreparation(t.Context(), infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn}, owner, effects)
			if !errors.Is(err, test.want) || len(effects.calls) != 1 || effects.calls[0] != "read:1" {
				t.Fatalf("unproven helper absence released the owner: calls=%v err=%v", effects.calls, err)
			}
		})
	}
}

func TestManualMigrationReplacementPreflightPreservesServingRole(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	available := canonicalTestExecutable(t)
	for _, test := range []struct {
		name    string
		origin  string
		runner  string
		expired bool
		want    error
	}{
		{"same-root nested", store.Root, available, false, infrastructure.ErrOwnerRequest},
		{"cross-root nested", filepath.Join(supervisorfixture.CanonicalTempDir(t), "other-root"), available, false, infrastructure.ErrOwnerRequest},
		{"missing B helper", "", filepath.Join(supervisorfixture.CanonicalTempDir(t), "missing-b"), false, os.ErrNotExist},
		{"expired budget", "", available, true, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			invocation := manualMigrationInvocation(t, test.origin, store.RootID)
			budget := time.Minute
			if test.expired {
				budget = -time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()
			contact := infrastructurePreparationContact{
				routing:   Routing{invocation: invocation, defaultRunner: test.runner},
				resources: &infrastructureRoutingResources{target: store},
				selection: infrastructureRoutingSelection{initial: infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn}},
			}
			before := supervisorfixture.SnapshotStore(t, store.Root)
			_, err := contact.ReleaseServing(ctx, manualMigrationStartup(1))
			if !errors.Is(err, test.want) {
				t.Fatalf("replacement preflight reached stop with invalid source/budget/helper: %v", err)
			}
			if after := supervisorfixture.SnapshotStore(t, store.Root); !maps.Equal(before, after) {
				t.Fatal("refused replacement changed the target")
			}
		})
	}
}

func manualMigrationInvocation(t *testing.T, root, rootID string) *Invocation {
	t.Helper()
	unsetAppInvocationOrigin(t)
	if root != "" {
		origin, err := infrastructure.NewOrigin(root, rootID)
		if err != nil {
			t.Fatal(err)
		}
		environment, err := infrastructure.WithOrigin([]string{}, origin)
		if err != nil {
			t.Fatal(err)
		}
		setAppInvocationOrigin(t, environment)
	}
	invocation := CaptureInvocation(t.Context())
	if invocation.err != nil {
		t.Fatal(invocation.err)
	}
	return invocation
}
