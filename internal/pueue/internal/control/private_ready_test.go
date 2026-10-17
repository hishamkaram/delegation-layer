package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadyAcceptsAdditiveQueueSchema(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	if err := os.WriteFile(fake.statusPath, []byte(`{"tasks":{},"groups":{},"future":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fake.client.Ready(context.Background()); err != nil {
		t.Fatalf("additive supervisor status was rejected: %v", err)
	}
}

func TestReadyRevalidatesBindingBeforeStatus(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	data, err := os.ReadFile(fake.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake.configPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fake.client.Ready(context.Background()); !errors.Is(err, ErrBinding) {
		t.Fatalf("Ready accepted a changed binding: %v", err)
	}
}

func TestReachableFailsClosedWhenStatusFailsOnLiveEndpoint(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	socketPath := filepath.Join(filepath.Dir(fake.configPath), "socket")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if removeErr := os.Remove(socketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			t.Error(removeErr)
		}
	})
	fake.environment = append(fake.environment, "FAKE_READY="+filepath.Join(filepath.Dir(fake.configPath), "missing-ready"))
	fake.client.options.Environment = fake.environment
	ready, err := fake.client.Reachable(context.Background())
	if ready {
		t.Fatal("reachable reported a failed status as ready")
	}
	if !errors.Is(err, ErrBinding) {
		t.Fatalf("failed status on a live endpoint was treated as absence: %v", err)
	}
}

func TestWaitUnavailableFailsClosedOnBindingError(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	fake.client.runtimeBinding.Endpoint = "tcp:invalid"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := fake.client.WaitUnavailable(ctx)
	if !errors.Is(err, ErrBinding) {
		t.Fatalf("WaitUnavailable did not report the binding error: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitUnavailable retried a non-recoverable binding error until its deadline: %v", err)
	}
}

func TestWaitUnavailableDoesNotTreatRefusedSocketAsAbsent(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	socketPath := filepath.Join(filepath.Dir(fake.configPath), "stopped.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener type = %T, want *net.UnixListener", listener)
	}
	unixListener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.Remove(socketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			t.Error(removeErr)
		}
	})
	fake.client.runtimeBinding.Endpoint = "unix:" + socketPath
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err = fake.client.WaitUnavailable(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitUnavailable treated a refused socket as absent: %v", err)
	}
}

func TestWaitUnavailableRemovesStateRootedStaleSocketAfterDaemonExit(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	socketPath := strings.TrimPrefix(client.runtimeBinding.Endpoint, "unix:")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener type = %T, want *net.UnixListener", listener)
	}
	unixListener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.pid")
	if err = os.WriteFile(pidPath, []byte("99999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = client.WaitUnavailable(context.Background()); err != nil {
		t.Fatalf("stale private socket was not recovered: %v", err)
	}
	if _, err = os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale private socket remained after recovery: %v", err)
	}
}

func TestWaitUnavailableWaitsForPrivateDaemonProcessExit(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	process, daemonPath, finishProcess := startBlockingProcess(t)
	client.runtimeBinding.DaemonExecutable = daemonPath

	pidPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.pid")
	if err = os.WriteFile(pidPath, []byte(strconv.Itoa(process.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client.runtimeBinding.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "missing.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = client.WaitUnavailable(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitUnavailable ignored a live private daemon process: %v", err)
	}

	if err = finishProcess(); err != nil {
		t.Fatal(err)
	}
	if err = client.WaitUnavailable(context.Background()); err != nil {
		t.Fatalf("WaitUnavailable did not complete after endpoint and process exit: %v", err)
	}
}

func startBlockingProcess(t *testing.T) (*exec.Cmd, string, func() error) {
	t.Helper()
	processInput, processOutput, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command("cat")
	process.Stdin = processInput
	if err = process.Start(); err != nil {
		if closeErr := processOutput.Close(); closeErr != nil {
			t.Logf("close process output: %v", closeErr)
		}
		if closeErr := processInput.Close(); closeErr != nil {
			t.Logf("close process input: %v", closeErr)
		}
		t.Fatal(err)
	}
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	daemonPath, err := canonicalExecutable(catPath)
	if err != nil {
		t.Fatal(err)
	}
	finished := false
	finish := func() error {
		if finished {
			return nil
		}
		finished = true
		if err = processOutput.Close(); err != nil {
			return err
		}
		if err = processInput.Close(); err != nil {
			return err
		}
		return process.Wait()
	}
	t.Cleanup(func() {
		if cleanupErr := finish(); cleanupErr != nil {
			t.Logf("finish blocking process: %v", cleanupErr)
		}
	})
	return process, daemonPath, finish
}

func TestBindPrivateFailsClosedWhenIncompatibleEndpointIsLive(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	socketPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if removeErr := os.Remove(socketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			t.Error(removeErr)
		}
	})
	if _, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout}); !errors.Is(err, ErrBinding) {
		t.Fatalf("private bootstrap accepted an incompatible live endpoint: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private bootstrap started a replacement daemon for a live incompatible endpoint: %v", err)
	}
}

func TestBindPrivateHandlesLargeActiveQueueWithoutCleanup(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	// This proves row capacity, not a wall-clock SLA under race instrumentation.
	client, err := BindPrivate(t.Context(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{
		ObservationTimeout:          30 * time.Second,
		SuccessfulCleanupAuthorizer: allowSuccessfulCleanupForTest,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeLargePrivateQueue(t, fixture.stateRoot, false)
	if err = client.Ready(context.Background()); err != nil {
		t.Fatalf("200 active rows failed readiness: %v", err)
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil || len(snapshot.Jobs) != 200 {
		t.Fatalf("large queue lost rows: count=%d err=%v", len(snapshot.Jobs), err)
	}
}

func TestPrivateCleanupRemovesOnlyAuthorizedTerminalRows(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	var removeCommands atomic.Int64
	client.options.Observer = func(event CommandEvent) {
		if event.Stage == "entry" && strings.Contains("\x00"+strings.Join(event.Argv, "\x00")+"\x00", "\x00remove\x00") {
			removeCommands.Add(1)
		}
	}
	writeLargePrivateQueue(t, fixture.stateRoot, true)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if err := os.WriteFile(filepath.Join(base, "test-record-kill"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error { return client.cleanSuccessfulTasks(context.Background(), false) }); err != nil {
		t.Fatal(err)
	}
	removed := strings.Fields(string(readPrivateSupervisorFile(t, filepath.Join(base, "test-kill-task-id"))))
	if len(removed) != 199 {
		t.Fatalf("removed %d rows, want199", len(removed))
	}
	for _, id := range removed {
		if id == "199" {
			t.Fatal("cleanup removed active row")
		}
	}
	if got := removeCommands.Load(); got != 1 {
		t.Fatalf("cleanup issued %d remove commands, want one batch", got)
	}
	if _, err := os.Stat(filepath.Join(base, "test-status.json")); err != nil {
		t.Fatalf("cleanup issued broad clean: %v", err)
	}
}

func TestPrivateCleanupFailsClosedWithoutApplicationAuthorizer(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, Options{ObservationTimeout: DefaultObservationTimeout})
	if err := client.cleanSuccessfulTasks(context.Background(), false); !errors.Is(err, ErrCleanupAuthorization) {
		t.Fatalf("cleanup without authorizer: %v", err)
	}
}

func writeLargePrivateQueue(t *testing.T, stateRoot string, completed bool) {
	t.Helper()
	rows := make(map[string]any)
	for id := range 200 {
		state := map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}
		if completed && id < 199 {
			state = map[string]any{"Done": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": "Success"}}
		}
		row := statusTestJob(int64(id), "delegate:"+strings.Repeat("a", 32)+":"+fmt.Sprintf("%032x", id), state)
		row["envs"] = map[string]string{"PATH": strings.Repeat("x", 6000)}
		rows[strconv.Itoa(id)] = row
	}
	data := statusTestPayload(t, rows, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if len(data) <= MaxControlBytes {
		t.Fatal("large fixture did not exceed old whole-queue limit")
	}
	if err := os.WriteFile(filepath.Join(stateRoot, privateSupervisorDirectory, "test-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
