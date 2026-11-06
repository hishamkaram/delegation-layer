package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

// taskMaintenanceWake is the durable wake an observer may advance.
type taskMaintenanceWake struct {
	Requested     uint64 `json:"requested"`
	Attempted     uint64 `json:"attempted"`
	FullRequested uint64 `json:"full_requested"`
}

// readTaskMaintenanceWake reads the private wake journal; an absent journal is
// the zero wake.
func readTaskMaintenanceWake(t *testing.T, root string) taskMaintenanceWake {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".supervisor", "queue-maintenance.json"))
	if errors.Is(err, os.ErrNotExist) {
		return taskMaintenanceWake{}
	}
	if err != nil {
		t.Fatal(err)
	}
	var wake taskMaintenanceWake
	if err = json.Unmarshal(data, &wake); err != nil {
		t.Fatal(err)
	}
	return wake
}

// requireTaskMaintenanceFullWake requires one new runnable full generation.
func requireTaskMaintenanceFullWake(t *testing.T, root string, before taskMaintenanceWake) {
	t.Helper()
	after := readTaskMaintenanceWake(t, root)
	if after.Requested != before.Requested+1 || after.FullRequested != after.Requested || after.Requested <= after.Attempted {
		t.Fatalf("maintenance wake = %+v after %+v, want one new runnable full generation", after, before)
	}
}

func requireTaskMaintenanceWakeUnchanged(t *testing.T, root string, before taskMaintenanceWake, observer string) {
	t.Helper()
	if after := readTaskMaintenanceWake(t, root); after != before {
		t.Fatalf("%s requested maintenance: %+v, want %+v", observer, after, before)
	}
}

// requirePublicCustodyRequest requires a public observer to leave custody to
// the owner and persist one runnable full wake for it.
func requirePublicCustodyRequest(t *testing.T, root string, registration *task.LifetimeRegistrationRecord, before taskMaintenanceWake) {
	t.Helper()
	if !custodyTestPending(t, registration) {
		t.Fatal("the public path released custody")
	}
	requireTaskMaintenanceFullWake(t, root, before)
}

// runTaskCustodyOwner runs the elected owner's full custody phase with no
// captured queue rows.
func runTaskCustodyOwner(t *testing.T, root string) {
	t.Helper()
	if err := releasePrivateTerminalCustody(t.Context(), root, task.SupervisorRef{}, pueue.PrivateTerminalCustodyScope{Full: true}); err != nil {
		t.Fatal(err)
	}
}

