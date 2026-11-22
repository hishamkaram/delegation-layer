package infrastructurehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
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
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func TestUpgradeCompletionObserverLetsExactDrainReachOwner(t *testing.T) {
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
	observed, terminal, err := observeUpgradeCompletion(t.Context(), pin, op)
	if err != nil || terminal || observed.OperationID != pueue.UpgradeOperationID(op) || observed.Phase != pueue.UpgradeDraining {
		t.Fatalf("exact draining completion was short-circuited before its owner: terminal=%t observed=%+v err=%v", terminal, observed, err)
	}
}

func TestExactCompletionAdvancesOnlyItsDrainAndPreservesMode(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mode          pueue.UpgradeMode
		advanceBefore bool
	}{
		{"reactive draining", pueue.UpgradeReactive, false},
		{"proactive draining", pueue.UpgradeOperationRequired, false},
		{"proactive awaiting installation", pueue.UpgradeOperationRequired, true},
	} {
		t.Run(tc.name, func(t *testing.T) { exerciseExactCompletionAuthorization(t, tc.mode, tc.advanceBefore) })
	}
}

func exerciseExactCompletionAuthorization(t *testing.T, mode pueue.UpgradeMode, advanceBefore bool) {
	t.Helper()
	store, original, owner := newOwnedPreparationProbe(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authorizations := 0
	options := pueue.Options{
		ObservationTimeout: 10 * time.Second, WaitForPendingClose: true, PrivateGenerationGuard: owner, DaemonWaitOwner: owner, DaemonStartGuard: owner,
		PrivateMigrationAuthorizer: func(context.Context, string, task.SupervisorRef, []pueue.Job) error { authorizations++; return nil },
	}
	preparation := infrastructurePreparationOwner{store: store, options: options, lookup: installation.ExecutableLookup{InitialExecutable: filepath.Join(owner.base, "missing-pueue")}}
	var operation pueue.UpgradeOperationID
	base := filepath.Dir(original.Binding().ConfigPath)
	before := supervisorfixture.CapturePairFaultWitness(t, base)
	err = pueue.WithExistingBootstrapLockContext(ctx, store.Root, func(locked context.Context) error {
		var beginErr error
		operation, beginErr = beginExactCompletionFixture(locked, pin, mode, advanceBefore, options)
		if beginErr != nil {
			return beginErr
		}
		observed, completeErr := preparation.upgradeLocked(locked, pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeComplete, OperationID: infrastructure.UpgradeOperationID(operation)})
		if completeErr == nil || observed.OperationID != operation || observed.Phase != pueue.UpgradeAuthorized {
			t.Errorf("completion did not authorize its exact drain before missing target refusal: %+v %v", observed, completeErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observed, found, err := pueue.ObservePrivateUpgrade(ctx, pin, operation)
	if err != nil || !found || observed.Mode != mode || observed.Phase != pueue.UpgradeAuthorized || authorizations != 1 {
		t.Fatalf("exact completion changed mode or skipped drain policy: %+v found=%t calls=%d err=%v", observed, found, authorizations, err)
	}
	assertExactCompletionAuthorization(t, base, mode, operation)
	after := supervisorfixture.CapturePairFaultWitness(t, base)
	// Only the exact drain/authorization record may change before the
	// unavailable target is refused. Pair, migration and Start stay fixed.
	delete(before, "bootstrap.pending")
	delete(after, "bootstrap.pending")
	if !maps.Equal(before, after) {
		t.Fatal("authorization changed pair, migration or Start evidence")
	}
}

func beginExactCompletionFixture(ctx context.Context, pin taskdir.RootPin, mode pueue.UpgradeMode, advanceBefore bool, options pueue.Options) (pueue.UpgradeOperationID, error) {
	begun, err := pueue.BeginPrivateUpgradeWithLock(ctx, pin, mode, options)
	if err != nil || !advanceBefore {
		return begun.OperationID, err
	}
	awaiting, err := pueue.AdvancePrivateUpgradeDrainWithLock(ctx, pin, begun.OperationID, options)
	if err == nil && awaiting.Phase != pueue.UpgradeAwaitingInstallation {
		err = errors.New("fixture did not reach awaiting installation")
	}
	return begun.OperationID, err
}

func assertExactCompletionAuthorization(t *testing.T, base string, mode pueue.UpgradeMode, operation pueue.UpgradeOperationID) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(base, "bootstrap.pending"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Drain struct {
			Authorization *struct {
				OperationID pueue.UpgradeOperationID `json:"operation_id"`
			} `json:"completion_authorization"`
		} `json:"drain_v1"`
	}
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if mode == pueue.UpgradeOperationRequired && (record.Drain.Authorization == nil || record.Drain.Authorization.OperationID != operation) {
		t.Fatal("proactive completion lost its explicit authorization")
	}
	if mode == pueue.UpgradeReactive && record.Drain.Authorization != nil {
		t.Fatal("reactive completion invented proactive authority")
	}
}

func TestCleanDrainIncompleteRejectsIndependentPendingAndFaults(t *testing.T) {
	pending := &pueue.PrivateUpgradeError{ReasonCode: "drain_incomplete", Err: pueue.ErrPrivateUpgradePending}
	for _, tc := range []struct {
		name  string
		cause error
		want  bool
	}{
		{"clean", pending, true},
		{"canceled", errors.Join(pending, context.Canceled), false},
		{"deadline", errors.Join(pending, context.DeadlineExceeded), false},
		{"cleanup", &retainedPreparationFixtureError{cause: pending}, false},
		{"unknown", errors.Join(pending, pueue.ErrUnknown), false},
		{"other pending reason", errors.Join(pending, &pueue.PrivateUpgradeError{ReasonCode: "predecessor_wait_pending", Err: pueue.ErrPrivateUpgradePending}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if cleanDrainIncomplete(tc.cause) != tc.want {
				t.Fatalf("drain advancement borrowed another error: %v", tc.cause)
			}
		})
	}
}

// These canonical retained-history inputs prove replay/selection only. They
// never claim actual daemon birth or closure; owner lifetime tests are separate.
func seedCompletedUpgradeAndCurrentDrain(t *testing.T, store *taskdir.Store, binding task.SupervisorRef) (taskdir.RootPin, infrastructure.UpgradeOperationID, infrastructure.UpgradeOperationID) {
	t.Helper()
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	a := seedUpgradeWaitPendingJournal(t, pin, binding, recordedUpgradeWaitPredecessor(t, binding))
	base := filepath.Dir(binding.ConfigPath)
	data, err := os.ReadFile(filepath.Join(base, "migration.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record["legacy_stopped"] = true
	generation, ok := record["upgrade_generation"].(map[string]any)
	if !ok {
		t.Fatal("missing fixture generation")
	}
	generation["accepted_start"], generation["completed"] = true, true
	generation["successor_boot_id"] = generation["legacy_boot_id"]
	generation["successor_pid"], generation["successor_process_start"] = self.PID(), self.StartToken()
	data, err = task.MarshalCanonical(map[string]any{"schema_version": task.SchemaVersion, "migrations": []any{record}})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(base, "migration-history.json"), data)
	if err = os.Remove(filepath.Join(base, "migration.json")); err != nil {
		t.Fatal(err)
	}
	b := infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("8", 32))
	data, err = task.MarshalCanonical(map[string]any{"schema_version": task.SchemaVersion, "drain_v1": map[string]any{"root": map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}, "predecessor": binding, "operation_id": b, "mode": "operation_required", "phase": "draining"}})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(base, "bootstrap.pending"), data)
	observed, found, err := pueue.ObservePrivateUpgrade(t.Context(), pin, a)
	if err != nil || !found || !observed.Completed {
		t.Fatalf("invalid retained fixture A: %+v %t %v", observed, found, err)
	}
	return pin, infrastructure.UpgradeOperationID(a), b
}

func TestCompletedExactUpgradeReplaysBeforeCurrentDrainAndRefresh(t *testing.T) {
	store, original, owner := newOwnedPreparationProbe(t)
	pin, a, b := seedCompletedUpgradeAndCurrentDrain(t, store, original.Binding())
	// Completed A must replay without consulting even unreadable migration
	// evidence belonging to the later current B.
	writeFixtureFile(t, filepath.Join(store.Root, ".supervisor", "migration.json"), []byte("unreadable current B migration"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	before := supervisorfixture.CapturePairFaultWitness(t, filepath.Dir(original.Binding().ConfigPath))
	preparation := infrastructurePreparationOwner{store: store, options: pueue.Options{PrivateGenerationGuard: owner, DaemonStartGuard: owner, DaemonWaitOwner: owner}}
	err = pueue.WithExistingBootstrapLockContext(ctx, store.Root, func(locked context.Context) error {
		observed, transitionErr := preparation.upgradeLocked(locked, pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeComplete, OperationID: a})
		if transitionErr != nil || !observed.Completed || observed.OperationID != pueue.UpgradeOperationID(a) {
			t.Errorf("retained completion advanced current drain: %+v %v", observed, transitionErr)
		}
		return transitionErr
	})
	if err != nil {
		t.Fatal(err)
	}
	// A stale selected owner must never even be contacted after completed A.
	stale := infrastructure.StartupReference{Sequence: 19, Epoch: strings.Repeat("c", 32), IntentSHA256: strings.Repeat("d", 64)}
	contact := infrastructurePreparationContact{resources: &infrastructureRoutingResources{reader: reader, target: store}, selection: infrastructureRoutingSelection{owner: stale}}
	outcome, err := contact.requestUpgrade(ctx, pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeComplete, OperationID: a}, true)
	if err != nil || outcome.Condition != infrastructure.UpgradeOK || !outcome.Report.Completed || outcome.Report.OperationID != a {
		t.Fatalf("completed A attempted owner refresh: %+v err=%v", outcome, err)
	}
	current, found, err := pueue.ObservePrivateUpgrade(ctx, pin, pueue.UpgradeOperationID(b))
	if err != nil || !found || current.Phase != pueue.UpgradeDraining {
		t.Fatalf("B changed: %+v %t %v", current, found, err)
	}
	supervisorfixture.AssertPairFaultWitness(t, filepath.Dir(original.Binding().ConfigPath), before)
}

// The canonical accepted-start records are fixture data, independently checked
// by production readers. The finite target below has an actual joined cmd.Wait.
// The authenticated Unix peer replies Busy so a forbidden retirement CONTACT
// fails without retiring a real resident or stranding a native daemon.
func TestUpgradeCompletionRefreshDoesNotContactAcceptedTargetOwner(t *testing.T) {
	exerciseUpgradeRefreshAcceptedTarget(t, true)
}

func TestUpgradeCurrentRefreshDoesNotContactAcceptedTargetOwner(t *testing.T) {
	exerciseUpgradeRefreshAcceptedTarget(t, false)
}

func exerciseUpgradeRefreshAcceptedTarget(t *testing.T, complete bool) {
	t.Helper()
	store, original, owner := newOwnedPreparationProbe(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected, contacts := newUpgradeRetirementStopProbe(t, ctx, store, pin, original.Binding())
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// Before accepting any target, prove that this exact live peer is reachable
	// and authenticated. Busy is fixture behavior, never native closure proof.
	baseline, baselineErr := infrastructure.StopOwnerServingAt(ctx, reader, selected)
	if baselineErr != nil || baseline.State != infrastructure.OwnerStopBusy || contacts.Load() != 1 {
		t.Fatalf("stop-boundary fixture was not reachable: %+v contacts=%d err=%v", baseline, contacts.Load(), baselineErr)
	}
	contacts.Store(0)
	target, birth, done := newAcceptedUpgradeTargetWait(t, owner, original.Binding())
	operation := seedAcceptedUpgradeTarget(t, pin, original.Binding(), target, birth)
	before := supervisorfixture.CapturePairFaultWitness(t, filepath.Dir(original.Binding().ConfigPath))
	var exact infrastructure.UpgradeOperationID
	if complete {
		exact = infrastructure.UpgradeOperationID(operation)
	}
	observed, terminal, releaseErr := releaseUpgradeOwnerRefreshServing(ctx, reader, pin, exact, selected)
	if !terminal {
		t.Error("accepted target refresh was not a terminal retirement refusal")
	}
	assertUpgradeAcceptedTargetRefusal(t, observed, releaseErr, operation)
	if contacts.Load() != 0 {
		t.Errorf("accepted target's selected owner received forbidden retirement contact: contacts=%d phase=%s completed=%t terminal=%t err=%v", contacts.Load(), observed.Phase, observed.Completed, terminal, releaseErr)
	}
	select {
	case <-done:
		t.Error("refresh ended the actual target before its cooperative input was closed")
	default:
	}
	supervisorfixture.AssertPairFaultWitness(t, filepath.Dir(original.Binding().ConfigPath), before)
}

func assertUpgradeAcceptedTargetRefusal(t *testing.T, observed pueue.PrivateUpgradeObservation, releaseErr error, operation pueue.UpgradeOperationID) {
	t.Helper()
	var pending *pueue.PrivateUpgradeError
	if !errors.Is(releaseErr, pueue.ErrPrivateUpgradePending) || !errors.As(releaseErr, &pending) || pending.ReasonCode != UpgradeOwnerRefreshSuccessorStartAccepted || observed.OperationID != operation || observed.Completed {
		t.Errorf("accepted target refresh lost its exact refusal: observed=%+v err=%v", observed, releaseErr)
	}
}

func TestUpgradeRefreshCanonicalFaultDoesNotContactOrBecomeBusy(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprintf("complete=%t", complete), func(t *testing.T) {
			store, original, _ := newOwnedPreparationProbe(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			pin, err := store.PinRootContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			selected, contacts := newUpgradeRetirementStopProbe(t, ctx, store, pin, original.Binding())
			operation := seedUpgradeWaitPendingJournal(t, pin, original.Binding(), recordedUpgradeWaitPredecessor(t, original.Binding()))
			writeFixtureFile(t, filepath.Join(store.Root, ".supervisor", "migration.json"), []byte("corrupt canonical migration"))
			reader, err := taskdir.OpenReadOnlyStoreContext(ctx, store.Root, predicate.Default())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if closeErr := reader.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			})
			request := infrastructure.UpgradeRequest{Action: infrastructure.UpgradeStatus}
			if complete {
				request.Action, request.OperationID = infrastructure.UpgradeComplete, infrastructure.UpgradeOperationID(operation)
			}
			before := supervisorfixture.CapturePairFaultWitness(t, filepath.Dir(original.Binding().ConfigPath))
			contact := infrastructurePreparationContact{resources: &infrastructureRoutingResources{reader: reader, target: store}, selection: infrastructureRoutingSelection{owner: selected}}
			outcome, err := contact.requestUpgrade(ctx, pin, request, true)
			if err == nil || outcome.Condition != infrastructure.UpgradeFault || outcome.ReasonCode == "owner_refresh_busy" || outcome.Report.OperationID != infrastructure.UpgradeOperationID(operation) || contacts.Load() != 0 {
				t.Fatalf("canonical read fault reached retirement or lost operation: outcome=%+v contacts=%d err=%v", outcome, contacts.Load(), err)
			}
			supervisorfixture.AssertPairFaultWitness(t, filepath.Dir(original.Binding().ConfigPath), before)
		})
	}
}

