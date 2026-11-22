package infrastructurehost

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type missingHelperRefreshFixture struct {
	store    *taskdir.Store
	client   *pueue.Client
	wait     *preparationProbe
	pin      taskdir.RootPin
	contact  infrastructurePreparationContact
	startup  infrastructure.StartupReference
	contacts *atomic.Int32
}

// Startup artifacts are immutable inputs, including the deliberately absent
// old runner. The installed replacement is resolved in place. The reused peer
// authenticates Stop and answers Busy; it never retires a real resident.
func newMissingHelperRefreshFixture(t *testing.T, kind infrastructure.InitialOperationKind) missingHelperRefreshFixture {
	t.Helper()
	store, client, wait := newOwnedPreparationProbe(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected := infrastructure.LaunchContext{Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS, SupervisorExecutable: client.Binding().ClientExecutable, ControlEnvironment: []string{}, MaintenanceEnvironment: []string{}}
	reference, err := infrastructure.StageLaunchContext(ctx, store, selected)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	intent := infrastructure.StartupIntent{Protocol: infrastructure.RecordProtocolVersion, Sequence: 1, Epoch: strings.Repeat("a", 32), Inputs: infrastructure.StartupInputs{Kind: infrastructure.StartupRootOwner, Root: pin.Root(), RootID: pin.RootID(), BootID: boot, RootDevice: pin.Device(), RootInode: pin.Inode(), Artifacts: infrastructure.Artifacts{Runner: filepath.Join(wait.base, "removed-installation", "delegate-run")}, Context: reference}, AcceptedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	intent.InputsSHA256, err = intent.Inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	startup, err := infrastructure.ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	contacts := new(atomic.Int32)
	endpoint := serveUpgradeStopPeer(t, ctx, pin, startup, contacts)
	lease, acquired, err := infrastructure.TryAcquireLease(ctx, filepath.Join(store.Root, ".infrastructure-owner.lock"))
	if err != nil || !acquired {
		t.Fatalf("missing-helper owner lease: acquired=%t err=%v", acquired, err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	publishUpgradeStopPeer(t, ctx, store, intent, startup, endpoint)
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	invocation := CaptureInvocation(ctx)
	if invocation.err != nil {
		t.Fatal(invocation.err)
	}
	contact := infrastructurePreparationContact{
		routing:   Routing{invocation: invocation, defaultRunner: canonicalTestExecutable(t)},
		resources: &infrastructureRoutingResources{target: store, reader: reader},
		selection: infrastructureRoutingSelection{initial: infrastructure.InitialOperation{Kind: kind}, context: selected, reference: reference, owner: startup},
	}
	return missingHelperRefreshFixture{store: store, client: client, wait: wait, pin: pin, contact: contact, startup: startup, contacts: contacts}
}

type missingHelperRefreshEffects struct {
	infrastructurePreparationContact
	establishes int
	requests    int
}

func (e *missingHelperRefreshEffects) EstablishReplacement(context.Context) (infrastructure.EstablishedOwner, error) {
	e.establishes++
	return infrastructure.EstablishedOwner{}, os.ErrInvalid
}

func (e *missingHelperRefreshEffects) Request(context.Context, infrastructure.EstablishedOwner) (infrastructure.PreparationOutcome, error) {
	e.requests++
	return infrastructure.PreparationOutcome{}, os.ErrInvalid
}

func TestMissingHelperRefreshRejectsAcceptedStart(t *testing.T) {
	for _, test := range []struct {
		name    string
		kind    infrastructure.InitialOperationKind
		unknown bool
		routing bool
	}{
		{name: "ordinary ensure"},
		{name: "explicit ensure unknown birth", kind: infrastructure.InitialEnsureOwn, unknown: true},
		{name: "manual migrate", kind: infrastructure.InitialMigrateOwn},
		{name: "manual routing occupied lease", kind: infrastructure.InitialMigrateOwn, routing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			exerciseMissingHelperAcceptedStart(t, test.kind, test.unknown, test.routing)
		})
	}
}

func exerciseMissingHelperAcceptedStart(t *testing.T, kind infrastructure.InitialOperationKind, unknown, routing bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	f := newMissingHelperRefreshFixture(t, kind)
	if routing {
		primeMissingHelperRoutingContact(t, ctx, f)
	}
	baseline, err := infrastructure.StopOwnerServingAt(ctx, f.contact.resources.reader, f.startup)
	if err != nil || baseline.State != infrastructure.OwnerStopBusy || f.contacts.Load() != 1 {
		t.Fatalf("authenticated Busy peer unreachable: result=%+v contacts=%d err=%v", baseline, f.contacts.Load(), err)
	}
	f.contacts.Store(0)
	target, birth, done := newAcceptedUpgradeTargetWait(t, f.wait, f.client.Binding())
	operation := seedAcceptedUpgradeTarget(t, f.pin, f.client.Binding(), target, birth)
	if unknown {
		removeMissingHelperSuccessorBirth(t, f.client.Binding().ConfigPath)
	}
	before := supervisorfixture.CapturePairFaultWitness(t, filepath.Dir(f.client.Binding().ConfigPath))
	storeBefore := missingHelperInfrastructureWitness(t, f.store.Root)
	effects := &missingHelperRefreshEffects{infrastructurePreparationContact: f.contact}
	if routing {
		_, _, err = f.contact.routing.MigrateOwn(ctx, f.store, f.contact.selection.context)
	} else {
		_, err = requestInfrastructurePreparation(ctx, f.contact.selection.initial, infrastructure.EstablishedOwner{Owner: f.startup}, effects)
	}
	if f.contacts.Load() != 0 || effects.establishes != 0 || effects.requests != 0 {
		t.Errorf("accepted Start reached forbidden work: Stop contacts=%d establishes=%d requests=%d", f.contacts.Load(), effects.establishes, effects.requests)
	}
	assertMissingHelperAcceptedRefusal(t, ctx, f, operation, err)
	select {
	case <-done:
		t.Error("refresh ended the actual target before cooperative EOF")
	default:
	}
	supervisorfixture.AssertPairFaultWitness(t, filepath.Dir(f.client.Binding().ConfigPath), before)
	if !maps.Equal(storeBefore, missingHelperInfrastructureWitness(t, f.store.Root)) {
		t.Error("refusal changed canonical store witnesses or established another owner")
	}
}

func primeMissingHelperRoutingContact(t *testing.T, ctx context.Context, f missingHelperRefreshFixture) {
	t.Helper()
	selection := f.contact.selection
	selection.owner = infrastructure.StartupReference{}
	established, err := f.contact.routing.establish(ctx, f.contact.resources, f.pin, selection, newInfrastructureEstablishmentHost(f.contact.routing.defaultRunner, selection.initial))
	if err != nil || established.Owner != f.startup || established.InitialResultFor != (infrastructure.StartupReference{}) {
		t.Fatalf("occupied lease did not normalize to the live contact: established=%+v err=%v", established, err)
	}
}

func assertMissingHelperAcceptedRefusal(t *testing.T, ctx context.Context, f missingHelperRefreshFixture, operation pueue.UpgradeOperationID, err error) {
	t.Helper()
	var pending *pueue.PrivateUpgradeError
	if !errors.Is(err, pueue.ErrPrivateUpgradePending) || !errors.As(err, &pending) || pending.ReasonCode != UpgradeOwnerRefreshSuccessorStartAccepted {
		t.Errorf("missing-helper refresh lost typed accepted-Start refusal: contacts=%d err=%v", f.contacts.Load(), err)
	}
	observed, found, observeErr := pueue.ObservePrivateUpgrade(ctx, f.pin, operation)
	if observeErr != nil || !found || observed.OperationID != operation || observed.Completed {
		t.Errorf("refusal lost exact retained A: observed=%+v found=%t err=%v", observed, found, observeErr)
	}
	assertMissingHelperBootstrapUnlocked(t, ctx, f.store.Root)
}

func missingHelperInfrastructureWitness(t *testing.T, root string) map[string]string {
	t.Helper()
	base := filepath.Join(root, "infrastructure")
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	witness := make(map[string]string, len(entries))
	for _, entry := range entries {
		witness[entry.Name()] = string(readPreparationProbeFile(t, filepath.Join(base, entry.Name())))
	}
	return witness
}

func TestMissingHelperRefreshBeforeStartReturnsActualStopResult(t *testing.T) {
	for _, state := range []string{"no upgrade", "before Start", "legacy without bootstrap lock"} {
		t.Run(state, func(t *testing.T) {
			exerciseMissingHelperBeforeStart(t, state)
		})
	}
}

func exerciseMissingHelperBeforeStart(t *testing.T, state string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	f := newMissingHelperRefreshFixture(t, infrastructure.InitialMigrateOwn)
	if state == "before Start" {
		seedUpgradeWaitPendingJournal(t, f.pin, f.client.Binding(), recordedUpgradeWaitPredecessor(t, f.client.Binding()))
	}
	baseline, err := infrastructure.StopOwnerServingAt(ctx, f.contact.resources.reader, f.startup)
	if err != nil || baseline.State != infrastructure.OwnerStopBusy || baseline.ProcessClosure != infrastructure.OwnerProcessPending || f.contacts.Load() != 1 {
		t.Fatalf("invalid baseline Stop result: result=%+v contacts=%d err=%v", baseline, f.contacts.Load(), err)
	}
	f.contacts.Store(0)
	lockPath := filepath.Join(f.store.Root, ".supervisor", "bootstrap.lock")
	if state == "legacy without bootstrap lock" {
		if err = os.Remove(lockPath); err != nil {
			t.Fatal(err)
		}
	}
	released, err := f.contact.ReleaseServing(ctx, f.startup)
	if err != nil || released != baseline || f.contacts.Load() != 1 {
		t.Fatalf("eligible refresh lost actual Stop result: result=%+v want=%+v contacts=%d err=%v", released, baseline, f.contacts.Load(), err)
	}
	// Establishment belongs after ReleaseServing returns. A separate EX
	// must be acquirable here; Busy proves no process closure or release.
	assertMissingHelperBootstrapUnlocked(t, ctx, f.store.Root)
}

func assertMissingHelperBootstrapUnlocked(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	unlocked, release := context.WithTimeout(ctx, time.Second)
	defer release()
	if err := pueue.WithExistingBootstrapLockContext(unlocked, root, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("ReleaseServing retained EX or failed legacy lock provisioning: %v", err)
	}
}

func TestMissingHelperRefreshPreservesStopFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	f := newMissingHelperRefreshFixture(t, infrastructure.InitialMigrateOwn)
	changed := f.startup
	changed.Sequence++
	baseline, baselineErr := infrastructure.StopOwnerServingAt(ctx, f.contact.resources.reader, changed)
	if !errors.Is(baselineErr, task.ErrLockBusy) {
		t.Fatalf("changed-owner baseline did not refuse: %v", baselineErr)
	}
	result, err := f.contact.ReleaseServing(ctx, changed)
	if !errors.Is(err, task.ErrLockBusy) || result != baseline || f.contacts.Load() != 0 {
		t.Fatalf("refresh changed real Stop fault/result: result=%+v want=%+v contacts=%d err=%v", result, baseline, f.contacts.Load(), err)
	}
	assertMissingHelperBootstrapUnlocked(t, ctx, f.store.Root)
}

func TestMissingHelperRefreshContendedBootstrapEXExpiresBeforeStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	f := newMissingHelperRefreshFixture(t, infrastructure.InitialMigrateOwn)
	before := supervisorfixture.CapturePairFaultWitness(t, filepath.Dir(f.client.Binding().ConfigPath))
	// This distinct acquisition owns the actual lock. The real refresh must
	// wait for it and honor its finite deadline without contacting the peer.
	err := pueue.WithExistingBootstrapLockContext(ctx, f.store.Root, func(context.Context) error {
		waiting, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		defer stop()
		result, releaseErr := f.contact.ReleaseServing(waiting, f.startup)
		if !errors.Is(releaseErr, context.DeadlineExceeded) || result != (infrastructure.OwnerStopResult{}) || f.contacts.Load() != 0 {
			t.Errorf("refresh bypassed contended EX: result=%+v contacts=%d err=%v", result, f.contacts.Load(), releaseErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	supervisorfixture.AssertPairFaultWitness(t, filepath.Dir(f.client.Binding().ConfigPath), before)
}

func TestMissingHelperRefreshFaultsBeforeStop(t *testing.T) {
	for _, fault := range []string{"corrupt canonical migration", "different reader root", "canceled context"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			f := newMissingHelperRefreshFixture(t, infrastructure.InitialMigrateOwn)
			var want error
			switch fault {
			case "corrupt canonical migration":
				seedUpgradeWaitPendingJournal(t, f.pin, f.client.Binding(), recordedUpgradeWaitPredecessor(t, f.client.Binding()))
				writeFixtureFile(t, filepath.Join(f.store.Root, ".supervisor", "migration.json"), []byte("corrupt canonical migration"))
			case "different reader root":
				other := supervisorfixture.NewInspectionStore(t)
				reader, err := taskdir.OpenReadOnlyStoreContext(ctx, other.Root, predicate.Default())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if closeErr := reader.Close(); closeErr != nil {
						t.Error(closeErr)
					}
				})
				f.contact.resources.reader = reader
				want = infrastructure.ErrIdentity
			case "canceled context":
				cancel()
				want = context.Canceled
			}
			before := supervisorfixture.CapturePairFaultWitness(t, filepath.Dir(f.client.Binding().ConfigPath))
			result, err := f.contact.ReleaseServing(ctx, f.startup)
			if err == nil || want != nil && !errors.Is(err, want) || errors.Is(err, infrastructure.ErrOwnerBusy) || errors.Is(err, pueue.ErrPrivateUpgradePending) || result != (infrastructure.OwnerStopResult{}) || f.contacts.Load() != 0 {
				t.Fatalf("fault was hidden or reached Stop: result=%+v contacts=%d err=%v", result, f.contacts.Load(), err)
			}
			supervisorfixture.AssertPairFaultWitness(t, filepath.Dir(f.client.Binding().ConfigPath), before)
		})
	}
}

func removeMissingHelperSuccessorBirth(t *testing.T, config string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(config), "migration.json")
	var record map[string]any
	if err := json.Unmarshal(readPreparationProbeFile(t, path), &record); err != nil {
		t.Fatal(err)
	}
	generation, ok := record["upgrade_generation"].(map[string]any)
	if !ok {
		t.Fatal("missing canonical fixture upgrade generation")
	}
	for _, key := range []string{"successor_pid", "successor_process_start", "successor_boot_id"} {
		delete(generation, key)
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, path, data)
}