// makePrivateSupervisorBase creates the private base a private root always has.
func makePrivateSupervisorBase(t *testing.T, root string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(root, ".supervisor"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func newPlacedTaskInStore(t *testing.T, store *taskdir.Store, id, config, cause string) (*taskdir.TaskDir, *task.LifetimeRegistrationRecord) {
	t.Helper()
	td, _ := lifecyclefixture.NewAppTaskInStoreWithSupervisorConfig(t, store, id, false, nil, config)
	t.Cleanup(func() {
		if err := td.Close(); err != nil {
			t.Error(err)
		}
	})
	registration := recordCustodyLifetime(t, td, newCustodyTestPlacement(t), cause)
	bindCustodyTestView(t, registration)
	return td, registration
}

// TestPrivateFullCustodyScopeReleasesTaskCustodyWithoutQueueRows: a full
// capture reconciles every task directory independently of queue rows. Closed
// custody is released, an open lifetime, an unplaced task and a partially
// created directory release nothing, and one task's missing pool is reported
// without skipping the others. A row-scoped capture touches no ordinary task.
func TestPrivateFullCustodyScopeReleasesTaskCustodyWithoutQueueRows(t *testing.T) {
	store, unplaced, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, unplaced)
	config := pueue.PrivateConfigPath(store.Root)
	_, rejected := newPlacedTaskInStore(t, store, strings.Repeat("d", 32), config, task.LifetimeTerminationCompleted)
	_, missing := newPlacedTaskInStore(t, store, strings.Repeat("e", 32), config, task.LifetimeTerminationBudget)
	_, open := newPlacedTaskInStore(t, store, strings.Repeat("f", 32), config, custodyLifetimeOpen)
	if err := os.Mkdir(filepath.Join(store.Root, "tasks", strings.Repeat("1", 32)), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := releasePrivateTerminalCustody(t.Context(), store.Root, task.SupervisorRef{}, pueue.PrivateTerminalCustodyScope{}); err != nil {
		t.Fatal(err)
	}
	if !custodyTestPending(t, rejected) {
		t.Fatal("a row-scoped capture released ordinary task custody")
	}
	restore := moveCustodyTestPool(t, missing.CustodyPlacement)
	err := releasePrivateTerminalCustody(t.Context(), store.Root, task.SupervisorRef{}, pueue.PrivateTerminalCustodyScope{Full: true})
	restore()
	if err == nil || !strings.Contains(err.Error(), missing.TaskID) {
		t.Fatalf("full custody fault = %v, want the missing pool's task reported", err)
	}
	if custodyTestPending(t, rejected) {
		t.Fatal("one task's pool fault skipped an independent closed task")
	}
	if !custodyTestPending(t, missing) || !custodyTestPending(t, open) {
		t.Fatal("a missing pool or an open lifetime was released")
	}
	runTaskCustodyOwner(t, store.Root)
	if custodyTestPending(t, missing) || !custodyTestPending(t, open) {
		t.Fatal("the next full pass did not release only the restored closed pool")
	}
}

// TestPrivateTaskObservationRefusesUnavailableAndMalformedBindings: a saved
// private binding whose supervisor is unavailable never falls back to the
// external synchronous release; its proven remaining release persists a full
// wake. A malformed saved binding classifies nothing and releases nothing.
func TestPrivateTaskObservationRefusesUnavailableAndMalformedBindings(t *testing.T) {
	store, unplaced, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, unplaced)
	observer := taskMaintenanceObserver{root: store.Root}
	makePrivateSupervisorBase(t, store.Root)
	td, registration := newPlacedTaskInStore(t, store, strings.Repeat("d", 32), pueue.PrivateConfigPath(store.Root), task.LifetimeTerminationCompleted)
	before := readTaskMaintenanceWake(t, store.Root)
	if err := observer.observeTask(t.Context(), td); err != nil {
		t.Fatal(err)
	}
	if !custodyTestPending(t, registration) {
		t.Fatal("an unavailable private binding fell back to synchronous release")
	}
	requireTaskMaintenanceFullWake(t, store.Root, before)

	malformed, malformedRegistration := newPlacedTaskInStore(t, store, strings.Repeat("e", 32), "/tmp/app-pueue.yml", task.LifetimeTerminationCompleted)
	writeAppTestFile(t, filepath.Join(malformed.Dir, "meta.json"), []byte("{"))
	if err := observer.observeTask(t.Context(), malformed); err == nil {
		t.Fatal("a malformed saved binding was classified")
	}
	if !custodyTestPending(t, malformedRegistration) {
		t.Fatal("a malformed saved binding fell back to external release")
	}
}

// TestGuardianCustodyFaultPersistsPrivateFullWake: a private run's release
// fault persists a full wake before the guardian returns, on both the ordinary
// and the inspection return path, and never changes the guardian's result. The
// paths diverge by design: an ordinary private run hands its custody to one
// launched maintenance owner, while an inspection run keeps its direct release
// and launches nothing. An external run's fault creates no private queue state.
func TestGuardianCustodyFaultPersistsPrivateFullWake(t *testing.T) {
	for _, inspectionRun := range []bool{false, true} {
		for _, private := range []bool{false, true} {
			run := newGuardianCustodyRun(t, task.LifetimeTerminationCompleted, private)
			restore := moveCustodyTestPool(t, run.registration.CustodyPlacement)
			launches := &guardianOwnerLaunches{}
			run.complete(t, inspectionRun, launches.dependencies())
			restore()
			if !custodyTestPending(t, run.registration) {
				t.Fatal("a missing pool was treated as released")
			}
			if private {
				requireTaskMaintenanceFullWake(t, run.store.Root, taskMaintenanceWake{})
			} else if _, statErr := os.Lstat(filepath.Join(run.store.Root, ".supervisor")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("an external release fault created private queue state: %v", statErr)
			}
			if private && !inspectionRun {
				launches.requireLaunchedOnce(t, run.store.Root)
			} else {
				launches.requireNoLaunch(t)
			}
		}
	}
}

// TestGuardianDirectCustodyReleaseRemainsForInspectionAndExternalRuns: an
// inspection run and an external run release a closed lifetime's custody
// themselves, as before, and never request or launch a maintenance owner.
func TestGuardianDirectCustodyReleaseRemainsForInspectionAndExternalRuns(t *testing.T) {
	for _, path := range []struct {
		inspectionRun, private bool
	}{{true, true}, {false, false}, {true, false}} {
		run := newGuardianCustodyRun(t, task.LifetimeTerminationCompleted, path.private)
		launches := &guardianOwnerLaunches{}
		before := readTaskMaintenanceWake(t, run.store.Root)
		run.complete(t, path.inspectionRun, launches.dependencies())
		if custodyTestPending(t, run.registration) {
			t.Fatalf("%+v run did not release its closed custody directly", path)
		}
		requireTaskMaintenanceWakeUnchanged(t, run.store.Root, before, "a direct release")
		launches.requireNoLaunch(t)
	}
}

// writeFailedFullMaintenancePass records a full generation that an owner
// attempted and failed, so no generation remains runnable.
func writeFailedFullMaintenancePass(t *testing.T, root string, generation uint64) taskMaintenanceWake {
	t.Helper()
	data, err := task.MarshalCanonical(struct {
		SchemaVersion int    `json:"schema_version"`
		Requested     uint64 `json:"requested"`
		Attempted     uint64 `json:"attempted"`
		Completed     uint64 `json:"completed"`
		FullRequested uint64 `json:"full_requested"`
		FullCompleted uint64 `json:"full_completed"`
	}{task.SchemaVersion, generation, generation, 0, generation, 0})
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, filepath.Join(root, ".supervisor", "queue-maintenance.json"), data)
	return readTaskMaintenanceWake(t, root)
}

