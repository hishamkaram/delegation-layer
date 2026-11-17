package infrastructurehost

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestUpgradeOutcomeDoesNotHideIndependentOrPostMutationFault(t *testing.T) {
	for _, fixture := range []struct {
		name      string
		cause     error
		condition infrastructure.UpgradeCondition
		fault     error
	}{
		{"handled pending", &pueue.PrivateUpgradeError{ReasonCode: "predecessor_wait_pending", Err: pueue.ErrPrivateUpgradePending}, infrastructure.UpgradePending, nil},
		{"dispatched endpoint handoff", &pueue.PrivateUpgradeError{ReasonCode: "handoff_in_progress", Err: errors.Join(pueue.ErrPrivateUpgradePending, pueue.ErrUnknown)}, infrastructure.UpgradePending, nil},
		{"handoff with independent missing file", errors.Join(&pueue.PrivateUpgradeError{ReasonCode: "handoff_in_progress", Err: errors.Join(pueue.ErrPrivateUpgradePending, pueue.ErrUnknown)}, os.ErrNotExist), infrastructure.UpgradeFault, os.ErrNotExist},
		{"pending with storage fault", errors.Join(pueue.ErrPrivateUpgradePending, os.ErrPermission), infrastructure.UpgradeFault, os.ErrPermission},
		{"pending with retained owner", &retainedPreparationFixtureError{cause: pueue.ErrPrivateUpgradePending}, infrastructure.UpgradeFault, pueue.ErrPrivateUpgradePending},
		{"pending after cancelled barrier", errors.Join(pueue.ErrPrivateUpgradePending, context.Canceled), infrastructure.UpgradeFault, context.Canceled},
		{"pending after deadline barrier", errors.Join(pueue.ErrPrivateUpgradePending, context.DeadlineExceeded), infrastructure.UpgradeFault, context.DeadlineExceeded},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			outcome, err := upgradeOutcome(pueue.PrivateUpgradeObservation{}, fixture.cause)
			if outcome.Condition != fixture.condition || fixture.fault == nil && err != nil || fixture.fault != nil && !errors.Is(err, fixture.fault) {
				t.Fatalf("condition=%s err=%v", outcome.Condition, err)
			}
		})
	}
}

func TestUpgradeContactPendingKeepsTransportAndCleanupFaults(t *testing.T) {
	outcome := infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradePending, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPending}}
	for _, fixture := range []struct {
		cause   error
		handled bool
	}{
		{infrastructure.ErrOwnerPending, true},
		{errors.Join(infrastructure.ErrOwnerPending, os.ErrPermission), false},
		{errors.Join(infrastructure.ErrOwnerPending, context.DeadlineExceeded), false},
		{&retainedPreparationFixtureError{cause: infrastructure.ErrOwnerPending}, false},
	} {
		observed, err := normalizeUpgradeContact(outcome, fixture.cause)
		if observed != outcome || fixture.handled && err != nil || !fixture.handled && !errors.Is(err, fixture.cause) {
			t.Fatalf("pending normalized independent fault: outcome=%+v err=%v", observed, err)
		}
	}
}

func TestUpgradeOrdinaryCurrentEnsureDoesNotCreateOperationOrStart(t *testing.T) {
	store, original, owner := newOwnedPreparationProbe(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	selected := infrastructure.LaunchContext{Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS, SupervisorExecutable: original.Binding().ClientExecutable, ControlEnvironment: pueue.DefaultEnvironment(), MaintenanceEnvironment: []string{}}
	digest, err := selected.Digest()
	if err != nil {
		t.Fatal(err)
	}
	observation, err := infrastructure.NewPreparationObservation(owner, infrastructure.LaunchContextReference{SHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	options := pueue.Options{ObservationTimeout: 10 * time.Second, WaitForPendingClose: true, PrivateGenerationGuard: owner, DaemonWaitOwner: owner, DaemonStartGuard: infrastructurePreparationGuard(func(ctx context.Context, root string, start func() error) error {
		starts.Add(1)
		return owner.GuardDaemonStart(ctx, root, start)
	})}
	if err = supervisor.EnsureAdmissionSchedulingContext(ctx, store.Root, original); err != nil {
		t.Fatal(err)
	}
	preparation := infrastructurePreparationOwner{store: store, options: options, observation: observation, lookup: installation.ExecutableLookup{InitialExecutable: selected.SupervisorExecutable}}
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before := supervisorfixture.CapturePairFaultWitness(t, filepath.Dir(original.Binding().ConfigPath))
	for range 2 {
		outcome, prepareErr := preparation.performUpgrade(ctx, pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeEnsure}, selected)
		if prepareErr != nil || outcome.Condition != infrastructure.UpgradeOK || outcome.Report != (infrastructure.UpgradeReport{}) {
			t.Fatalf("ordinary ensure became upgrade: %+v %v", outcome, prepareErr)
		}
	}
	if starts.Load() != 0 || observation.Disposition().Kind != infrastructure.DispositionWarmReuse {
		t.Fatalf("ordinary same-pair ensure started daemon: count=%d disposition=%+v", starts.Load(), observation.Disposition())
	}
	supervisorfixture.AssertPairFaultWitness(t, filepath.Dir(original.Binding().ConfigPath), before)
	if ordinary, observeErr := ordinaryInstalledEnsure(t.Context(), pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradePrepare}, selected); observeErr != nil || ordinary {
		t.Fatalf("proactive preparation selected ordinary path: %t %v", ordinary, observeErr)
	}
}

func TestUpgradeRoutingEarlyCompleteIsPassiveBeforeRunnerAndStaging(t *testing.T) {
	f := newInfrastructureSavedBindingFixture(t, infrastructure.SavedBindingTaskMetadata, true)
	pin, err := f.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(f.store.Root, ".supervisor")
	if err = os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	op := infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("9", 32))
	data, err := task.MarshalCanonical(map[string]any{"schema_version": task.SchemaVersion, "drain_v1": map[string]any{"root": map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}, "predecessor": f.binding, "operation_id": op, "mode": "operation_required", "phase": "draining"}})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(base, "bootstrap.pending"), data)
	unsetAppInvocationOrigin(t)
	routing := Routing{invocation: CaptureInvocation(t.Context()), defaultRunner: "/must-not-open/delegate-run"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	before := supervisorfixture.SnapshotStore(t, f.store.Root)
	outcome, err := routing.UpgradeOwn(ctx, f.store, infrastructure.LaunchContext{}, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeComplete, OperationID: op}, true)
	if err != nil || outcome.Condition != infrastructure.UpgradePending || outcome.Report.OperationID != op {
		t.Fatalf("early complete reached mutable preparation: %+v %v", outcome, err)
	}
	wrong := infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("8", 32))
	outcome, err = routing.UpgradeOwn(ctx, f.store, infrastructure.LaunchContext{}, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeComplete, OperationID: wrong}, true)
	if err != nil || outcome.Condition != infrastructure.UpgradeRefused || outcome.Report != (infrastructure.UpgradeReport{}) {
		t.Fatalf("wrong OP reached preparation: %+v %v", outcome, err)
	}
	if !maps.Equal(before, supervisorfixture.SnapshotStore(t, f.store.Root)) {
		t.Fatal("wrong/early completion staged or changed root state")
	}
}
