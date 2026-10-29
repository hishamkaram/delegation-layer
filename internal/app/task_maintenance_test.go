package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
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
// and the inspection return path, and never changes the guardian's result. An
// external run's fault creates no private queue state.
func TestGuardianCustodyFaultPersistsPrivateFullWake(t *testing.T) {
	var failed *guardian.Guardian
	wantErr := failed.Run(context.Background())
	for _, inspectionRun := range []bool{false, true} {
		for _, private := range []bool{false, true} {
			store, td, request := newAppTestTask(t, false)
			if private {
				makePrivateSupervisorBase(t, store.Root)
			}
			registration := recordCustodyLifetime(t, td, newCustodyTestPlacement(t), task.LifetimeTerminationCompleted)
			bindCustodyTestView(t, registration)
			restore := moveCustodyTestPool(t, registration.CustodyPlacement)
			state := lifetimeGuardianRunState{closure: td, privateSupervisor: private}
			parsed := runnerArguments{Root: store.Root, TaskID: request.TaskID, Inspection: inspectionRun}
			err := runGuardianWithWorkspaceCompletion(failed, state, store, parsed, Dependencies{})
			restore()
			if err == nil || err.Error() != wantErr.Error() {
				t.Fatalf("guardian result = %v, want unchanged %v", err, wantErr)
			}
			if !custodyTestPending(t, registration) {
				t.Fatal("a missing pool was treated as released")
			}
			if private {
				requireTaskMaintenanceFullWake(t, store.Root, taskMaintenanceWake{})
			} else if _, statErr := os.Lstat(filepath.Join(store.Root, ".supervisor")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("an external release fault created private queue state: %v", statErr)
			}
			closeAppTestTask(t, store, td)
		}
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