// TestPrivateCollectRequestsMaintenanceForRetainedEndedRow: collect publishes
// the outcome of a private task whose exact ended row is still present. The
// public command keeps its result, sends no queue mutation, and turns the
// read-only exact match into a new runnable full generation even after a
// failed full pass, so the remaining row is not stranded.
func TestPrivateCollectRequestsMaintenanceForRetainedEndedRow(t *testing.T) {
	fixture := newRebootFixture(t, sealedRebootSetup())
	before := writeFailedFullMaintenancePass(t, fixture.root, 3)
	resetHistoricalSupervisorCommandLog(t, fixture.historicalSupervisorFixture)
	// The fixture predicate is in the default registry, as for any sealed task.
	result := collect(Arguments{Root: fixture.root, TaskID: fixture.req.TaskID}, storeDependencies{supervisorOptions: fixture.options})
	if result.code != 0 || result.response.Publication != task.PublicationCommitted.String() || result.response.Outcome == nil {
		t.Fatalf("collect = code %d err=%v %+v, want the committed outcome", result.code, result.err, result.response)
	}
	requireTaskMaintenanceFullWake(t, fixture.root, before)
	commands := string(readHistoricalSupervisorBytes(t, fixture.commandLog))
	for _, mutation := range []string{"remove", "kill", "shutdown", "add", "enqueue"} {
		if strings.Contains(commands, mutation) {
			t.Fatalf("public collect sent queue mutation %q: %s", mutation, commands)
		}
	}
}

