package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	store := newBareInspectionStore(t)
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
	invocation := captureInfrastructureInvocation(t.Context())
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
	routing := infrastructureOwnerRouting{invocation: invocation}
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
				if dispatchFailureCode(got, "", "") != dispatchFailureCode(fixture.cause, "", "") {
					t.Fatalf("diagnostic probe changed public failure guidance: %v", got)
				}
			})
		}
	}
	if err := resources.finishPreparation(nil, infrastructure.ErrOwnerBusy); err != nil {
		t.Fatalf("successful canonical establishment kept an obsolete status fault: %v", err)
	}
}

func TestInfrastructureRoutingProbeDiagnosticRetainsActualCleanupOwner(t *testing.T) {
	store := newBareInspectionStore(t)
	cause := &infrastructureRoutingCleanupError{resources: &infrastructureRoutingResources{target: store}, cause: os.ErrPermission}
	got := (&infrastructureRoutingResources{}).finishPreparation(cause, os.ErrNotExist)
	if !cleanupRetainsStore(got, store) || !errors.Is(got, os.ErrPermission) {
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
