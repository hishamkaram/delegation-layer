package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func schedulerServeDependencies(t *testing.T) Dependencies {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	deps := schedulerStartupDependencies(t)
	// Startup controls below never launch a child; the executable only satisfies
	// the native runner-path prerequisite before exercising storage ownership.
	deps.RunnerExecutable = executable
	return deps
}

func schedulerServeJSON(t *testing.T, ctx context.Context, root string, deps Dependencies, flags ...string) (Response, int) {
	t.Helper()
	argv := append([]string{"--root", root, "scheduler", "serve", "--json"}, flags...)
	var stdout, stderr bytes.Buffer
	code := RunContext(ctx, argv, &stdout, &stderr, deps)
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || stderr.Len() != 0 {
		t.Fatalf("serve code=%d stdout=%q stderr=%q decode=%v", code, stdout.String(), stderr.String(), err)
	}
	return response, code
}

func schedulerServeStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestSchedulerServeInitializesEmptyStorageBeforeHostOwnership(t *testing.T) {
	store := schedulerServeStore(t)
	lease, err := host.AcquireHostLease(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(schedulerStartupContext(t))
	finished := make(chan commandResult, 1)
	deps := schedulerServeDependencies(t)
	go func() { finished <- serveScheduler(ctx, Arguments{Root: store.Root}, deps) }()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			<-finished
		}
	})
	// Observe the actual publication and E release while H remains occupied;
	// native filesystem speed must not select which timeout branch is tested.
	execution := awaitSchedulerInitialization(t, ctx, store)
	cancel()
	result := <-finished
	joined = true
	if result.code == 0 || result.response.RootID != store.RootID || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("serve did not join cancellation at the occupied host boundary: %+v", result)
	}
	if err := execution.Close(); err != nil {
		t.Fatal(err)
	}
}

func awaitSchedulerInitialization(t *testing.T, ctx context.Context, store *taskdir.Store) *scheduler.OwnerLease {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		opened, err := scheduler.Open(ctx, store)
		if err == nil {
			if closeErr := opened.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			lease, leaseErr := scheduler.AcquireOwnerLease(ctx, store)
			if leaseErr == nil {
				t.Cleanup(func() {
					if releaseErr := lease.Close(); releaseErr != nil {
						t.Error(releaseErr)
					}
				})
				return lease
			}
			last = leaseErr
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("scheduler initialization did not publish and release E: %v", last)
		case <-ticker.C:
		}
	}
}

func TestSchedulerServeInvalidRunnerDoesNotInitialize(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing managed runner", true: "custom runner"}[custom], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			deps := schedulerStartupDependencies(t)
			deps.RunnerExecutable = filepath.Join(t.TempDir(), "missing-runner")
			var flags []string
			wantCode := 1
			if custom {
				flags, wantCode = []string{"--runner", deps.RunnerExecutable}, 2
			}
			response, code := schedulerServeJSON(t, schedulerStartupContext(t), root, deps, flags...)
			if code != wantCode || !strings.Contains(response.Error, "runner") || strings.Contains(response.Error, "create a schedule first") {
				t.Fatalf("runner precondition lost: code=%d response=%+v", code, response)
			}
			if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("invalid serve initialized storage: %v", err)
			}
		})
	}
}

func TestSchedulerInitializersRefuseBusyOwnerWithoutDatabaseOrHost(t *testing.T) {
	for _, action := range []string{"serve", "create"} {
		t.Run(action, func(t *testing.T) { assertSchedulerInitializerRefusesBusyOwner(t, action) })
	}
}

func assertSchedulerInitializerRefusesBusyOwner(t *testing.T, action string) {
	t.Helper()
	store := schedulerServeStore(t)
	lease, err := scheduler.AcquireOwnerLease(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	var response Response
	var code int
	if action == "serve" {
		response, code = schedulerServeJSON(t, schedulerStartupContext(t), store.Root, schedulerServeDependencies(t))
	} else {
		args := scheduleCreateArgs(t)
		args.Root = store.Root
		deps := Dependencies{}
		result := createScheduleCommand(args, deps.storeDependencies())
		response, code = result.response, result.code
		if response.Schedule == nil || response.Schedule.State == contract.StateEnabled || response.Schedule.Saved {
			t.Fatalf("busy initialization reported a saved definition: %+v", response)
		}
	}
	if code != 1 || !strings.Contains(response.Error, task.ErrLockBusy.Error()) {
		t.Fatalf("busy initializer did not fail explicitly: code=%d response=%+v", code, response)
	}
	for _, name := range []string{filepath.Join(".scheduler", "schedules.sqlite"), ".scheduler-host.lock"} {
		if _, err := os.Lstat(filepath.Join(store.Root, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("busy initializer created %s: %v", name, err)
		}
	}
}

func TestSchedulerServePreservesCorruptStorageAndRootIdentity(t *testing.T) {
	store := schedulerServeStore(t)
	directory := filepath.Join(store.Root, ".scheduler")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "schedules.sqlite")
	corrupt := []byte("not a SQLite database")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	response, code := schedulerServeJSON(t, schedulerStartupContext(t), store.Root, schedulerServeDependencies(t))
	if code != 1 || response.Error == "" || response.RootID != store.RootID || strings.Contains(response.Error, "create a schedule first") {
		t.Fatalf("corrupt storage was not refused: code=%d response=%+v", code, response)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, corrupt) {
		t.Fatalf("serve replaced corrupt storage: %q, %v", actual, err)
	}
	if _, err := os.Lstat(filepath.Join(store.Root, ".scheduler-host.lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("corrupt storage acquired host ownership: %v", err)
	}
}