// TestGuardianCustodyRecoveryOutlivesExhaustedReleaseContext: a release that
// exhausted or cancelled its context still leaves a durable full wake.
func TestGuardianCustodyRecoveryOutlivesExhaustedReleaseContext(t *testing.T) {
	exhausted := map[string]func() context.Context{
		"canceled": func() context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		},
		"deadline exceeded": func() context.Context {
			ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		},
	}
	for name, release := range exhausted {
		t.Run(name, func(t *testing.T) {
			// The private journal refuses symlinked ancestors, and TempDir is
			// not canonical where the temporary root is an alias (macOS /var).
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(parent, "state")
			if err = os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			makePrivateSupervisorBase(t, root)
			ctx := release()
			if ctx.Err() == nil {
				t.Fatal("fixture release context is still live")
			}
			if err = requestPrivateFullMaintenanceWake(ctx, root); err != nil {
				t.Errorf("recovery request after an exhausted release: %v", err)
			}
			requireTaskMaintenanceFullWake(t, root, taskMaintenanceWake{})
		})
	}
}

// TestPrivateObserverWakeOutlivesExhaustedObservationContext: a positive hint
// whose row observation exhausted or cancelled the observer's context still
// leaves a durable full wake.
func TestPrivateObserverWakeOutlivesExhaustedObservationContext(t *testing.T) {
	exhausted := map[string]func() context.Context{
		"canceled": func() context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		},
		"deadline exceeded": func() context.Context {
			ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		},
	}
	for name, observation := range exhausted {
		t.Run(name, func(t *testing.T) {
			// The private journal refuses symlinked ancestors, and TempDir is
			// not canonical where the temporary root is an alias (macOS /var).
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(parent, "state")
			if err = os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			makePrivateSupervisorBase(t, root)
			ctx := observation()
			if ctx.Err() == nil {
				t.Fatal("fixture observation context is still live")
			}
			if err = (taskMaintenanceObserver{root: root}).requestMaintenance(ctx, nil); err != nil {
				t.Errorf("maintenance request after an exhausted observation: %v", err)
			}
			requireTaskMaintenanceFullWake(t, root, taskMaintenanceWake{})
		})
	}
}

// handoffOwnerExecutable is the normalized maintenance executable a recorded
// launch receives; the recorder executes nothing.
const handoffOwnerExecutable = "/opt/delegate/bin/delegate-run"

// handoffCapturedEnvironment is a guardian's captured launch scope. Only its
// bounded control variables may reach the launched owner.
func handoffCapturedEnvironment() []string {
	return []string{"PATH=/captured/bin", "DELEGATE_HANDOFF_UNSCOPED=1", "HOME=/captured/home"}
}

type guardianOwnerLaunch struct {
	executable  string
	root        string
	environment []string
	bounded     bool
}

// guardianOwnerLaunches records each maintenance owner launch a guardian's
// post-run custody step requests and starts no process. before, when set,
// runs at the launch point; err is the launch result.
type guardianOwnerLaunches struct {
	before   func()
	err      error
	launches []guardianOwnerLaunch
}

func (l *guardianOwnerLaunches) launch(ctx context.Context, executable, root string, environment []string) error {
	if l.before != nil {
		l.before()
	}
	_, bounded := ctx.Deadline()
	l.launches = append(l.launches, guardianOwnerLaunch{
		executable: executable, root: root, environment: slices.Clone(environment), bounded: bounded && ctx.Err() == nil,
	})
	return l.err
}

// dependencies is a guardian's normalized composition with this recorder as
// its maintenance launcher and a captured launch scope.
func (l *guardianOwnerLaunches) dependencies() Dependencies {
	return Dependencies{
		Environment:       handoffCapturedEnvironment(),
		SupervisorOptions: pueue.Options{ControlOwnerExecutable: handoffOwnerExecutable, PrivateMaintenanceLauncher: l.launch},
	}
}

// requireLaunchedOnce requires exactly one bounded launch of the normalized
// executable for root, with only the captured scope's bounded variables.
func (l *guardianOwnerLaunches) requireLaunchedOnce(t *testing.T, root string) {
	t.Helper()
	wantEnvironment := []string{"PATH=/captured/bin", "HOME=/captured/home"}
	if len(l.launches) != 1 {
		t.Fatalf("maintenance owner launches = %+v, want exactly one", l.launches)
	}
	got := l.launches[0]
	if got.executable != handoffOwnerExecutable || got.root != root || !slices.Equal(got.environment, wantEnvironment) || !got.bounded {
		t.Fatalf("maintenance owner launch = %+v, want %s for %s with %q under a live bound", got, handoffOwnerExecutable, root, wantEnvironment)
	}
}