func TestUpgradeRefreshBeforeAcceptedStartCanContactOwner(t *testing.T) {
	for _, state := range []string{"no operation", "current before Start", "exact before Start"} {
		t.Run(state, func(t *testing.T) {
			exerciseUpgradeRefreshBeforeStart(t, state)
		})
	}
}

func exerciseUpgradeRefreshBeforeStart(t *testing.T, state string) {
	t.Helper()
	store, original, _ := newOwnedPreparationProbe(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected, contacts := newUpgradeRetirementStopProbe(t, ctx, store, pin, original.Binding())
	var operation infrastructure.UpgradeOperationID
	if state != "no operation" {
		current := seedUpgradeWaitPendingJournal(t, pin, original.Binding(), recordedUpgradeWaitPredecessor(t, original.Binding()))
		if state == "exact before Start" {
			operation = infrastructure.UpgradeOperationID(current)
		}
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	_, terminal, err := releaseUpgradeOwnerRefreshServing(ctx, reader, pin, operation, selected)
	if terminal || !errors.Is(err, infrastructure.ErrOwnerBusy) || contacts.Load() != 1 {
		t.Fatalf("pre-Start refresh was incorrectly fenced: terminal=%t contacts=%d err=%v", terminal, contacts.Load(), err)
	}
}

func newAcceptedUpgradeTargetWait(t *testing.T, owner *preparationProbe, predecessor task.SupervisorRef) (task.SupervisorRef, lifetime.ProcessRef, <-chan struct{}) {
	t.Helper()
	target := predecessor
	target.DaemonExecutable = filepath.Join(owner.base, "bundle", "accepted-pueued")
	supervisorfixture.WriteExecutable(t, target.DaemonExecutable, "#!/bin/sh\nexec /bin/cat > /dev/null\n")
	input, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(target.DaemonExecutable, "-c", target.ConfigPath)
	command.Stdin, command.Stdout, command.Stderr = input, io.Discard, io.Discard
	done := make(chan struct{})
	var started bool
	var waitErr error
	// Registered before Start: EOF cooperatively ends this exact child; its
	// sole Wait joins before preparationProbe closes/removes any borrowed root.
	t.Cleanup(func() {
		if closeErr := release.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := input.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			t.Error(closeErr)
		}
		if started {
			<-done
			if waitErr != nil {
				t.Error(waitErr)
			}
		}
	})
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	started = true
	go func() {
		waitErr = command.Wait()
		close(done)
	}()
	if err = input.Close(); err != nil {
		t.Fatal(err)
	}
	if err = owner.RegisterDaemonWait(owner.root, command.Process.Pid, done); err != nil {
		t.Fatal(err)
	}
	birth, err := lifetime.ObserveProcess(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return target, birth, done
}

func seedAcceptedUpgradeTarget(t *testing.T, pin taskdir.RootPin, predecessor, target task.SupervisorRef, birth lifetime.ProcessRef) pueue.UpgradeOperationID {
	t.Helper()
	operation := seedUpgradeWaitPendingJournal(t, pin, predecessor, recordedUpgradeWaitPredecessor(t, predecessor))
	base := filepath.Dir(predecessor.ConfigPath)
	for _, name := range []string{"bootstrap.pending", "migration.json"} {
		var record map[string]any
		if err := json.Unmarshal(readPreparationProbeFile(t, filepath.Join(base, name)), &record); err != nil {
			t.Fatal(err)
		}
		if name == "bootstrap.pending" {
			drain, ok := record["drain_v1"].(map[string]any)
			if !ok {
				t.Fatal("missing canonical fixture Drain")
			}
			drain["selected_pair"] = map[string]any{"client_executable": target.ClientExecutable, "daemon_executable": target.DaemonExecutable}
		} else {
			generation, ok := record["upgrade_generation"].(map[string]any)
			if !ok {
				t.Fatal("missing canonical fixture migration generation")
			}
			successor, ok := record["successor"].(map[string]any)
			if !ok {
				t.Fatal("missing canonical fixture successor")
			}
			successor["executable"] = target.DaemonExecutable
			record["legacy_stopped"] = true
			generation["target"], generation["accepted_start"] = target, true
			generation["successor_pid"], generation["successor_process_start"] = birth.PID(), birth.StartToken()
			generation["successor_boot_id"] = generation["legacy_boot_id"]
		}
		data, err := task.MarshalCanonical(record)
		if err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(base, name), data)
	}
	observed, found, err := pueue.ObservePrivateUpgrade(t.Context(), pin, operation)
	if err != nil || !found || observed.Completed || observed.Phase != pueue.UpgradeMigrating {
		t.Fatalf("accepted-target fixture did not validate through canonical reader: %+v found=%t err=%v", observed, found, err)
	}
	if _, found, err = pueue.ObservePrivateMigrationPhase(pin.Root()); err != nil || !found {
		t.Fatalf("accepted-target migration fixture did not validate: found=%t err=%v", found, err)
	}
	return operation
}

func newUpgradeRetirementStopProbe(t *testing.T, ctx context.Context, store *taskdir.Store, pin taskdir.RootPin, binding task.SupervisorRef) (infrastructure.StartupReference, *atomic.Int32) {
	t.Helper()
	selected := infrastructure.LaunchContext{Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS, SupervisorExecutable: binding.ClientExecutable, ControlEnvironment: []string{}, MaintenanceEnvironment: []string{}}
	reference, err := infrastructure.StageLaunchContext(ctx, store, selected)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	intent := infrastructure.StartupIntent{Protocol: infrastructure.RecordProtocolVersion, Sequence: 1, Epoch: strings.Repeat("a", 32), Inputs: infrastructure.StartupInputs{Kind: infrastructure.StartupRootOwner, Root: pin.Root(), RootID: pin.RootID(), BootID: boot, RootDevice: pin.Device(), RootInode: pin.Inode(), Artifacts: infrastructure.Artifacts{Runner: canonicalTestExecutable(t)}, Context: reference}, AcceptedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
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
		t.Fatalf("fixture owner lease not acquired: acquired=%t err=%v", acquired, err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	publishUpgradeStopPeer(t, ctx, store, intent, startup, endpoint)
	return startup, contacts
}

func publishUpgradeStopPeer(t *testing.T, ctx context.Context, store *taskdir.Store, intent infrastructure.StartupIntent, startup infrastructure.StartupReference, endpoint infrastructure.EndpointIdentity) {
	t.Helper()
	control, err := store.OpenInfrastructureContext(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	assumption := infrastructure.StartupAssumption{Startup: startup, Process: infrastructure.ProcessIdentity{PID: self.PID(), StartToken: self.StartToken()}, UID: os.Geteuid(), AssumedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	digest, err := assumption.Digest(intent)
	if err != nil {
		t.Fatal(err)
	}
	publication := infrastructure.OwnerPublication{Protocol: infrastructure.RecordProtocolVersion, Startup: startup, AssumptionSHA256: digest, Endpoint: endpoint}
	if err = publication.Validate(intent, assumption); err != nil {
		t.Fatal(err)
	}
	for name, record := range map[string]any{"intent-00000000000000000001.json": intent, "assumption-00000000000000000001.json": assumption, "publication-00000000000000000001.json": publication} {
		if _, err = control.PutContext(ctx, name, record); err != nil {
			t.Fatal(err)
		}
	}
}

func serveUpgradeStopPeer(t *testing.T, ctx context.Context, pin taskdir.RootPin, startup infrastructure.StartupReference, contacts *atomic.Int32) infrastructure.EndpointIdentity {
	t.Helper()
	path := filepath.Join(pin.Root(), "0000000000001")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	joined := make(chan struct{})
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
		<-joined
	})
	go func() {
		defer close(joined)
		for {
			connection, acceptErr := listener.AcceptUnix()
			if errors.Is(acceptErr, net.ErrClosed) {
				return
			}
			if acceptErr != nil {
				t.Error(acceptErr)
				return
			}
			if handleErr := replyUpgradeStopBusy(ctx, connection, startup, pin.RootID(), contacts); handleErr != nil {
				t.Error(handleErr)
			}
		}
	}()
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	var socket unix.Stat_t
	if err = unix.Lstat(path, &socket); err != nil {
		t.Fatal(err)
	}
	return infrastructure.EndpointIdentity{Path: path, DirectoryDev: pin.Device(), DirectoryInode: pin.Inode(), SocketDev: uint64(socket.Dev), SocketInode: uint64(socket.Ino)}
}

func replyUpgradeStopBusy(ctx context.Context, connection *net.UnixConn, startup infrastructure.StartupReference, rootID string, contacts *atomic.Int32) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return err
	}
	var request struct {
		StopProtocol int                             `json:"stop_protocol"`
		RootID       string                          `json:"root_id"`
		Owner        infrastructure.StartupReference `json:"owner"`
		Publication  string                          `json:"publication_sha256"`
	}
	if err := json.NewDecoder(io.LimitReader(connection, task.MaxControlRecordSize)).Decode(&request); err != nil {
		return err
	}
	if request.StopProtocol != 1 || request.RootID != rootID || request.Owner != startup {
		return fmt.Errorf("unexpected retirement subject: %+v", request)
	}
	contacts.Add(1)
	return json.NewEncoder(connection).Encode(struct {
		StopProtocol int                             `json:"stop_protocol"`
		RootID       string                          `json:"root_id"`
		Owner        infrastructure.StartupReference `json:"owner"`
		Publication  string                          `json:"publication_sha256"`
		Code         string                          `json:"code"`
	}{1, rootID, startup, request.Publication, "busy"})
}