func (l *guardianOwnerLaunches) requireNoLaunch(t *testing.T) {
	t.Helper()
	if len(l.launches) != 0 {
		t.Fatalf("maintenance owner launches = %+v, want none", l.launches)
	}
}

// guardianCustodyRun is one closed or open task lifetime with a bound view,
// completed by a guardian whose own run failed.
type guardianCustodyRun struct {
	store        *taskdir.Store
	td           *taskdir.TaskDir
	taskID       string
	private      bool
	registration *task.LifetimeRegistrationRecord
}

func newGuardianCustodyRun(t *testing.T, cause string, private bool) guardianCustodyRun {
	t.Helper()
	store, td, request := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	if private {
		makePrivateSupervisorBase(t, store.Root)
	}
	registration := recordCustodyLifetime(t, td, newCustodyTestPlacement(t), cause)
	bindCustodyTestView(t, registration)
	return guardianCustodyRun{store: store, td: td, taskID: request.TaskID, private: private, registration: registration}
}

// complete runs the guardian's post-run composition and requires its result
// to stay the guardian's own.
func (r guardianCustodyRun) complete(t *testing.T, inspectionRun bool, deps Dependencies) {
	t.Helper()
	var failed *guardian.Guardian
	wantErr := failed.Run(context.Background())
	state := lifetimeGuardianRunState{closure: r.td, privateSupervisor: r.private}
	parsed := runnerArguments{Root: r.store.Root, TaskID: r.taskID, Inspection: inspectionRun}
	if err := runGuardianWithWorkspaceCompletion(failed, state, r.store, parsed, deps); err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("guardian result = %v, want unchanged %v", err, wantErr)
	}
}

// TestOrdinaryPrivateGuardianHandsCustodyToLaunchedOwner: an ordinary private
// guardian with a closed lifetime removes no view. Its full wake is durable
// before it launches one owner with the normalized executable, its root and
// its captured scope. That owner alone, with no terminal callback, releases
// the exact recorded pool and drains the wake.
func TestOrdinaryPrivateGuardianHandsCustodyToLaunchedOwner(t *testing.T) {
	store := newBareInspectionStore(t)
	newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	taskID := strings.Repeat("d", 32)
	td, registration := newPlacedTaskInStore(t, store, taskID, pueue.PrivateConfigPath(store.Root), task.LifetimeTerminationCompleted)
	run := guardianCustodyRun{store: store, td: td, taskID: taskID, private: true, registration: registration}
	before := readTaskMaintenanceWake(t, store.Root)
	launches := &guardianOwnerLaunches{before: func() {
		requireTaskMaintenanceFullWake(t, store.Root, before)
		if !custodyTestPending(t, registration) {
			t.Error("the guardian removed views before its handoff")
		}
	}}
	run.complete(t, false, launches.dependencies())
	launches.requireLaunchedOnce(t, store.Root)
	if !custodyTestPending(t, registration) {
		t.Fatal("the guardian released custody directly")
	}
	requireTaskMaintenanceFullWake(t, store.Root, before)

	owner := terminalEventMaintenanceDependencies(nil, nil)
	if err := runQueueMaintenance(runnerArguments{Root: launches.launches[0].root}, owner); err != nil {
		t.Fatal(err)
	}
	if custodyTestPending(t, registration) {
		t.Fatal("the launched owner did not release the exact recorded pool")
	}
	if wake := readTaskMaintenanceWake(t, store.Root); wake.Attempted != wake.Requested {
		t.Fatalf("the launched owner left a runnable wake: %+v", wake)
	}
}

// TestOrdinaryPrivateGuardianHandoffNeverReleasesDirectly: an ordinary private
// guardian never falls back to a direct release. Pending closure and released
// custody request nothing; a wake publication fault launches nothing; a launch
// fault or a missing launcher keeps the committed wake for a later owner.
func TestOrdinaryPrivateGuardianHandoffNeverReleasesDirectly(t *testing.T) {
	for _, row := range []struct {
		name          string
		cause         string
		prepare       func(*testing.T, guardianCustodyRun, *guardianOwnerLaunches, *Dependencies)
		pending, wake bool
		launched      bool
	}{
		{name: "closure pending", cause: custodyLifetimeOpen, pending: true},
		{name: "already released", cause: task.LifetimeTerminationCompleted, prepare: func(t *testing.T, run guardianCustodyRun, _ *guardianOwnerLaunches, _ *Dependencies) {
			if err := releaseLifetimeCustody(t.Context(), run.td); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wake publication fault", cause: task.LifetimeTerminationCompleted, pending: true, prepare: func(t *testing.T, run guardianCustodyRun, _ *guardianOwnerLaunches, _ *Dependencies) {
			writeFailedFullMaintenancePass(t, run.store.Root, ^uint64(0))
		}},
		{name: "launch fault", cause: task.LifetimeTerminationCompleted, pending: true, wake: true, launched: true, prepare: func(_ *testing.T, _ guardianCustodyRun, launches *guardianOwnerLaunches, _ *Dependencies) {
			launches.err = errors.New("launch refused")
		}},
		{name: "missing launcher", cause: task.LifetimeTerminationCompleted, pending: true, wake: true, prepare: func(_ *testing.T, _ guardianCustodyRun, _ *guardianOwnerLaunches, deps *Dependencies) {
			deps.SupervisorOptions.PrivateMaintenanceLauncher = nil
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := newGuardianCustodyRun(t, row.cause, true)
			launches := &guardianOwnerLaunches{}
			deps := launches.dependencies()
			if row.prepare != nil {
				row.prepare(t, run, launches, &deps)
			}
			before := readTaskMaintenanceWake(t, run.store.Root)
			run.complete(t, false, deps)
			if pending := custodyTestPending(t, run.registration); pending != row.pending {
				t.Fatalf("custody pending = %t, want %t", pending, row.pending)
			}
			if row.launched {
				launches.requireLaunchedOnce(t, run.store.Root)
			} else {
				launches.requireNoLaunch(t)
			}
			if !row.wake {
				requireTaskMaintenanceWakeUnchanged(t, run.store.Root, before, "the guardian")
				return
			}
			requireTaskMaintenanceFullWake(t, run.store.Root, before)
			runTaskCustodyOwner(t, run.store.Root)
			if custodyTestPending(t, run.registration) {
				t.Fatal("a later owner did not recover the retained wake's custody")
			}
		})
	}
}

// TestGuardianWithoutClosureRequestsNoCustodyHandoff: a run without closure
// records, such as a workspace operation, neither releases, requests nor
// launches anything for custody.
func TestGuardianWithoutClosureRequestsNoCustodyHandoff(t *testing.T) {
	run := newGuardianCustodyRun(t, task.LifetimeTerminationCompleted, true)
	launches := &guardianOwnerLaunches{}
	var failed *guardian.Guardian
	wantErr := failed.Run(context.Background())
	state := lifetimeGuardianRunState{privateSupervisor: true}
	parsed := runnerArguments{Root: run.store.Root, TaskID: run.taskID}
	if err := runGuardianWithWorkspaceCompletion(failed, state, run.store, parsed, launches.dependencies()); err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("guardian result = %v, want unchanged %v", err, wantErr)
	}
	if !custodyTestPending(t, run.registration) {
		t.Fatal("a run without closure records released custody")
	}
	requireTaskMaintenanceWakeUnchanged(t, run.store.Root, taskMaintenanceWake{}, "a run without closure records")
	launches.requireNoLaunch(t)
}

// substituteCustodyTestPool puts another prepared pool at the recorded root,
// so the root exists with a different identity, and returns the restore step;
// cleanup restores both pools if the test did not.
func substituteCustodyTestPool(t *testing.T, placement *task.LifetimeCustodyPlacement) func() {
	t.Helper()
	foreign := newCustodyTestPlacement(t)
	restoreRecorded := moveCustodyTestPool(t, placement)
	if err := os.Rename(foreign.PoolRoot, placement.PoolRoot); err != nil {
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		if err := os.Rename(placement.PoolRoot, foreign.PoolRoot); err != nil {
			t.Error(err)
		}
		restoreRecorded()
	})
	t.Cleanup(restore)
	return restore
}

// handoffFaultCase is one fault an ordinary private guardian's handoff must
// report without removing a view. prepare, when set, injects the fault and
// returns the step that restores the fixture before views are observed.
// launchErr is the recorded launcher's result.
type handoffFaultCase struct {
	name      string
	prepare   func(*testing.T, guardianCustodyRun, *pueue.Options) (restore func())
	launchErr error
	want      []error
	wake      bool
	launched  bool
}

func unchangedHandoffFixture() {}

// TestGuardianCustodyHandoffReportsFaultsWithoutReleasing: a pending check
// fault is returned beside one conservative wake and launch; a wake fault
// launches nothing; a launch fault and a missing executable or launcher keep
// the wake and report that no owner started. No row removes a view.
func TestGuardianCustodyHandoffReportsFaultsWithoutReleasing(t *testing.T) {
	launchErr := errors.New("launch refused")
	for _, fault := range []handoffFaultCase{
		{name: "missing pool", wake: true, launched: true, prepare: func(t *testing.T, run guardianCustodyRun, _ *pueue.Options) func() {
			return moveCustodyTestPool(t, run.registration.CustodyPlacement)
		}},
		{name: "substituted pool", wake: true, launched: true, prepare: func(t *testing.T, run guardianCustodyRun, _ *pueue.Options) func() {
			return substituteCustodyTestPool(t, run.registration.CustodyPlacement)
		}},
		{name: "wake publication fault", want: []error{pueue.ErrConfiguration}, prepare: func(t *testing.T, run guardianCustodyRun, _ *pueue.Options) func() {
			writeFailedFullMaintenancePass(t, run.store.Root, ^uint64(0))
			return unchangedHandoffFixture
		}},
		{name: "launch fault", launchErr: launchErr, want: []error{launchErr, errPrivateMaintenanceNotStarted}, wake: true, launched: true},
		{name: "missing executable", want: []error{errPrivateMaintenanceNotStarted}, wake: true, prepare: func(_ *testing.T, _ guardianCustodyRun, options *pueue.Options) func() {
			options.ControlOwnerExecutable = ""
			return unchangedHandoffFixture
		}},
	} {
		t.Run(fault.name, func(t *testing.T) { runHandoffFaultCase(t, fault) })
	}
}

// runHandoffFaultCase runs one fault against a closed private lifetime with a
// bound view and requires its reported causes, retained views, launch and wake.
func runHandoffFaultCase(t *testing.T, fault handoffFaultCase) {
	t.Helper()
	run := newGuardianCustodyRun(t, task.LifetimeTerminationCompleted, true)
	launches := &guardianOwnerLaunches{err: fault.launchErr}
	deps := launches.dependencies()
	options := supervisorcore.OptionsForEnvironment(deps.SupervisorOptions, deps.Environment)
	restore := unchangedHandoffFixture
	if fault.prepare != nil {
		restore = fault.prepare(t, run, &options)
	}
	before := readTaskMaintenanceWake(t, run.store.Root)
	err := handOffGuardianLifetimeCustody(run.store.Root, run.td, options)
	restore()
	if err == nil {
		t.Fatal("a handoff fault was not reported")
	}
	for _, want := range fault.want {
		if !errors.Is(err, want) {
			t.Fatalf("handoff fault = %v, want %v", err, want)
		}
	}
	if !custodyTestPending(t, run.registration) {
		t.Fatal("a handoff fault removed views")
	}
	if fault.launched {
		launches.requireLaunchedOnce(t, run.store.Root)
	} else {
		launches.requireNoLaunch(t)
	}
	if fault.wake {
		requireTaskMaintenanceFullWake(t, run.store.Root, before)
	} else {
		requireTaskMaintenanceWakeUnchanged(t, run.store.Root, before, "a failed wake publication")
	}
}
