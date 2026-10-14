package pueue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// privateTestObservationTimeout keeps normal success-path fixtures independent
// from race-instrumented process scheduling. Tests that prove a bounded timeout
// set their own smaller observation window explicitly.
const privateTestObservationTimeout = 10 * time.Second

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

func TestProbePrivateDoesNotStartStoppedSupervisor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = client.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	probed, ready, err := ProbePrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	if probed == nil || ready {
		t.Fatalf("stopped private supervisor probe = client %v ready %t, want client and not ready", probed != nil, ready)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestProbePrivateEndpointUsesNoControlCommand(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	client, err := BindPrivate(t.Context(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = WithBootstrapLock(t.Context(), fixture.stateRoot, func() error {
		_, configureErr := client.SetPrivateDefaultParallelTasksWithBootstrapLock(t.Context(), fixture.stateRoot, 0)
		return configureErr
	}); err != nil {
		t.Fatal(err)
	}
	listener := listenPrivateTestEndpoint(t, client.Binding().Endpoint)
	defer func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	path := strings.TrimPrefix(client.Binding().Endpoint, "unix:")
	if unavailable, refused, probeErr := privateEndpointProbe(t.Context(), path); probeErr != nil || unavailable || refused {
		t.Fatalf("listening endpoint = unavailable %t refused %t err %v", unavailable, refused, probeErr)
	}
	controlMarker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "endpoint-probe-control")
	options.Environment = append(os.Environ(), "PRIVATE_CONTROL_MARKER="+controlMarker)

	probed, ready, err := ProbePrivateEndpointWithGate(t.Context(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options, func(context.Context, *Client) (bool, error) {
		return true, nil
	})
	if err != nil || !ready || probed == nil {
		t.Fatalf("endpoint probe = client %v ready %t err %v", probed != nil, ready, err)
	}
	if err = probed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(controlMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("endpoint probe launched a Pueue control command: %v", err)
	}
}

func TestProbePrivateEndpointRejectsRefusedSocketWithoutControlCommand(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	client, err := BindPrivate(t.Context(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = WithBootstrapLock(t.Context(), fixture.stateRoot, func() error {
		_, configureErr := client.SetPrivateDefaultParallelTasksWithBootstrapLock(t.Context(), fixture.stateRoot, 0)
		return configureErr
	}); err != nil {
		t.Fatal(err)
	}
	listener := listenPrivateTestEndpoint(t, client.Binding().Endpoint)
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener type = %T, want *net.UnixListener", listener)
	}
	unixListener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		path := strings.TrimPrefix(client.Binding().Endpoint, "unix:")
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			t.Error(removeErr)
		}
	})
	controlMarker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "endpoint-probe-control")
	options.Environment = append(os.Environ(), "PRIVATE_CONTROL_MARKER="+controlMarker)

	probed, ready, err := ProbePrivateEndpointWithGate(t.Context(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options, nil)
	if err != nil || ready || probed == nil {
		t.Fatalf("refused endpoint probe = client %v ready %t err %v", probed != nil, ready, err)
	}
	if err = probed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(controlMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused endpoint probe launched a Pueue control command: %v", err)
	}
}

func listenPrivateTestEndpoint(t *testing.T, endpoint string) net.Listener {
	t.Helper()
	const prefix = "unix:"
	if !strings.HasPrefix(endpoint, prefix) {
		t.Fatalf("private supervisor endpoint = %q, want Unix socket", endpoint)
	}
	listener, err := net.Listen("unix", strings.TrimPrefix(endpoint, prefix))
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func TestReachableDoesNotMutateSuccessfulTasksWhenStatusOverflows(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	cleanMarker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "clean-reachable-successful")
	options := Options{ObservationTimeout: 5 * time.Second, Environment: append(os.Environ(), "PRIVATE_CLEAN_MARKER="+cleanMarker), SuccessfulCleanupAuthorizer: allowSuccessfulCleanupForTest}
	client, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json")
	status := []byte(`{"tasks":{},"groups":{},"extra":"` + strings.Repeat("x", MaxControlBytes+1) + `"}`)
	if writeErr := os.WriteFile(statusPath, status, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	ready, err := client.Reachable(context.Background())
	if ready {
		t.Fatal("reachable reported an oversized status as ready")
	}
	if !errors.Is(err, errStatusControlLimit) {
		t.Fatalf("reachable returned the wrong oversized-status error: %v", err)
	}
	if _, err := os.Stat(cleanMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only reachable unexpectedly requested cleanup: %v", err)
	}
	if _, err := os.Stat(statusPath); err != nil {
		t.Fatalf("read-only reachable removed oversized status state: %v", err)
	}
}

func TestStartPrivateObservesLargeActiveQueueWithoutReaping(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	cleanMarker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "clean-start-successful")
	options := Options{ObservationTimeout: 5 * time.Second, Environment: append(os.Environ(), "PRIVATE_CLEAN_MARKER="+cleanMarker), SuccessfulCleanupAuthorizer: allowSuccessfulCleanupForTest}
	client, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}

	privateBase := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if err := os.Remove(filepath.Join(privateBase, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, fixture.stateRoot)
	statusPath := filepath.Join(privateBase, "test-status.json")
	writeLargePrivateQueue(t, fixture.stateRoot, false)

	if err := client.StartPrivate(context.Background(), fixture.stateRoot); err != nil {
		t.Fatalf("private startup did not recover from oversized terminal state: %v", err)
	}
	if _, err := os.Stat(cleanMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup reaped active queue: %v", err)
	}
	if _, err := os.Stat(statusPath); err != nil {
		t.Fatalf("large active queue was removed: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func TestPrivateClientReadyDoesNotCleanAfterVersionOverflows(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	versionPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "oversized-version")
	if err := os.WriteFile(versionPath, bytes.Repeat([]byte("v"), MaxControlBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanMarker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "clean-version-overflow")
	client.options.Environment = append(
		os.Environ(),
		"PRIVATE_VERSION_FILE="+versionPath,
		"PRIVATE_CLEAN_MARKER="+cleanMarker,
	)
	// Version discovery belongs to binding. Readiness of an already pinned
	// executable probes status directly rather than running --version again.
	rebound, err := BindPrivate(t.Context(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, client.options)
	if rebound != nil {
		if closeErr := rebound.CloseAfterPending(err); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("version overflow produced a new private binding")
	}
	if !errors.Is(err, ErrControlLimit) {
		t.Fatalf("version overflow returned the wrong error: %v", err)
	}
	if _, err := os.Stat(cleanMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version overflow triggered successful-task cleanup: %v", err)
	}
}

func TestBindSavedRejectsChangedLogicalSupervisor(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	saved := fake.client.Binding()
	data, err := os.ReadFile(fake.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(fake.configPath, append(data, '\n'), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if _, err := BindSaved(context.Background(), saved, Options{Environment: fake.environment}); !errors.Is(err, ErrBinding) {
		t.Fatalf("saved binding accepted changed logical supervisor: %v", err)
	}
}

func TestBindSavedForMigrationAcceptsChangedConfigAtSameEndpoint(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	saved := fake.client.Binding()
	data, err := os.ReadFile(fake.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(fake.configPath, append(data, '\n'), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	migration, err := BindSavedForMigration(context.Background(), saved, Options{Environment: fake.environment})
	if err != nil {
		t.Fatalf("migration binding rejected compatible config drift: %v", err)
	}
	defer func() {
		if err := migration.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if migration.Binding() != saved {
		t.Fatalf("migration changed durable legacy identity: got=%+v want=%+v", migration.Binding(), saved)
	}
	if err := migration.Ready(context.Background()); err != nil {
		t.Fatalf("migration binding could not inspect the saved endpoint: %v", err)
	}
}

func TestBindSavedForMigrationRejectsPrivateStatePathDriftAtSameEndpoint(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	saved := first.Binding()
	configPath := saved.ConfigPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	oldData := strconv.Quote(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "data"))
	newData := strconv.Quote(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "other-data"))
	changed := strings.Replace(string(data), "pueue_directory: "+oldData, "pueue_directory: "+newData, 1)
	if changed == string(data) {
		t.Fatal("private fixture data path was not changed")
	}
	if writeErr := os.WriteFile(configPath, []byte(changed), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if _, err := BindSavedForMigration(context.Background(), saved, Options{}); !errors.Is(err, ErrBinding) {
		t.Fatalf("private state path drift was accepted: %v", err)
	}
}

func TestBindSavedForMigrationUsesSavedEndpointAfterPrivateConfigDrift(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	saved := first.Binding()
	configPath := saved.ConfigPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	oldSocket := strconv.Quote(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.sock"))
	newSocket := strconv.Quote(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "current.sock"))
	changed := strings.Replace(string(data), "unix_socket_path: "+oldSocket, "unix_socket_path: "+newSocket, 1)
	if changed == string(data) {
		t.Fatal("private fixture socket path was not changed")
	}
	if writeErr := os.WriteFile(configPath, []byte(changed), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	migration, err := BindSavedForMigration(context.Background(), saved, Options{})
	if err != nil {
		t.Fatalf("migration binding rejected endpoint drift: %v", err)
	}
	defer func() {
		if err := migration.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if migration.Binding() != saved || migration.runtimeBinding.Endpoint != saved.Endpoint {
		t.Fatalf("migration did not retain the saved endpoint: durable=%+v runtime=%+v saved=%+v", migration.Binding(), migration.runtimeBinding, saved)
	}
	if migration.runtimeBinding.ConfigPath == saved.ConfigPath {
		t.Fatal("endpoint migration reused the changed config instead of an isolated recovery config")
	}
	if err := migration.Ready(context.Background()); err != nil {
		t.Fatalf("migration binding could not inspect the saved endpoint: %v", err)
	}
}

func TestCompactPrivateEndpointMigrationAndRecoveryUseSavedEndpoint(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		t.Fatal(err)
	}
	if err := markPrivateBootstrapPending(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	configPath := PrivateConfigPath(fixture.stateRoot)
	if err := createPrivateConfig(configPath, base, privateConfigYAMLWithSocket(base, filepath.Join(base, "s"))); err != nil {
		t.Fatal(err)
	}
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	saved := first.Binding()
	if saved.Endpoint != "unix:"+filepath.Join(base, "s") {
		t.Fatalf("saved compact endpoint = %q", saved.Endpoint)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := configWithSocketPath(data, filepath.Join(base, "t"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}

	migration, err := BindSavedForMigration(context.Background(), saved, Options{})
	if err != nil {
		t.Fatalf("compact endpoint migration failed: %v", err)
	}
	if migration.runtimeBinding.Endpoint != saved.Endpoint {
		t.Fatalf("migration endpoint = %q, want %q", migration.runtimeBinding.Endpoint, saved.Endpoint)
	}
	if err = migration.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatalf("persist compact recovery binding: %v", err)
	}
	if err = migration.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := RecoverPrivate(
		context.Background(),
		fixture.stateRoot,
		saved,
		fixture.clientPath,
		fixture.daemonPath,
		Options{ObservationTimeout: privateTestObservationTimeout},
	)
	if err != nil {
		t.Fatalf("compact endpoint recovery failed: %v", err)
	}
	defer func() {
		if closeErr := recovered.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	if recovered.runtimeBinding.Endpoint != saved.Endpoint {
		t.Fatalf("recovered endpoint = %q, want %q", recovered.runtimeBinding.Endpoint, saved.Endpoint)
	}
}

func TestRecoverSavedWithPairUsesCurrentExecutablesAfterSavedInstallRemoval(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldClient, oldDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "old"))
	currentClient, currentDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "current"))
	first := bindPrivateForTest(t, stateRoot, oldClient, oldDaemon)
	saved := first.Binding()
	configPath := saved.ConfigPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	oldSocket := strconv.Quote(filepath.Join(stateRoot, privateSupervisorDirectory, "run", "pueue.sock"))
	newSocket := strconv.Quote(filepath.Join(stateRoot, privateSupervisorDirectory, "run", "legacy.sock"))
	changed := strings.Replace(string(data), "unix_socket_path: "+oldSocket, "unix_socket_path: "+newSocket, 1)
	if changed == string(data) {
		t.Fatal("private fixture socket path was not changed")
	}
	if err = os.WriteFile(configPath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPath(t, filepath.Join(base, "old"))

	recovered, started, err := RecoverSavedWithPair(context.Background(), saved, currentClient, currentDaemon, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatalf("recovery with current pair failed against the reachable legacy endpoint: %v", err)
	}
	if started {
		t.Fatal("recovery started a daemon while the legacy endpoint was reachable")
	}
	if recovered.runtimeBinding.ClientExecutable != currentClient {
		t.Fatalf("recovery did not use the current client executable: %q", recovered.runtimeBinding.ClientExecutable)
	}
	if recovered.runtimeBinding.Endpoint != saved.Endpoint {
		t.Fatalf("recovery did not retain the legacy endpoint after config drift: got=%q want=%q", recovered.runtimeBinding.Endpoint, saved.Endpoint)
	}
	if shutdownErr := recovered.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatal(shutdownErr)
	}
	if waitErr := recovered.WaitUnavailable(context.Background()); waitErr != nil {
		t.Fatal(waitErr)
	}

	restarted, started, err := RecoverSavedWithPair(context.Background(), saved, currentClient, currentDaemon, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatalf("recovery with current pair failed after the legacy install was removed: %v", err)
	}
	if !started {
		t.Fatal("recovery did not start the verified current daemon after the legacy endpoint became unavailable")
	}
	if restarted.runtimeBinding.DaemonExecutable != currentDaemon {
		t.Fatalf("recovery did not use the current daemon executable: %q", restarted.runtimeBinding.DaemonExecutable)
	}
	if shutdownErr := restarted.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatal(shutdownErr)
	}
	if waitErr := restarted.WaitUnavailable(context.Background()); waitErr != nil {
		t.Fatal(waitErr)
	}
}

//nolint:gocyclo // This test intentionally covers the complete persisted-alias recovery transaction.
func TestRecoverPrivateUsesPersistedLegacyBindingAfterConfigDrift(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	saved := first.Binding()
	data, err := os.ReadFile(saved.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	oldSocket := strconv.Quote(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.sock"))
	newSocket := strconv.Quote(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "current.sock"))
	changed := strings.Replace(string(data), "unix_socket_path: "+oldSocket, "unix_socket_path: "+newSocket, 1)
	if changed == string(data) {
		t.Fatal("private fixture socket path was not changed")
	}
	if err = os.WriteFile(saved.ConfigPath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}

	legacy, started, err := RecoverSavedWithPair(context.Background(), saved, fixture.clientPath, fixture.daemonPath, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatalf("legacy recovery failed: %v", err)
	}
	if started {
		t.Fatal("legacy recovery started a daemon while the endpoint was reachable")
	}
	if err = legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatalf("persist legacy recovery binding: %v", err)
	}
	if _, err = os.Stat(privateRecoveryRecordPath(fixture.stateRoot)); err != nil {
		t.Fatalf("legacy recovery record was not published: %v", err)
	}

	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, saved, fixture.clientPath, fixture.daemonPath, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatalf("recovery did not use the persisted legacy binding: %v", err)
	}
	if recovered.runtimeBinding.ConfigPath != privateRecoveryConfigPath(fixture.stateRoot) {
		t.Fatalf("recovery config path = %q, want durable legacy path", recovered.runtimeBinding.ConfigPath)
	}
	if recovered.runtimeBinding.Endpoint != saved.Endpoint {
		t.Fatalf("recovery endpoint = %q, want %q", recovered.runtimeBinding.Endpoint, saved.Endpoint)
	}
	if err = recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if err = ClearPrivateRecoveryBinding(fixture.stateRoot, saved); err != nil {
		t.Fatalf("clear persisted legacy recovery binding: %v", err)
	}
	if _, err = os.Stat(privateRecoveryRecordPath(fixture.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy recovery record remained after clear: %v", err)
	}
}

func TestPrivateIdentityPublicationRecoversTornPair(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	current := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	successorDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "successor-pueued")
	writeExecutable(t, successorDaemon, privateDaemonFixture+"\n# successor\n")
	successor := current.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, successorDaemon)
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, successorDaemon)
	pendingData, err := task.MarshalCanonical(privateIdentityPublicationRecord{
		SchemaVersion: task.SchemaVersion,
		Binding:       successor,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(current.Binding().ConfigPath)
	if err = publishPrivateRecord(base, privateIdentityPublicationPath(fixture.stateRoot), pendingData); err != nil {
		t.Fatal(err)
	}

	if err = WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return recoverPrivateIdentityPublication(fixture.stateRoot)
	}); err != nil {
		t.Fatalf("torn private identity publication was not recovered: %v", err)
	}
	identity := readPrivateDaemonIdentity(t, fixture.stateRoot)
	if identity.Executable != successor.DaemonExecutable || identity.SHA256 != successor.DaemonSHA256 {
		t.Fatalf("daemon identity was not replayed from the publication journal: %+v", identity)
	}
	persisted, found, err := loadPrivateSupervisorBinding(fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !task.SameSupervisorIdentity(persisted, successor) {
		t.Fatalf("logical identity was not replayed from the publication journal: found=%t binding=%+v want=%+v", found, persisted, successor)
	}
	if _, err = os.Stat(privateIdentityPublicationPath(fixture.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private identity publication journal remained: %v", err)
	}
}

func TestPrivateRecoveryPublicationRecoversAfterConfigOnlyStage(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	data, err := os.ReadFile(legacy.runtimeBinding.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	record := privateRecoveryBindingRecord{
		SchemaVersion: task.SchemaVersion,
		Supervisor:    legacy.Binding(),
		ConfigPath:    privateRecoveryConfigPath(fixture.stateRoot),
		ConfigSHA256:  task.ComputeSHA256(data),
	}
	pendingData, err := task.MarshalCanonical(privateRecoveryPublicationRecord{
		SchemaVersion: task.SchemaVersion,
		Binding:       record,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if err = publishPrivateRecord(base, privateRecoveryPendingConfigPath(fixture.stateRoot), data); err != nil {
		t.Fatal(err)
	}
	if err = publishPrivateRecord(base, privateRecoveryPendingRecordPath(fixture.stateRoot), pendingData); err != nil {
		t.Fatal(err)
	}
	if err = publishPrivateRecord(base, record.ConfigPath, data); err != nil {
		t.Fatal(err)
	}

	if err = WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return recoverPrivateRecoveryPublication(fixture.stateRoot)
	}); err != nil {
		t.Fatalf("staged legacy recovery publication was not recovered: %v", err)
	}
	got, found, err := readPrivateRecoveryBinding(fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !task.SameSupervisorIdentity(got.Supervisor, record.Supervisor) || got.ConfigSHA256 != record.ConfigSHA256 {
		t.Fatalf("legacy recovery publication was not restored: found=%t record=%+v want=%+v", found, got, record)
	}
	for _, path := range []string{privateRecoveryPendingConfigPath(fixture.stateRoot), privateRecoveryPendingRecordPath(fixture.stateRoot)} {
		if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy recovery publication stage remained at %s: %v", path, err)
		}
	}
}

func TestPrivateRecoveryPublicationReconcilesCommittedPairWithoutStageConfig(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	data, err := os.ReadFile(legacy.runtimeBinding.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	record := privateRecoveryBindingRecord{
		SchemaVersion: task.SchemaVersion,
		Supervisor:    legacy.Binding(),
		ConfigPath:    privateRecoveryConfigPath(fixture.stateRoot),
		ConfigSHA256:  task.ComputeSHA256(data),
	}
	pendingData, err := task.MarshalCanonical(privateRecoveryPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: record})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if err = publishPrivateRecord(base, record.ConfigPath, data); err != nil {
		t.Fatal(err)
	}
	finalData, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = publishPrivateRecord(base, privateRecoveryRecordPath(fixture.stateRoot), finalData); err != nil {
		t.Fatal(err)
	}
	if err = publishPrivateRecord(base, privateRecoveryPendingRecordPath(fixture.stateRoot), pendingData); err != nil {
		t.Fatal(err)
	}
	if err = WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return recoverPrivateRecoveryPublication(fixture.stateRoot)
	}); err != nil {
		t.Fatalf("committed recovery publication was not reconciled: %v", err)
	}
	if _, err = os.Stat(privateRecoveryPendingRecordPath(fixture.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending recovery journal remained after reconciliation: %v", err)
	}
}

func TestFreshPrivateConfigRecordsBootstrapIntentBeforePublication(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	resolution, err := CurrentResolutionContext()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ensurePrivateConfig(fixture.stateRoot, resolution); err != nil {
		t.Fatal(err)
	}
	pending, err := PrivateBootstrapPending(fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("fresh private config was published without bootstrap intent")
	}
	if _, err = BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout}); err != nil {
		t.Fatalf("partial private bootstrap was not recoverable: %v", err)
	}
	pending, err = PrivateBootstrapPending(fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("completed private bootstrap retained its pending marker")
	}
}

func TestFreshPrivateConfigUsesCompactDarwinSocketWithinNativeLimit(t *testing.T) {
	stateRoot, resolution := newLongDarwinPrivateStateRoot(t)
	configPath, base, err := ensurePrivateConfig(stateRoot, resolution)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := readPrivateConfig(configPath, base, resolution)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "s")
	if resolved.SocketPath != want {
		t.Fatalf("socket path = %q, want %q", resolved.SocketPath, want)
	}
	limit, ok := unixSocketPathLimit("darwin")
	if !ok || len(resolved.SocketPath) > limit {
		t.Fatalf("socket path length = %d, Darwin limit = %d", len(resolved.SocketPath), limit)
	}
	pending, err := PrivateBootstrapPending(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("fresh compact config was published without bootstrap intent")
	}
}

func TestFreshPrivateConfigRejectsUnrepresentableDarwinSocketBeforePublication(t *testing.T) {
	stateRoot, resolution := newPrivateStateRootAtBaseLength(t, 104)
	if _, _, err := ensurePrivateConfig(stateRoot, resolution); !errors.Is(err, errUnixSocketPathTooLong) {
		t.Fatalf("unrepresentable socket error = %v", err)
	}
	if _, err := os.Stat(PrivateConfigPath(stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid config was published: %v", err)
	}
	pending, err := PrivateBootstrapPending(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("invalid config published bootstrap intent")
	}
}

func TestExistingValidPrivateConfigRemainsByteIdentical(t *testing.T) {
	stateRoot, resolution := newLongDarwinPrivateStateRoot(t)
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		t.Fatal(err)
	}
	if err := markPrivateBootstrapPending(stateRoot); err != nil {
		t.Fatal(err)
	}
	source := privateConfigYAMLWithSocket(base, filepath.Join(base, "x"))
	configPath := PrivateConfigPath(stateRoot)
	if err := createPrivateConfig(configPath, base, source); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensurePrivateConfig(stateRoot, resolution); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != source {
		t.Fatal("existing valid config bytes changed")
	}
}

func TestPendingPrivateConfigRecoversReplacementVisibleBeforeDirectoryBarrier(t *testing.T) {
	stateRoot, resolution := newLongDarwinPrivateStateRoot(t)
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		t.Fatal(err)
	}
	if err := markPrivateBootstrapPending(stateRoot); err != nil {
		t.Fatal(err)
	}
	configPath := PrivateConfigPath(stateRoot)
	if err := createPrivateConfig(configPath, base, privateConfigYAML(base)); err != nil {
		t.Fatal(err)
	}
	replacement, err := privateConfigYAMLForResolution(base, resolution)
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(base, ".interrupted-config")
	if err = os.WriteFile(stage, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(stage, configPath); err != nil {
		t.Fatal(err)
	}

	if _, _, err = ensurePrivateConfig(stateRoot, resolution); err != nil {
		t.Fatalf("visible replacement was not recovered: %v", err)
	}
	resolved, err := readPrivateConfig(configPath, base, resolution)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SocketPath != filepath.Join(base, "s") {
		t.Fatalf("recovered socket path = %q", resolved.SocketPath)
	}
}

func TestConcurrentCompactPrivateConfigBootstrapConverges(t *testing.T) {
	stateRoot, resolution := newLongDarwinPrivateStateRoot(t)
	const callers = 8
	start := make(chan struct{})
	errorsByCaller := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errorsByCaller <- WithBootstrapLock(context.Background(), stateRoot, func() error {
				_, _, err := ensurePrivateConfig(stateRoot, resolution)
				return err
			})
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := readPrivateConfig(
		PrivateConfigPath(stateRoot),
		filepath.Join(stateRoot, privateSupervisorDirectory),
		resolution,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SocketPath != filepath.Join(stateRoot, privateSupervisorDirectory, "s") {
		t.Fatalf("concurrent bootstrap socket path = %q", resolved.SocketPath)
	}
}

func TestIncompletePrivateConfigRepairsOwnedLegacyDarwinPath(t *testing.T) {
	stateRoot, resolution, base, configPath, _ := newIncompletePrivateConfig(t, true, false)
	if err := validatePrivateConfig(configPath, base, resolution); !errors.Is(err, errUnixSocketPathTooLong) {
		t.Fatalf("legacy config validation = %v", err)
	}

	gotPath, gotBase, err := ensurePrivateConfig(stateRoot, resolution)
	if err != nil {
		t.Fatalf("owned incomplete config was not repaired: %v", err)
	}
	if gotPath != configPath || gotBase != base {
		t.Fatalf("repair changed config identity: path=%q base=%q", gotPath, gotBase)
	}
	resolved, err := readPrivateConfig(configPath, base, resolution)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SocketPath != filepath.Join(base, "s") {
		t.Fatalf("repaired socket path = %q", resolved.SocketPath)
	}
	if _, _, err = ensurePrivateConfig(stateRoot, resolution); err != nil {
		t.Fatalf("repaired config was not idempotent: %v", err)
	}
}

func TestIncompletePrivateConfigRepairRefusesAmbiguousState(t *testing.T) {
	for _, test := range []struct {
		name     string
		pending  bool
		changed  bool
		artifact string
	}{
		{name: "missing bootstrap ownership"},
		{name: "changed config", pending: true, changed: true},
		{name: "daemon identity present", pending: true, artifact: "identity"},
		{name: "recovery alias present", pending: true, artifact: "recovery"},
		{name: "migration history present", pending: true, artifact: "history"},
		{name: "compact endpoint present", pending: true, artifact: "endpoint"},
		{name: "runtime state present", pending: true, artifact: "runtime"},
		{name: "data state present", pending: true, artifact: "data"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateRoot, resolution, base, configPath, source := newIncompletePrivateConfig(t, test.pending, test.changed)
			addIncompletePrivateConfigArtifact(t, stateRoot, base, test.artifact)

			_, _, err := ensurePrivateConfig(stateRoot, resolution)
			if !errors.Is(err, errUnixSocketPathTooLong) {
				t.Fatalf("unsafe repair was not refused: %v", err)
			}
			got, readErr := os.ReadFile(configPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != source {
				t.Fatal("refused repair changed the existing config")
			}
		})
	}
}

func newIncompletePrivateConfig(t *testing.T, pending, changed bool) (string, ResolutionContext, string, string, string) {
	t.Helper()
	stateRoot, resolution := newLongDarwinPrivateStateRoot(t)
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		t.Fatal(err)
	}
	if pending {
		if err := markPrivateBootstrapPending(stateRoot); err != nil {
			t.Fatal(err)
		}
	}
	source := privateConfigYAML(base)
	if changed {
		source += "\n"
	}
	configPath := PrivateConfigPath(stateRoot)
	if err := createPrivateConfig(configPath, base, source); err != nil {
		t.Fatal(err)
	}
	return stateRoot, resolution, base, configPath, source
}

func addIncompletePrivateConfigArtifact(t *testing.T, stateRoot, base, artifact string) {
	t.Helper()
	if addIncompletePrivatePathArtifact(t, base, artifact) {
		return
	}
	switch artifact {
	case "":
		return
	case "identity":
		data, err := task.MarshalCanonical(privateDaemonIdentity{
			Executable: "/bin/sh",
			SHA256:     strings.Repeat("a", 64),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = publishPrivateRecord(base, privateDaemonIdentityPath(base), data); err != nil {
			t.Fatal(err)
		}
	case "recovery":
		if err := os.WriteFile(privateRecoveryPendingConfigPath(stateRoot), []byte("retained"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "history":
		data, err := task.MarshalCanonical(privateMigrationHistory{
			SchemaVersion: task.SchemaVersion,
			Migrations:    []privateMigrationRecord{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = publishPrivateRecord(base, privateMigrationHistoryPath(base), data); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown incomplete-config artifact %q", artifact)
	}
}

func addIncompletePrivatePathArtifact(t *testing.T, base, artifact string) bool {
	t.Helper()
	switch artifact {
	case "endpoint":
		if err := os.WriteFile(filepath.Join(base, "s"), []byte("retained"), 0o600); err != nil {
			t.Fatal(err)
		}
		return true
	case "runtime":
		if err := os.Mkdir(filepath.Join(base, "run"), 0o700); err != nil {
			t.Fatal(err)
		}
		return true
	case "data":
		if err := os.Mkdir(filepath.Join(base, "data"), 0o700); err != nil {
			t.Fatal(err)
		}
		return true
	default:
		return false
	}
}

func newLongDarwinPrivateStateRoot(t *testing.T) (string, ResolutionContext) {
	t.Helper()
	stateRoot, resolution := newPrivateStateRootAtBaseLength(t, 100)
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	limit, ok := unixSocketPathLimit("darwin")
	if !ok || len(filepath.Join(base, "run", "pueue.sock")) <= limit || len(filepath.Join(base, "s")) > limit {
		t.Fatal("Darwin socket boundary fixture does not straddle the native limit")
	}
	return stateRoot, resolution
}

func newPrivateStateRootAtBaseLength(t *testing.T, targetBaseLength int) (string, ResolutionContext) {
	t.Helper()
	parent := canonicalTemp(t)
	padding := targetBaseLength - len(parent) - 2 - len(privateSupervisorDirectory)
	if padding < 1 {
		t.Fatalf("temporary parent is too long for Darwin boundary fixture: %q", parent)
	}
	stateRoot := filepath.Join(parent, strings.Repeat("s", padding))
	writePrivateStateRootRecord(t, stateRoot)
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if len(base) != targetBaseLength {
		t.Fatalf("private base length = %d, want %d", len(base), targetBaseLength)
	}
	return stateRoot, ResolutionContext{
		OS:       "darwin",
		Home:     "/Users/delegate-test",
		Username: "delegate-test",
		Cwd:      parent,
	}
}

func TestMigratePrivateSupervisorRecoversMissingLogicalBindingFromLivePredecessor(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	if _, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, options); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(privateSupervisorBindingPath(stateRoot)); err != nil {
		t.Fatal(err)
	}
	current, err := BindPrivate(context.Background(), clientPath, currentDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = WithBootstrapLock(context.Background(), stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); err != nil {
		t.Fatalf("migration did not recover missing logical binding: %v", err)
	}
	persisted, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !task.SameSupervisorIdentity(persisted, current.Binding()) {
		t.Fatalf("successor logical binding was not published: found=%t binding=%+v want=%+v", found, persisted, current.Binding())
	}
}

func TestMigratePrivateSupervisorRejectsChangedConfigWithoutLogicalPredecessorProof(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	legacy, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	legacyBinding := legacy.Binding()
	t.Cleanup(func() {
		removePrivateTestDaemonState(t, stateRoot)
	})
	identityPath := privateDaemonIdentityPath(filepath.Join(stateRoot, privateSupervisorDirectory))
	writePrivateSuccessorTestRecord(t, identityPath, privateDaemonIdentity{
		Executable: legacyBinding.DaemonExecutable,
		SHA256:     legacyBinding.DaemonSHA256,
	})
	configData, err := os.ReadFile(legacyBinding.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	configData = append(configData, []byte("# changed logical generation\n")...)
	if writeErr := os.WriteFile(legacyBinding.ConfigPath, configData, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if removeErr := os.Remove(privateSupervisorBindingPath(stateRoot)); removeErr != nil {
		t.Fatal(removeErr)
	}
	current, err := Bind(context.Background(), clientPath, legacyBinding.ConfigPath, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := attachPrivateDaemon(current, currentDaemonPath); err != nil {
		t.Fatal(err)
	}
	identityBefore := readPrivateSupervisorFile(t, identityPath)
	configBefore := readPrivateSupervisorFile(t, legacyBinding.ConfigPath)
	if err := WithBootstrapLock(context.Background(), stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("changed config was accepted without durable predecessor provenance: %v", err)
	}
	assertPrivateDaemonStartCount(t, stateRoot, 1)
	if got := readPrivateSupervisorFile(t, identityPath); !bytes.Equal(got, identityBefore) {
		t.Fatal("changed-config refusal rewrote daemon provenance")
	}
	if got := readPrivateSupervisorFile(t, legacyBinding.ConfigPath); !bytes.Equal(got, configBefore) {
		t.Fatal("changed-config refusal rewrote the saved config")
	}
	if _, err := os.Stat(privateSupervisorBindingPath(stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed-config refusal published a logical binding: %v", err)
	}
}

func TestMigratePrivateSupervisorRejectsSemanticallyChangedCompleteIdentityWithoutRelabeling(t *testing.T) {
	fixture := newSemanticallyChangedPrivateMigrationFixture(t)
	t.Cleanup(func() {
		removePrivateTestDaemonState(t, fixture.stateRoot)
	})

	bindingPath := privateSupervisorBindingPath(fixture.stateRoot)
	historyPath := privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))
	bindingBefore, bindingPresentBefore := snapshotPrivateTestFile(t, bindingPath)
	historyBefore, historyPresentBefore := snapshotPrivateTestFile(t, historyPath)
	identityBefore := readPrivateSupervisorFile(t, fixture.identityPath)
	configBefore := readPrivateSupervisorFile(t, fixture.configPath)
	err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return fixture.current.MigratePrivateSupervisorWithLock(context.Background(), fixture.stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("live daemon response through changed generation was accepted as predecessor proof: %v", err)
	}
	assertPrivateTestFileUnchanged(t, bindingPath, bindingBefore, bindingPresentBefore)
	assertPrivateTestFileUnchanged(t, historyPath, historyBefore, historyPresentBefore)
	if got := readPrivateSupervisorFile(t, fixture.identityPath); !bytes.Equal(got, identityBefore) {
		t.Fatal("semantic-generation refusal changed daemon provenance")
	}
	if got := readPrivateSupervisorFile(t, fixture.configPath); !bytes.Equal(got, configBefore) {
		t.Fatal("semantic-generation refusal changed the current config")
	}
}

type semanticallyChangedPrivateMigrationFixture struct {
	stateRoot    string
	configPath   string
	identityPath string
	legacy       *Client
	current      *Client
}

func newSemanticallyChangedPrivateMigrationFixture(t *testing.T) semanticallyChangedPrivateMigrationFixture {
	t.Helper()
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	legacy, bindErr := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if bindErr != nil {
		t.Fatal(bindErr)
	}
	legacyBinding := legacy.Binding()
	identityPath := privateDaemonIdentityPath(filepath.Join(stateRoot, privateSupervisorDirectory))
	writePrivateSuccessorTestRecord(t, identityPath, privateDaemonIdentityForBinding(legacyBinding))
	configPath := legacyBinding.ConfigPath
	configData := readPrivateSupervisorFile(t, configPath)
	const showConfirmation = "  show_confirmation_questions: false\n"
	semanticConfig := strings.Replace(string(configData), showConfirmation, showConfirmation+"  edit_mode: files\n", 1)
	if semanticConfig == string(configData) {
		t.Fatal("test fixture did not find the client configuration anchor")
	}
	if writeErr := os.WriteFile(configPath, []byte(semanticConfig), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if removeErr := os.Remove(privateSupervisorBindingPath(stateRoot)); removeErr != nil {
		t.Fatal(removeErr)
	}
	current, currentErr := Bind(context.Background(), clientPath, configPath, options)
	if currentErr != nil {
		t.Fatal(currentErr)
	}
	if attachErr := attachPrivateDaemon(current, daemonPath); attachErr != nil {
		t.Fatal(attachErr)
	}
	if task.SameSupervisorIdentity(current.Binding(), legacyBinding) {
		t.Fatal("semantic config change did not create a distinct logical generation")
	}
	if current.Binding().Endpoint != legacyBinding.Endpoint {
		t.Fatalf("semantic config fixture changed endpoint: current=%+v legacy=%+v", current.Binding(), legacyBinding)
	}
	ready, readyErr := current.availableForBootstrap(context.Background())
	if readyErr != nil || !ready {
		t.Fatalf("live predecessor was not reachable through the changed generation: ready=%t err=%v", ready, readyErr)
	}
	return semanticallyChangedPrivateMigrationFixture{stateRoot: stateRoot, configPath: configPath, identityPath: identityPath, legacy: legacy, current: current}
}

func TestRetirePrivateRecoveryBindingRemovesStaleAliasAfterSuccessorReady(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "current-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# successor\n")
	successor := legacy.Binding()
	successor.ConfigDigest = task.ComputeSHA256([]byte("successor-config"))
	successor.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("successor-resolved-config"))
	successor.Endpoint = "unix:/successor.sock"
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, currentDaemon)
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, currentDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return RetirePrivateRecoveryBinding(fixture.stateRoot, successor)
	}); err != nil {
		t.Fatalf("stale recovery alias was not retired: %v", err)
	}
	if _, err := os.Stat(privateRecoveryRecordPath(fixture.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale recovery record remained after retirement: %v", err)
	}
}

func TestRetirePrivateRecoveryBindingContextRejectsCanceledContextWithoutMutation(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	paths := []string{privateRecoveryRecordPath(fixture.stateRoot), privateRecoveryConfigPath(fixture.stateRoot)}
	before := snapshotPrivateTestFiles(t, paths)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RetirePrivateRecoveryBindingContext(ctx, fixture.stateRoot, legacy.Binding()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled alias retirement error = %v, want context cancellation", err)
	}
	after := snapshotPrivateTestFiles(t, paths)
	for path, want := range before {
		if got, found := after[path]; !found || !bytes.Equal(got, want) {
			t.Fatalf("canceled alias retirement changed %s", path)
		}
	}
}

func TestRetirePrivateRecoveryBindingRemovesAliasForSameSuccessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "same-logical-successor-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# same logical successor\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, currentDaemon)
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, currentDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return RetirePrivateRecoveryBinding(fixture.stateRoot, successor)
	}); err != nil {
		t.Fatalf("same-identity recovery alias was not retired: %v", err)
	}
	if _, err := os.Stat(privateRecoveryRecordPath(fixture.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("same-identity recovery record remained after retirement: %v", err)
	}
}

func TestRetirePrivateRecoveryBindingRejectsUnrelatedSuccessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "current-pueued")
	otherDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "other-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# current successor\n")
	writeExecutable(t, otherDaemon, privateDaemonFixture+"\n# unrelated successor\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, currentDaemon)
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, currentDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	recordBefore := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot))
	configBefore := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot))
	unrelated := successor
	unrelated.DaemonExecutable = canonicalPrivateTestExecutable(t, otherDaemon)
	unrelated.DaemonSHA256 = hashPrivateTestExecutable(t, otherDaemon)
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return RetirePrivateRecoveryBinding(fixture.stateRoot, unrelated)
	}); err == nil {
		t.Fatal("unrelated successor was allowed to retire the active recovery alias")
	}
	if _, err := os.Stat(privateRecoveryRecordPath(fixture.stateRoot)); err != nil {
		t.Fatalf("recovery record was removed after unrelated retirement attempt: %v", err)
	}
	if _, err := os.Stat(privateRecoveryConfigPath(fixture.stateRoot)); err != nil {
		t.Fatalf("recovery config was removed after unrelated retirement attempt: %v", err)
	}
	if got := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot)); !bytes.Equal(got, recordBefore) {
		t.Fatal("unrelated retirement changed the recovery record")
	}
	if got := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot)); !bytes.Equal(got, configBefore) {
		t.Fatal("unrelated retirement changed the recovery config")
	}
}

func TestRetirePrivateRecoveryBindingRejectsMismatchedSuccessorHistory(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "history-current-pueued")
	otherDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "history-other-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# current successor\n")
	writeExecutable(t, otherDaemon, privateDaemonFixture+"\n# history mismatch\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, currentDaemon)
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, currentDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	historyPath := privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))
	history, found, err := loadPrivateMigrationHistory(filepath.Dir(historyPath))
	if err != nil || !found {
		t.Fatalf("successor history was not seeded: found=%t err=%v", found, err)
	}
	history.Migrations[0].Successor.Executable = canonicalPrivateTestExecutable(t, otherDaemon)
	history.Migrations[0].Successor.SHA256 = hashPrivateTestExecutable(t, otherDaemon)
	writePrivateSuccessorTestRecord(t, historyPath, history)
	recordBefore := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot))
	configBefore := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot))
	if err := RetirePrivateRecoveryBinding(fixture.stateRoot, successor); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("mismatched successor history was accepted: %v", err)
	}
	if got := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot)); !bytes.Equal(got, recordBefore) {
		t.Fatal("mismatched successor history changed the recovery record")
	}
	if got := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot)); !bytes.Equal(got, configBefore) {
		t.Fatal("mismatched successor history changed the recovery config")
	}
}

func TestRetirePrivateRecoveryBindingRejectsIncompletePredecessorShutdown(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	successorDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "incomplete-successor-pueued")
	writeExecutable(t, successorDaemon, privateDaemonFixture+"\n# incomplete-history successor\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, successorDaemon)
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, successorDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	historyPath := privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))
	writePrivateSuccessorTestRecord(t, historyPath, privateMigrationHistory{
		SchemaVersion: task.SchemaVersion,
		Migrations: []privateMigrationRecord{{
			SchemaVersion: task.SchemaVersion,
			Legacy:        privateDaemonIdentityForBinding(legacy.Binding()),
			Successor:     privateDaemonIdentityForBinding(successor),
			LegacyPID:     101,
			LegacyStopped: false,
		}},
	})

	paths := []string{
		privateRecoveryConfigPath(fixture.stateRoot),
		privateRecoveryRecordPath(fixture.stateRoot),
		privateRecoveryPendingConfigPath(fixture.stateRoot),
		privateRecoveryPendingRecordPath(fixture.stateRoot),
	}
	before := snapshotPrivateTestFiles(t, paths)
	if err := RetirePrivateRecoveryBinding(fixture.stateRoot, successor); !errors.Is(err, ErrBinding) {
		t.Fatalf("incomplete predecessor shutdown history was accepted: %v", err)
	}
	after := snapshotPrivateTestFiles(t, paths)
	if len(after) != len(before) {
		t.Fatalf("incomplete-history refusal changed recovery alias artifact set: before=%d after=%d", len(before), len(after))
	}
	for path, want := range before {
		got, ok := after[path]
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("incomplete-history refusal changed recovery alias artifact %s", path)
		}
	}
}

func seedPrivateRecoveryRetirementSuccessor(t *testing.T, stateRoot string, legacy, successor task.SupervisorRef) {
	t.Helper()
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := publishPrivateDaemonIdentity(base, successor); err != nil {
		t.Fatal(err)
	}
	if err := appendPrivateMigrationHistory(base, privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacy),
		Successor:     privateDaemonIdentityForBinding(successor),
		LegacyPID:     101,
		LegacyStopped: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRetirePrivateRecoveryBindingRetriesOrphanedConfig(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	successorDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "orphan-successor-pueued")
	writeExecutable(t, successorDaemon, privateDaemonFixture+"\n# orphan cleanup successor\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, successorDaemon)
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, successorDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := os.Remove(privateRecoveryRecordPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	if err := RetirePrivateRecoveryBinding(fixture.stateRoot, successor); err != nil {
		t.Fatalf("orphaned recovery config was not retired: %v", err)
	}
	if _, err := os.Stat(privateRecoveryConfigPath(fixture.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned recovery config remained after retirement: %v", err)
	}
}

func TestRetirePrivateRecoveryBindingRejectsAmbiguousOrphanedConfig(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(privateRecoveryRecordPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	configBefore := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot))
	if err := RetirePrivateRecoveryBinding(fixture.stateRoot, legacy.Binding()); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("ambiguous orphaned recovery config was retired: %v", err)
	}
	if got := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot)); !bytes.Equal(got, configBefore) {
		t.Fatal("ambiguous orphaned recovery config changed")
	}
}

func TestRetirePrivateRecoveryBindingCleansPartiallyRemovedRecord(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	successorDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "partial-successor-pueued")
	writeExecutable(t, successorDaemon, privateDaemonFixture+"\n# partial cleanup successor\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, successorDaemon)
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, successorDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := os.Remove(privateRecoveryConfigPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	if err := RetirePrivateRecoveryBinding(fixture.stateRoot, successor); err != nil {
		t.Fatalf("partially removed recovery record was not retired: %v", err)
	}
	for _, path := range []string{privateRecoveryRecordPath(fixture.stateRoot), privateRecoveryConfigPath(fixture.stateRoot)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial recovery artifact remained at %s: %v", path, err)
		}
	}
}

func TestSelectRecoveryDaemonUsesFallbackAfterSavedBytesChange(t *testing.T) {
	base := canonicalTemp(t)
	savedPath := filepath.Join(base, "saved-pueued")
	fallbackPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, savedPath, "#!/bin/sh\nold\n")
	writeExecutable(t, fallbackPath, "#!/bin/sh\ncurrent\n")
	savedDigest, err := hashExecutable(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(savedPath, []byte("#!/bin/sh\nreplaced\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	saved := task.SupervisorRef{DaemonExecutable: savedPath, DaemonSHA256: savedDigest}
	got, digest, err := selectRecoveryDaemon(saved, fallbackPath)
	if err != nil {
		t.Fatalf("select recovery daemon failed: %v", err)
	}
	want, err := canonicalExecutable(fallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := hashExecutable(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || digest != wantDigest {
		t.Fatalf("fallback daemon = (%q, %q), want (%q, %q)", got, digest, want, wantDigest)
	}
}

func TestFinishPrivateBootstrapRetainsLockForEveryPendingCommand(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireBootstrapLock(context.Background(), filepath.Join(privateBase, "bootstrap.lock"))
	if err != nil {
		t.Fatal(err)
	}
	releasePath := filepath.Join(base, "release")
	waitScript := `while [ ! -f "$1" ]; do sleep 0.01; done`
	first := startOwned(exec.Command("sh", "-c", waitScript, "wait", releasePath))
	second := startOwned(exec.Command("sh", "-c", waitScript, "wait", releasePath))
	resultErr := errors.Join(&InFlightError{Pending: first}, &InFlightError{Pending: second})
	finishPrivateBootstrap(&lock, &resultErr)

	lockContext, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	deferredLock, lockErr := acquireBootstrapLock(lockContext, filepath.Join(privateBase, "bootstrap.lock"))
	cancel()
	if deferredLock != nil {
		if closeErr := closeBootstrapLock(deferredLock); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !errors.Is(lockErr, context.DeadlineExceeded) {
		t.Fatalf("bootstrap lock was released before every pending command completed: %v", lockErr)
	}
	if writeErr := os.WriteFile(releasePath, nil, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if !awaitPendingNaturally(first, time.Second) || !awaitPendingNaturally(second, time.Second) {
		t.Fatal("pending bootstrap commands did not complete")
	}
	lock, err = acquireBootstrapLock(context.Background(), filepath.Join(privateBase, "bootstrap.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err = closeBootstrapLock(lock); err != nil {
		t.Fatal(err)
	}
}

func TestRetainBootstrapLockReportsDeferredReleaseFailure(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "closed-bootstrap-lock-")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	lock := &bootstrapLock{file: file}
	done := make(chan struct{})
	close(done)
	reported := make(chan error, 1)

	retainBootstrapLock(&lock, []*Pending{{done: done}}, func(releaseErr error) {
		reported <- releaseErr
	})
	if lock != nil {
		t.Fatal("retained bootstrap lock ownership was not transferred")
	}
	select {
	case releaseErr := <-reported:
		if releaseErr == nil {
			t.Fatal("deferred bootstrap lock release reported a nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("deferred bootstrap lock release failure was discarded")
	}
}

func TestRecoverSavedCleansStagedConfigAfterProbeFailure(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	saved := first.Binding()
	configPath := saved.ConfigPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	oldSocket := strconv.Quote(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.sock"))
	newSocket := strconv.Quote(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "current.sock"))
	changed := strings.Replace(string(data), "unix_socket_path: "+oldSocket, "unix_socket_path: "+newSocket, 1)
	if changed == string(data) {
		t.Fatal("private fixture socket path was not changed")
	}
	if writeErr := os.WriteFile(configPath, []byte(changed), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	options := Options{Environment: append(os.Environ(), "PRIVATE_INVALID_STATUS=1")}
	if _, _, recoverErr := RecoverSaved(context.Background(), saved, options); recoverErr == nil {
		t.Fatal("recover unexpectedly accepted an invalid legacy status")
	}
	staged, err := filepath.Glob(filepath.Join(filepath.Dir(configPath), ".migration-pueue-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 0 {
		t.Fatalf("staged migration configs leaked after probe failure: %v", staged)
	}
}

func TestRecoverSavedRetainsStagedConfigUntilProbeCompletes(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	saved := first.Binding()
	configPath := saved.ConfigPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	oldSocket := strconv.Quote(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.sock"))
	newSocket := strconv.Quote(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "current.sock"))
	changed := strings.Replace(string(data), "unix_socket_path: "+oldSocket, "unix_socket_path: "+newSocket, 1)
	if changed == string(data) {
		t.Fatal("private fixture socket path was not changed")
	}
	if writeErr := os.WriteFile(configPath, []byte(changed), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	options := Options{
		ObservationTimeout: time.Second,
		Environment: append(
			os.Environ(),
			"PRIVATE_VERSION_RELEASE="+filepath.Join(filepath.Dir(configPath), "version-release"),
			"PRIVATE_VERSION_MARKER="+filepath.Join(filepath.Dir(configPath), "version-marker"),
		),
	}
	releasePath := filepath.Join(filepath.Dir(configPath), "version-release")
	versionMarker := filepath.Join(filepath.Dir(configPath), "version-marker")
	t.Cleanup(func() {
		if writeErr := os.WriteFile(releasePath, nil, 0o600); writeErr != nil {
			t.Error(writeErr)
		}
	})
	_, _, recoverErr := RecoverSaved(context.Background(), saved, options)
	if recoverErr == nil {
		t.Fatal("recover unexpectedly completed during the in-flight version probe")
	}
	var inFlight *InFlightError
	if !errors.As(recoverErr, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("recover lost version probe ownership: %v", recoverErr)
	}
	waitForPrivateFixtureMarker(t, versionMarker)
	staged, err := filepath.Glob(filepath.Join(filepath.Dir(configPath), ".migration-pueue-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) == 0 {
		t.Fatal("migration config was removed while its version probe was still running")
	}
	if writeErr := os.WriteFile(releasePath, nil, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if !awaitPendingNaturally(inFlight.Pending, 2*time.Second) {
		t.Fatal("version probe remained in flight")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		staged, err = filepath.Glob(filepath.Join(filepath.Dir(configPath), ".migration-pueue-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(staged) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("migration config remained after probe completion: %v", staged)
}

func TestWithBootstrapLockSerializesSupervisorTransition(t *testing.T) {
	stateRoot := filepath.Join(canonicalTemp(t), "state")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithBootstrapLock(context.Background(), stateRoot, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	lockContext, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	lock, err := acquireBootstrapLock(lockContext, filepath.Join(stateRoot, privateSupervisorDirectory, "bootstrap.lock"))
	if lock != nil {
		if closeErr := closeBootstrapLock(lock); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		<-done
		t.Fatalf("transition lock was not held during callback: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPrivateBindingsMatchPrivateConfigNotInstallation(t *testing.T) {
	base := task.SupervisorRef{
		ConfigPath:           "/state/.supervisor/pueue.yml",
		ConfigDigest:         strings.Repeat("a", 64),
		ResolvedConfigSHA256: strings.Repeat("b", 64),
		Endpoint:             "unix:/state/.supervisor/runtime/pueue.sock",
		ObservedVersion:      "pueue 4.0.4",
		ClientExecutable:     "/old/bin/pueue",
		ClientSHA256:         strings.Repeat("c", 64),
		DaemonExecutable:     "/old/libexec/pueued",
		DaemonSHA256:         strings.Repeat("d", 64),
		ResolutionOS:         "darwin",
		ResolutionHome:       "/Users/example",
		ResolutionRuntime:    "/Users/example/Library/Caches",
		ResolutionUsername:   "example",
		ResolutionCwd:        "/old/cwd",
	}
	upgraded := base
	upgraded.ObservedVersion = "pueue 4.1.0"
	upgraded.ClientExecutable = "/new/bin/pueue"
	upgraded.ClientSHA256 = strings.Repeat("e", 64)
	upgraded.DaemonExecutable = "/new/libexec/pueued"
	upgraded.DaemonSHA256 = strings.Repeat("f", 64)
	upgraded.ResolutionOS = "linux"
	upgraded.ResolutionHome = "/home/example"
	upgraded.ResolutionRuntime = "/home/example/.cache"
	upgraded.ResolutionUsername = "new-example"
	upgraded.ResolutionCwd = "/new/cwd"
	if !privateBindingsMatch(upgraded, base) {
		t.Fatal("installation provenance drift changed private supervisor identity")
	}

	for _, tc := range []struct {
		name   string
		change func(*task.SupervisorRef)
	}{
		{name: "config path", change: func(ref *task.SupervisorRef) { ref.ConfigPath += ".other" }},
		{name: "config digest", change: func(ref *task.SupervisorRef) { ref.ConfigDigest = strings.Repeat("1", 64) }},
		{name: "resolved config digest", change: func(ref *task.SupervisorRef) { ref.ResolvedConfigSHA256 = strings.Repeat("2", 64) }},
		{name: "endpoint", change: func(ref *task.SupervisorRef) { ref.Endpoint += ".other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.change(&changed)
			if privateBindingsMatch(changed, base) {
				t.Fatal("private binding matched after logical supervisor identity changed")
			}
		})
	}
}

func TestWaitReadyAcceptsLateSuccessfulStatusAfterReaping(t *testing.T) {
	fake := newFakeSupervisor(t, "delay-status")
	started := time.Now()
	if err := waitReady(context.Background(), fake.client, 20*time.Millisecond); err != nil {
		t.Fatalf("late successful readiness was rejected: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("waitReady returned before its status command was reaped: %s", elapsed)
	}
}

func TestWaitReadyAcceptsLateVersionBeforeStatus(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "ok", "pueue "+FixtureVersion)
	fake.environment = append(fake.environment, "FAKE_VERSION_DELAY=0.08")
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{
		ObservationTimeout: DefaultObservationTimeout,
		Environment:        fake.environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.options.ObservationTimeout = 20 * time.Millisecond
	if err := waitReady(context.Background(), client, time.Second); err != nil {
		t.Fatalf("late version probe prevented readiness: %v", err)
	}
	if count := countCommand(readInvocations(t, fake.logPath), "--version"); count != 1 {
		t.Fatalf("readiness repeated initial version discovery: %d probes", count)
	}
}

func TestPrivateClientReadyAcceptsLateVersionBeforeStatus(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "ok", "pueue "+FixtureVersion)
	fake.environment = append(fake.environment, "FAKE_VERSION_DELAY=0.08")
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{
		ObservationTimeout: DefaultObservationTimeout,
		Environment:        fake.environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.options.ObservationTimeout = 20 * time.Millisecond
	ready, err := privateClientReady(context.Background(), client)
	if err != nil || !ready {
		t.Fatalf("late version probe did not lead to a ready client: ready=%v err=%v", ready, err)
	}
	if count := countCommand(readInvocations(t, fake.logPath), "--version"); count != 1 {
		t.Fatalf("private readiness repeated initial version discovery: %d probes", count)
	}
}

func TestWaitReadyRejectsParentCancellationAndRetainsPendingOwner(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "delay-status", "pueue "+FixtureVersion)
	statusGate := filepath.Join(filepath.Dir(fake.configPath), "status-gate")
	fake.environment = append(fake.environment, "FAKE_STATUS_GATE="+statusGate)
	t.Cleanup(func() {
		if err := os.WriteFile(statusGate, []byte("release\n"), 0o600); err != nil {
			t.Errorf("release fake status gate: %v", err)
		}
	})
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{
		ObservationTimeout: DefaultObservationTimeout,
		Environment:        fake.environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.client = client
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan error, 1)
	go func() { ready <- waitReady(ctx, fake.client, 2*time.Second) }()
	waitForFakeInvocation(t, fake.logPath, "arg=status\n")
	cancel()

	readinessErr := <-ready
	var inFlight *InFlightError
	hasPending := errors.As(readinessErr, &inFlight) && inFlight.Pending != nil
	if hasPending {
		fake.cleanupPending = inFlight.Pending
		if err = os.WriteFile(statusGate, []byte("release\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !awaitPendingNaturally(inFlight.Pending, 5*time.Second) {
			t.Fatal("cancelled readiness command did not finish within its bound")
		}
		fake.cleanupPending = nil
	}
	if !errors.Is(readinessErr, context.Canceled) {
		t.Fatalf("waitReady accepted readiness after caller cancellation: %v", readinessErr)
	}
	if !hasPending {
		t.Fatalf("waitReady did not retain ownership of the active command: %v", readinessErr)
	}
}

func TestStopRecoveredWaitsForPriorPendingProbeBeforeShutdown(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "delay-status", "pueue "+FixtureVersion)
	shutdownMarker := filepath.Join(filepath.Dir(fake.configPath), "shutdown-marker")
	fake.environment = append(
		fake.environment,
		"FAKE_STATUS_DELAY=0.3",
		"FAKE_SHUTDOWN_MARKER="+shutdownMarker,
	)
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{
		ObservationTimeout: DefaultObservationTimeout,
		Environment:        fake.environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	go func() { ready <- waitReady(ctx, client, time.Second) }()
	waitForFakeInvocation(t, fake.logPath, "arg=status\n")
	cancel()
	readinessErr := <-ready
	var inFlight *InFlightError
	if !errors.As(readinessErr, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("waitReady did not return the active probe: %v", readinessErr)
	}

	cleanup := make(chan error, 1)
	go func() {
		cleanup <- stopRecoveredSupervisor(context.Background(), client, 2*time.Second, readinessErr)
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(shutdownMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shutdown overlapped the pending readiness probe: %v", err)
	}
	if err := <-cleanup; err != nil {
		t.Fatalf("recovered supervisor cleanup failed: %v", err)
	}
	if _, err := os.Stat(shutdownMarker); err != nil {
		t.Fatalf("recovered supervisor was not shut down after probe completion: %v", err)
	}
}

func waitForFakeInvocation(t *testing.T, path, invocation string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		log, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(log), invocation) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("fake supervisor did not record invocation %q", invocation)
}

func TestWaitReadyRetriesReapedUnavailableStatus(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "delay-status-fail", "pueue "+FixtureVersion)
	readyPath := fake.statusPath + ".ready"
	fake.environment = append(fake.environment, "FAKE_READY="+readyPath)
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{
		// Keep the initial version probe independent from the short status
		// observation window. Shell startup can exceed 20ms under -race.
		ObservationTimeout: DefaultObservationTimeout,
		Environment:        fake.environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	readyWritten := make(chan error, 1)
	go func() {
		time.Sleep(120 * time.Millisecond)
		readyWritten <- os.WriteFile(readyPath, nil, 0o600)
	}()
	if err := waitReady(context.Background(), client, time.Second); err != nil {
		t.Fatalf("waitReady did not retry a transient unavailable endpoint: %v", err)
	}
	if err := <-readyWritten; err != nil {
		t.Fatal(err)
	}
}

func TestBindPrivateBoundsTimedOutVersionProbeBeforeReturning(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	marker := filepath.Join(base, "version-marker")
	release := filepath.Join(base, "version-release")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{}, 1)
	t.Cleanup(func() {
		if writeErr := os.WriteFile(release, nil, 0o600); writeErr != nil {
			t.Error(writeErr)
		}
	})
	options := Options{
		ObservationTimeout: 10 * time.Second,
		Environment: append(
			os.Environ(),
			"PRIVATE_VERSION_MARKER="+marker,
			"PRIVATE_VERSION_HOLD="+release,
		),
		Observer: func(event CommandEvent) {
			if event.Stage == "started" && strings.Contains("\x00"+strings.Join(event.Argv, "\x00")+"\x00", "\x00--version\x00") {
				select {
				case started <- struct{}{}:
				default:
				}
				cancel()
			}
		},
	}
	_, err := BindPrivate(ctx, clientPath, daemonPath, stateRoot, options)
	if err == nil {
		t.Fatal("timed out version probe unexpectedly bootstrapped the supervisor")
	}
	select {
	case <-started:
	default:
		t.Fatal("version probe did not start before the caller stopped observing")
	}
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("timed out version probe lost its pending ownership: %v", err)
	}
	select {
	case <-inFlight.Pending.Done():
		t.Fatal("version probe completed before BindPrivate returned")
	default:
	}
	if writeErr := os.WriteFile(release, nil, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if !awaitPendingNaturally(inFlight.Pending, 3*time.Second) {
		t.Fatal("version probe remained in flight after the bounded return")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "started\ncompleted\n" {
		t.Fatalf("version probe did not finish under retained ownership: %q", data)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started after a failed version probe: %v", err)
	}
}

func TestBindPrivateUsesLateSuccessfulReadinessWithoutStartingDaemon(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateBase, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	seedPrivateDaemonIdentity(t, privateBase, daemonPath)
	options := Options{
		// macOS validates a temporary shell-script executable before launch;
		// keep that fixture startup outside the short readiness observation
		// window while retaining a status command that must be reaped after it
		// exceeds the per-command observation timeout.
		ObservationTimeout: time.Second,
		Environment:        append(os.Environ(), "PRIVATE_STATUS_DELAY=1.25"),
	}
	client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("late successful readiness returned no client")
	}
	if _, err := os.Stat(filepath.Join(privateBase, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started despite a successful late readiness result: %v", err)
	}
}

func TestPreparePrivateDefersDaemonStartUntilBootstrapLock(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client, err := PreparePrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared private supervisor started before migration inspection: %v", err)
	}
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return client.StartPrivate(context.Background(), fixture.stateRoot)
	}); err != nil {
		t.Fatal(err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestBindPrivateBoundsSlowReadinessBeforeReturning(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateBase, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	seedPrivateDaemonIdentity(t, privateBase, daemonPath)
	startedMarker := filepath.Join(base, "status-started")
	releaseMarker := filepath.Join(base, "status-release")
	defer releasePrivateStatusOnCleanup(t, releaseMarker)
	options := Options{
		// The blocked status command is the fault under test. Give the preceding
		// executable probe a normal process-start window so scheduler load cannot
		// turn this into a version-probe test.
		ObservationTimeout: time.Second,
		Environment: append(
			os.Environ(),
			"PRIVATE_STATUS_STARTED="+startedMarker,
			"PRIVATE_STATUS_RELEASE="+releaseMarker,
		),
	}
	const expectedBootstrapObservationPhases = 7
	expectedBootstrapTimeout := time.Duration(expectedBootstrapObservationPhases) * options.ObservationTimeout
	deadlineLowerBound := time.Now().Add(expectedBootstrapTimeout)
	bootstrapContext, cancel, deadlineErr := privateBootstrapContext(context.Background(), options.ObservationTimeout)
	deadlineUpperBound := time.Now().Add(expectedBootstrapTimeout)
	if deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	deadline, bounded := bootstrapContext.Deadline()
	cancel()
	if !bounded || deadline.Before(deadlineLowerBound) || deadline.After(deadlineUpperBound) {
		t.Fatalf("private bootstrap deadline = %s, want seven observation windows in [%s, %s]", deadline, deadlineLowerBound, deadlineUpperBound)
	}
	client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err == nil || client != nil {
		t.Fatalf("slow readiness unexpectedly completed: client_present=%t err=%v", client != nil, err)
	}
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("slow readiness lost its pending ownership: %v", err)
	}
	args := inFlight.Pending.args
	if !strings.HasSuffix(strings.Join(args, "\x00"), "status\x00--json") {
		t.Fatalf("slow readiness pending command = %q, want status --json", args)
	}
	waitForPrivateFixtureMarker(t, startedMarker)
	select {
	case <-inFlight.Pending.Done():
		t.Fatal("blocked status command completed before fixture release")
	default:
	}
	releasePrivateStatus(t, releaseMarker)
	if !awaitPendingNaturally(inFlight.Pending, 10*time.Second) {
		t.Fatal("slow readiness remained in flight after the bounded return")
	}
	if _, err := os.Stat(filepath.Join(privateBase, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started despite a successful slow readiness result: %v", err)
	}
}

func TestBindPrivateDoesNotRestartOnMalformedReadyResponse(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateBase, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	seedPrivateDaemonIdentity(t, privateBase, daemonPath)
	options := Options{
		ObservationTimeout: privateTestObservationTimeout,
		Environment:        append(os.Environ(), "PRIVATE_INVALID_STATUS=1"),
	}
	if _, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options); !errors.Is(err, ErrBinding) {
		t.Fatalf("malformed ready response was treated as daemon absence: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started after malformed ready response: %v", err)
	}
}

func TestBindPrivateAdoptsReadyDaemonAfterExecutableDrift(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateBase, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	oldDaemonPath := filepath.Join(base, "old", "pueued")
	if err := os.MkdirAll(filepath.Dir(oldDaemonPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	seedPrivateDaemonIdentity(t, privateBase, oldDaemonPath)
	identityBefore, err := os.ReadFile(privateDaemonIdentityPath(privateBase))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(daemonPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{ObservationTimeout: privateTestObservationTimeout}); err != nil {
		t.Fatalf("compatible ready daemon was rejected after executable drift: %v", err)
	}
	if _, err = os.Stat(filepath.Join(privateBase, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second daemon started while the existing endpoint was ready: %v", err)
	}
	identityAfter, err := os.ReadFile(privateDaemonIdentityPath(privateBase))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(identityAfter, identityBefore) {
		t.Fatalf("adoption rewrote launch provenance: before=%q after=%q", identityBefore, identityAfter)
	}
}

func TestBindPrivateRejectsLogicalConfigDriftBeforeStartingSuccessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	configPath := PrivateConfigPath(fixture.stateRoot)
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := configWithSocketPath(data, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "successor.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, fixture.stateRoot)

	if _, err = BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("logical supervisor drift was not blocked before startup: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if first.Binding().Endpoint == "" {
		t.Fatal("initial private binding did not record an endpoint")
	}
}

func TestMigratePrivateSupervisorWithLockRepinsLogicalConfigDrift(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	// This exercises a full migration, not a one-second observation failure.
	// Keep its fixture readiness budget independent from race-instrumented
	// process startup so a timeout cannot strand a background test child.
	options := privateMigrationTestOptions(fixture.stateRoot, 10*time.Second)
	bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	configPath := PrivateConfigPath(fixture.stateRoot)
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := configWithSocketPath(data, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "successor.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}

	var current *Client
	err = WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		var prepareErr error
		current, prepareErr = PreparePrivateForMigrationWithLock(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
		if prepareErr != nil {
			return prepareErr
		}
		return current.MigratePrivateSupervisorWithLock(context.Background(), fixture.stateRoot)
	})
	if current != nil {
		if closeErr := current.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if err != nil {
		t.Fatalf("logical supervisor migration failed: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	if _, err := os.Stat(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "migration.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed logical migration journal remained: %v", err)
	}
}

func TestRemoveCompletedTaskUsesExactSupervisorRow(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	rootID := strings.Repeat("a", 32)
	taskID := strings.Repeat("b", 32)
	numericTaskID := int64(42)
	status := statusTestPayload(t, map[string]any{
		"42": statusTestJob(numericTaskID, "delegate:"+rootID+":"+taskID, map[string]any{"Done": map[string]any{
			"enqueued_at": statusTestTime,
			"start":       statusTestTime,
			"end":         statusTestTime,
			"result":      "Success",
		}}),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-record-kill")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := Identity{
		RootID:        rootID,
		TaskID:        taskID,
		SpecSHA256:    strings.Repeat("c", 64),
		MetaSHA256:    strings.Repeat("d", 64),
		NumericTaskID: &numericTaskID,
	}
	if err := client.RemoveCompletedTask(context.Background(), identity); err != nil {
		t.Fatalf("completed task cleanup failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-kill-task-id"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "42" {
		t.Fatalf("removed task id=%q, want 42", got)
	}
}

func TestRemoveCompletedTaskDefersExactRunningSupervisorRow(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	rootID := strings.Repeat("a", 32)
	taskID := strings.Repeat("b", 32)
	numericTaskID := int64(42)
	status := statusTestPayload(t, map[string]any{
		"42": statusTestJob(numericTaskID, "delegate:"+rootID+":"+taskID, map[string]any{"Running": map[string]any{
			"enqueued_at": statusTestTime,
			"start":       statusTestTime,
		}}),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 1}})
	if err := os.WriteFile(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-record-kill")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := Identity{
		RootID:        rootID,
		TaskID:        taskID,
		SpecSHA256:    strings.Repeat("c", 64),
		MetaSHA256:    strings.Repeat("d", 64),
		NumericTaskID: &numericTaskID,
	}
	if err := client.RemoveCompletedTask(context.Background(), identity); !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("running completed-task cleanup error=%v, want ErrCleanupDeferred", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-kill-task-id")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("running task was removed or removal marker was unavailable: %v", err)
	}
}

func TestCreatePrivateConfigPublishesCompleteFile(t *testing.T) {
	base := filepath.Join(canonicalTemp(t), "supervisor")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "pueue.yml")
	want := "private-config\n"
	if err := createPrivateConfig(configPath, base, want); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("published private config=%q, want %q", got, want)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pueue.yml-") {
			t.Fatalf("staged private config was left behind: %s", entry.Name())
		}
	}
}

func TestBindPrivateBootstrapsAndReusesSupervisor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if got := first.Binding().ObservedVersion; got != "pueue 99.7.3" {
		t.Fatalf("runtime version was not preserved: %q", got)
	}
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, first.Binding())
	identityAtStart := readPrivateSupervisorFile(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "daemon-identity-at-start"))
	if string(identityAtStart) != "absent\n" {
		t.Fatalf("launch provenance was published before daemon readiness: %q", identityAtStart)
	}
	assertPrivateConfigRootedInState(t, fixture.stateRoot)
	if err := WithBootstrapLock(t.Context(), fixture.stateRoot, func() error {
		_, configureErr := first.SetPrivateDefaultParallelTasksWithBootstrapLock(t.Context(), fixture.stateRoot, 0)
		return configureErr
	}); err != nil {
		t.Fatalf("private admission did not configure unlimited ordinary parallelism: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "parallel-unlimited")); err != nil {
		t.Fatalf("private bootstrap did not configure unlimited ordinary parallelism: %v", err)
	}

	second := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if second.Binding().ConfigPath != first.Binding().ConfigPath || second.Binding().Endpoint != first.Binding().Endpoint {
		t.Fatalf("private binding changed across reuse: first=%+v second=%+v", first.Binding(), second.Binding())
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestRecoverPrivateRestartsStoppedSupervisor(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: privateTestObservationTimeout}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, stateRoot)
	saved := first.Binding()
	saved.ResolutionCwd = filepath.Join(base, "removed-before-recovery")
	recovered, err := RecoverPrivate(context.Background(), stateRoot, saved, clientPath, daemonPath, options)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Binding() != saved {
		t.Fatalf("recovery changed the saved supervisor binding: saved=%+v recovered=%+v", saved, recovered.Binding())
	}
	command, err := recovered.prepareCommand("status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Dir(recovered.RuntimeBinding().ConfigPath); command.Dir != want {
		t.Fatalf("recovered supervisor command directory=%q, want config directory %q", command.Dir, want)
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 2 {
		t.Fatalf("private daemon was restarted %d times, want 2: %q", got, log)
	}
}

func TestRecoverPrivatePredecessorAdoptsLiveCompletedSuccessor(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, true)
	historyBefore := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))
	identityBefore := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))

	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	if err != nil {
		t.Fatalf("predecessor recovery against live successor failed: %v", err)
	}
	if recovered.Binding() != fixture.saved {
		t.Fatalf("recovery changed durable predecessor binding: got=%+v want=%+v", recovered.Binding(), fixture.saved)
	}
	if recovered.RuntimeBinding() != fixture.successor {
		t.Fatalf("recovery did not use completed successor at runtime: got=%+v want=%+v", recovered.RuntimeBinding(), fixture.successor)
	}
	if identity := readPrivateDaemonIdentity(t, fixture.stateRoot); identity.Executable != fixture.successor.DaemonExecutable || identity.SHA256 != fixture.successor.DaemonSHA256 {
		t.Fatalf("recovery changed completed daemon identity: got=%+v successor=%+v", identity, fixture.successor)
	}
	if got := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot)); !bytes.Equal(got, identityBefore) {
		t.Fatal("live predecessor recovery republished the completed supervisor binding")
	}
	if got := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, historyBefore) {
		t.Fatal("live predecessor recovery changed migration history")
	}
	if _, err := os.Stat(privateMigrationRecordPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live predecessor recovery created a migration journal: %v", err)
	}
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivatePredecessorAdoptsLiveSuccessorWithCompatiblePair(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, true)
	compatibleClient, compatibleDaemon := installPrivateSupervisorPair(t, filepath.Join(fixture.base, "compatible"))
	removePrivateTestPath(t, filepath.Join(fixture.base, "completed"))
	identityBefore := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))
	supervisorBefore := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))

	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, fixture.saved, compatibleClient, compatibleDaemon, fixture.options)
	if err != nil {
		t.Fatalf("predecessor recovery did not adopt live successor through compatible pair: %v", err)
	}
	if recovered.Binding() != fixture.saved {
		t.Fatalf("recovery changed durable predecessor binding: got=%+v want=%+v", recovered.Binding(), fixture.saved)
	}
	if !task.SameSupervisorIdentity(recovered.RuntimeBinding(), fixture.successor) || recovered.RuntimeBinding().ClientExecutable != compatibleClient || recovered.RuntimeBinding().DaemonExecutable != compatibleDaemon {
		t.Fatalf("recovery did not retain completed successor endpoint with compatible runtime pair: got=%+v successor=%+v", recovered.RuntimeBinding(), fixture.successor)
	}
	if got := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, identityBefore) {
		t.Fatal("compatible live recovery changed daemon identity")
	}
	if got := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot)); !bytes.Equal(got, supervisorBefore) {
		t.Fatal("compatible live recovery changed supervisor identity")
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivateRestartsExactCompletedSuccessorForPredecessor(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, false)
	historyBefore := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))
	identityBefore := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))
	supervisorBefore := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))

	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	if err != nil {
		t.Fatalf("predecessor recovery did not restart completed successor: %v", err)
	}
	if recovered.Binding() != fixture.saved {
		t.Fatalf("recovery changed durable predecessor binding: got=%+v want=%+v", recovered.Binding(), fixture.saved)
	}
	if recovered.RuntimeBinding() != fixture.successor {
		t.Fatalf("recovery did not restart exact completed successor: got=%+v want=%+v", recovered.RuntimeBinding(), fixture.successor)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, fixture.successor)
	if got := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, identityBefore) {
		t.Fatal("exact successor restart changed the completed daemon identity")
	}
	if got := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot)); !bytes.Equal(got, supervisorBefore) {
		t.Fatal("exact successor restart changed the completed supervisor binding")
	}
	if got := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, historyBefore) {
		t.Fatal("exact successor restart changed migration history")
	}
	if _, err := os.Stat(privateMigrationRecordPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact successor restart created a migration journal: %v", err)
	}
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivateRejectsUnrelatedPredecessorWithoutStartingSuccessor(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, false)
	unrelated := fixture.saved
	unrelated.ConfigDigest = task.ComputeSHA256([]byte("unrelated config"))
	unrelated.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("unrelated resolved config"))
	unrelated.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "unrelated.sock")
	identityBefore := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))
	supervisorBefore := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))
	historyBefore := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))

	if _, err := RecoverPrivate(context.Background(), fixture.stateRoot, unrelated, fixture.clientPath, fixture.daemonPath, fixture.options); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unrelated saved predecessor was accepted: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if got := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, identityBefore) {
		t.Fatal("unrelated recovery changed daemon identity")
	}
	if got := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot)); !bytes.Equal(got, supervisorBefore) {
		t.Fatal("unrelated recovery changed supervisor identity")
	}
	if got := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, historyBefore) {
		t.Fatal("unrelated recovery changed migration history")
	}
}

func TestRecoverPrivateStartsCompletedSuccessorInsteadOfStagedPair(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, false)
	stagedClient, stagedDaemon := installPrivateSupervisorPair(t, filepath.Join(fixture.base, "staged"))
	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, fixture.saved, stagedClient, stagedDaemon, fixture.options)
	if err != nil {
		t.Fatalf("recovery with staged successor inventory failed: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, fixture.successor)
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivateRestoresExactLegacyGenerationWhenCompletedBindingMissing(t *testing.T) {
	fixture := newCompletedPrivateLegacyTestFixture(t)
	if err := os.Remove(privateSupervisorBindingPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, fixture.stateRoot)
	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	if err != nil {
		t.Fatalf("exact legacy generation was not restored: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, fixture.saved)
	persisted, found, err := loadPrivateSupervisorBinding(fixture.stateRoot)
	if err != nil || !found || persisted != fixture.saved {
		t.Fatalf("legacy recovery did not republish exact saved binding: found=%t binding=%+v err=%v", found, persisted, err)
	}
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivateRejectsDifferentPairWhenCompletedBindingMissing(t *testing.T) {
	fixture := newCompletedPrivateLegacyTestFixture(t)
	if err := os.Remove(privateSupervisorBindingPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	differentClient, differentDaemon := installPrivateSupervisorPair(t, filepath.Join(fixture.base, "different"))
	different := fixture.saved
	different.ClientExecutable = canonicalPrivateTestExecutable(t, differentClient)
	different.ClientSHA256 = hashPrivateTestExecutable(t, differentClient)
	different.DaemonExecutable = canonicalPrivateTestExecutable(t, differentDaemon)
	different.DaemonSHA256 = hashPrivateTestExecutable(t, differentDaemon)
	identityBefore := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))

	if _, err := RecoverPrivate(context.Background(), fixture.stateRoot, different, differentClient, differentDaemon, fixture.options); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("different saved generation was accepted without a completed binding: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if got := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, identityBefore) {
		t.Fatal("rejected legacy recovery changed daemon identity")
	}
}

type completedPrivateSuccessorTestFixture struct {
	base       string
	stateRoot  string
	clientPath string
	daemonPath string
	saved      task.SupervisorRef
	successor  task.SupervisorRef
	options    Options
}

func newCompletedPrivateSuccessorTestFixture(t *testing.T, live bool) completedPrivateSuccessorTestFixture {
	t.Helper()
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldClient, oldDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "predecessor"))
	currentClient, currentDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "completed"))
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	legacy := bindPrivateSetupForTest(t, stateRoot, oldClient, oldDaemon, options)
	saved := legacy.Binding()
	if err := legacy.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := legacy.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	configPath := saved.ConfigPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	oldSocket := strconv.Quote(filepath.Join(stateRoot, privateSupervisorDirectory, "run", "pueue.sock"))
	newSocket := strconv.Quote(filepath.Join(stateRoot, privateSupervisorDirectory, "run", "completed.sock"))
	changed := strings.Replace(string(data), "unix_socket_path: "+oldSocket, "unix_socket_path: "+newSocket, 1)
	if changed == string(data) {
		t.Fatal("completed successor fixture did not change its endpoint")
	}
	if writeErr := os.WriteFile(configPath, []byte(changed), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	successorClient, err := Bind(context.Background(), currentClient, configPath, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := attachPrivateDaemon(successorClient, currentDaemon); err != nil {
		t.Fatal(err)
	}
	successor := successorClient.Binding()
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := publishPrivateDaemonIdentity(privateBase, successor); err != nil {
		t.Fatal(err)
	}
	if err := appendPrivateMigrationHistory(privateBase, privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(saved),
		Successor:     privateDaemonIdentityForBinding(successor),
		LegacyPID:     101,
		LegacyStopped: true,
	}); err != nil {
		t.Fatal(err)
	}
	if live {
		if err := successorClient.StartPrivate(context.Background(), stateRoot); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cleanupErr := successorClient.Shutdown(context.Background()); cleanupErr != nil {
				t.Logf("stop completed successor fixture: %v", cleanupErr)
			}
			if waitErr := successorClient.WaitUnavailable(context.Background()); waitErr != nil {
				t.Logf("wait for completed successor fixture: %v", waitErr)
			}
		})
	}
	return completedPrivateSuccessorTestFixture{
		base: base, stateRoot: stateRoot, clientPath: currentClient, daemonPath: currentDaemon,
		saved: saved, successor: successor, options: options,
	}
}

type completedPrivateLegacyTestFixture struct {
	base       string
	stateRoot  string
	clientPath string
	daemonPath string
	saved      task.SupervisorRef
	options    Options
}

func newCompletedPrivateLegacyTestFixture(t *testing.T) completedPrivateLegacyTestFixture {
	t.Helper()
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "legacy"))
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	client := bindPrivateSetupForTest(t, stateRoot, clientPath, daemonPath, options)
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	return completedPrivateLegacyTestFixture{base: base, stateRoot: stateRoot, clientPath: clientPath, daemonPath: daemonPath, saved: client.Binding(), options: options}
}

func TestRecoverPrivateRestoresMissingLogicalBindingForStoppedSupervisor(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: privateTestObservationTimeout}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	saved := first.Binding()
	if err = os.Remove(privateSupervisorBindingPath(stateRoot)); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, stateRoot)

	recovered, err := RecoverPrivate(context.Background(), stateRoot, saved, clientPath, daemonPath, options)
	if err != nil {
		t.Fatalf("stopped supervisor with missing logical binding was not recoverable: %v", err)
	}
	if recovered.Binding() != saved {
		t.Fatalf("recovery changed the saved supervisor binding: saved=%+v recovered=%+v", saved, recovered.Binding())
	}
	persisted, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !task.SameSupervisorIdentity(persisted, saved) {
		t.Fatalf("saved logical binding was not restored: found=%t binding=%+v want=%+v", found, persisted, saved)
	}
}

func TestRecoverSavedStartsAndGracefullyStopsRecordedLegacySupervisor(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	readyPath := filepath.Join(filepath.Dir(fake.configPath), "legacy-ready")
	shutdownPath := filepath.Join(filepath.Dir(fake.configPath), "legacy-shutdown")
	fake.environment = append(fake.environment, "FAKE_READY="+readyPath, "FAKE_SHUTDOWN_MARKER="+shutdownPath)
	fake.client.options.Environment = fake.environment
	daemonPath := filepath.Join(filepath.Dir(fake.configPath), "pueued")
	writeExecutable(t, daemonPath, `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c) config=$2; shift 2 ;;
    *) shift ;;
  esac
done
touch "$(dirname "$config")/legacy-ready"
`)
	digest, err := hashExecutable(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	saved := fake.client.Binding()
	saved.DaemonExecutable = daemonPath
	saved.DaemonSHA256 = digest
	client, started, err := RecoverSaved(context.Background(), saved, Options{ObservationTimeout: privateTestObservationTimeout, Environment: fake.environment})
	if err != nil {
		t.Fatalf("legacy recovery failed: %v", err)
	}
	if !started || client == nil {
		t.Fatalf("legacy recovery did not report its started daemon: client=%v started=%t", client != nil, started)
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatalf("legacy shutdown failed: %v", err)
	}
	if err := client.WaitUnavailable(context.Background()); err != nil {
		t.Fatalf("legacy endpoint remained available after shutdown: %v", err)
	}
}

func TestRecoverSavedRejectsPartialDaemonIdentity(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	saved := fake.client.Binding()
	saved.DaemonExecutable = fake.executable
	if _, _, err := RecoverSaved(context.Background(), saved, Options{Environment: fake.environment}); !errors.Is(err, ErrBinding) {
		t.Fatalf("partial daemon identity was accepted: %v", err)
	}
}

func TestRecoverSavedStopsAlreadyReachableLegacySupervisor(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	shutdownPath := filepath.Join(filepath.Dir(fake.configPath), "legacy-shutdown")
	fake.environment = append(fake.environment, "FAKE_SHUTDOWN_MARKER="+shutdownPath)
	saved := fake.client.Binding()
	client, started, err := RecoverSaved(context.Background(), saved, Options{ObservationTimeout: privateTestObservationTimeout, Environment: fake.environment})
	if err != nil {
		t.Fatalf("reachable legacy recovery failed: %v", err)
	}
	if started {
		t.Fatal("reachable legacy supervisor was started a second time")
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatalf("reachable legacy shutdown failed: %v", err)
	}
	if err := client.WaitUnavailable(context.Background()); err != nil {
		t.Fatalf("reachable legacy endpoint remained available after shutdown: %v", err)
	}
}

func TestRecoverPrivateUsesCurrentPairAfterSavedInstallRemoved(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldInstall := filepath.Join(base, "old")
	currentInstall := filepath.Join(base, "current")
	oldClientPath := filepath.Join(oldInstall, "pueue")
	oldDaemonPath := filepath.Join(oldInstall, "pueued")
	currentClientPath := filepath.Join(currentInstall, "pueue")
	currentDaemonPath := filepath.Join(currentInstall, "pueued")
	makePrivateTestDirectory(t, oldInstall)
	makePrivateTestDirectory(t, currentInstall)
	writeExecutable(t, oldClientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentClientPath, privateClientFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture)

	first := bindPrivateForTest(t, stateRoot, oldClientPath, oldDaemonPath)
	identityBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	daemonIdentityPath := privateDaemonIdentityPath(identityBase)
	supervisorIdentityPath := privateSupervisorBindingPath(stateRoot)
	oldDaemonIdentity := readPrivateSupervisorFile(t, daemonIdentityPath)
	oldSupervisorIdentity := readPrivateSupervisorFile(t, supervisorIdentityPath)
	appendPrivateSupervisorFile(t, currentDaemonPath, "\n# upgraded daemon pair\n")
	removePrivateTestPath(t, oldInstall)
	assertPrivateTestPathMissing(t, first.Binding().ClientExecutable)

	recovered := recoverPrivateForTest(t, stateRoot, first.Binding(), currentClientPath, currentDaemonPath)
	if recovered.Binding() != first.Binding() {
		t.Fatalf("recovery changed the durable task supervisor identity: first=%+v recovered=%+v", first.Binding(), recovered.Binding())
	}
	if recovered.runtimeBinding.ClientExecutable != currentClientPath || recovered.runtimeBinding.DaemonExecutable != currentDaemonPath {
		t.Fatalf("recovered client did not use the current executable pair: %+v", recovered.runtimeBinding)
	}
	if err := recovered.Ready(context.Background()); err != nil {
		t.Fatalf("recovered client could not use the current pair to inspect the live endpoint: %v", err)
	}
	if identityAfterAdoption := readPrivateSupervisorFile(t, daemonIdentityPath); !bytes.Equal(identityAfterAdoption, oldDaemonIdentity) {
		t.Fatal("recovery rewrote daemon launch provenance while adopting the existing endpoint")
	}
	if identityAfterAdoption := readPrivateSupervisorFile(t, supervisorIdentityPath); !bytes.Equal(identityAfterAdoption, oldSupervisorIdentity) {
		t.Fatal("recovery rewrote the completed supervisor generation while adopting the existing endpoint")
	}

	removePrivateTestPath(t, filepath.Join(stateRoot, privateSupervisorDirectory, "ready"))
	if _, err := RecoverPrivate(context.Background(), stateRoot, first.Binding(), currentClientPath, currentDaemonPath, Options{ObservationTimeout: DefaultObservationTimeout}); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("stopped active generation accepted a replacement pair: %v", err)
	}
	if identityAfterRefusal := readPrivateSupervisorFile(t, daemonIdentityPath); !bytes.Equal(identityAfterRefusal, oldDaemonIdentity) {
		t.Fatal("refused recovery changed daemon launch provenance")
	}
	if identityAfterRefusal := readPrivateSupervisorFile(t, supervisorIdentityPath); !bytes.Equal(identityAfterRefusal, oldSupervisorIdentity) {
		t.Fatal("refused recovery changed the completed supervisor generation")
	}
	assertPrivateDaemonStartCount(t, stateRoot, 1)
}

func TestRecoveredPrivateClientCanStopTaskWithOriginalSupervisorReference(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldClientPath, oldDaemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "old"))
	currentClientPath, currentDaemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "current"))
	privateRoot := filepath.Join(stateRoot, privateSupervisorDirectory)
	statusPath := filepath.Join(privateRoot, "test-status.json")
	killMarker := filepath.Join(privateRoot, "test-kill-task-id")
	// This test verifies recovery preserves the durable stop authority; it does
	// not exercise a short observation timeout. Use the production default so
	// a full -race run cannot turn scheduler contention into a false failure.
	options := Options{ObservationTimeout: DefaultObservationTimeout}
	first, err := BindPrivate(context.Background(), oldClientPath, oldDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(privateRoot, "test-record-kill"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newTaskFixture(t, first.Binding())
	submission := createSubmission(t, fixture, first.Binding())
	writePrivateRunningTaskStatus(t, statusPath, submission, 7)
	observation, err := first.Reconcile(context.Background(), identityFromRecord(submission))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.taskDir.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
	permit := createStopPermit(t, fixture)
	defer func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()

	removePrivateTestPath(t, filepath.Join(base, "old"))
	recovered := recoverPrivateForTest(t, stateRoot, first.Binding(), currentClientPath, currentDaemonPath)
	if recovered.Binding() != first.Binding() {
		t.Fatalf("recovery changed the queued task's supervisor reference: first=%+v recovered=%+v", first.Binding(), recovered.Binding())
	}
	result, err := recovered.Stop(context.Background(), permit)
	if err != nil {
		t.Fatalf("recovered private client could not stop the task: %+v %v", result, err)
	}
	if !result.Matched || !result.Attempted || result.Action != "kill" || result.Acknowledged == nil || !*result.Acknowledged {
		t.Fatalf("recovered private client did not complete the authorized stop: %+v", result)
	}
	if got := string(readPrivateSupervisorFile(t, killMarker)); got != "7\n" {
		t.Fatalf("recovered client stopped task %q, want task 7", got)
	}
}

func TestRecoverPrivateBoundsHungBootstrapAndRetainsLock(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, stateRoot)

	startedMarker := filepath.Join(base, "status-started")
	releaseMarker := filepath.Join(base, "status-release")
	options := Options{
		// Leave enough time for RecoverPrivate's version preflight to finish
		// before the deliberately blocked status command consumes the budget.
		ObservationTimeout: time.Second,
		Environment: append(
			os.Environ(),
			"PRIVATE_STATUS_STARTED="+startedMarker,
			"PRIVATE_STATUS_RELEASE="+releaseMarker,
		),
	}
	defer releasePrivateStatusOnCleanup(t, releaseMarker)
	started := time.Now()
	_, err = RecoverPrivate(context.Background(), stateRoot, first.Binding(), clientPath, daemonPath, options)
	if err == nil {
		t.Fatal("hung private bootstrap unexpectedly recovered")
	}
	maxBootstrapDuration := time.Duration(privateBootstrapObservationPhases)*options.ObservationTimeout + 500*time.Millisecond
	if elapsed := time.Since(started); elapsed > maxBootstrapDuration {
		t.Fatalf("private recovery exceeded its bounded budget: elapsed=%s limit=%s", elapsed, maxBootstrapDuration)
	}
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("private recovery lost its pending ownership: %v", err)
	}
	if !strings.Contains("\x00"+strings.Join(inFlight.Pending.args, "\x00")+"\x00", "\x00status\x00") {
		t.Fatalf("private recovery timed out before the blocked status probe: %q", inFlight.Pending.args)
	}
	waitForPrivateFixtureMarker(t, startedMarker)

	lockContext, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	deferredLock, lockErr := acquireBootstrapLock(lockContext, filepath.Join(stateRoot, privateSupervisorDirectory, "bootstrap.lock"))
	cancel()
	if deferredLock != nil {
		if closeErr := closeBootstrapLock(deferredLock); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !errors.Is(lockErr, context.DeadlineExceeded) {
		t.Fatalf("bootstrap lock was released while readiness was still running: %v", lockErr)
	}
	releasePrivateStatus(t, releaseMarker)
	if !awaitPendingNaturally(inFlight.Pending, 10*time.Second) {
		t.Fatal("hung readiness process remained in flight")
	}
	lock, lockErr := acquireBootstrapLock(context.Background(), filepath.Join(stateRoot, privateSupervisorDirectory, "bootstrap.lock"))
	if lockErr != nil {
		t.Fatal(lockErr)
	}
	if err = closeBootstrapLock(lock); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivateRestartsStoppedSupervisorAfterExecutableDrift(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	wantDaemonIdentity := readPrivateDaemonIdentity(t, fixture.stateRoot)
	wantSupervisorIdentity := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))
	removePrivateTestPath(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready"))
	appendPrivateSupervisorFile(t, fixture.daemonPath, "\n")
	if _, err := RecoverPrivate(context.Background(), fixture.stateRoot, first.Binding(), fixture.clientPath, fixture.daemonPath, Options{ObservationTimeout: DefaultObservationTimeout}); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("stopped active generation accepted executable drift: %v", err)
	}

	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if identity := readPrivateDaemonIdentity(t, fixture.stateRoot); identity != wantDaemonIdentity {
		t.Fatalf("rejected recovery changed daemon launch provenance: got=%+v want=%+v", identity, wantDaemonIdentity)
	}
	if identity := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot)); !bytes.Equal(identity, wantSupervisorIdentity) {
		t.Fatal("rejected recovery changed the completed supervisor generation")
	}
}

func TestRestartPrivateRepinsIdentityAfterGracefulShutdown(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")

	if _, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, Options{ObservationTimeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	legacyIdentity := readPrivateDaemonIdentity(t, stateRoot)
	current, err := BindPrivate(context.Background(), clientPath, currentDaemonPath, stateRoot, Options{ObservationTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got := readPrivateDaemonIdentity(t, stateRoot); got != legacyIdentity {
		t.Fatalf("reachable upgrade rewrote identity before migration: got=%+v legacy=%+v", got, legacyIdentity)
	}
	if err := current.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := current.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := current.RestartPrivate(context.Background(), stateRoot); err != nil {
		t.Fatal(err)
	}
	want := privateDaemonIdentityForBinding(current.Binding())
	if got := readPrivateDaemonIdentity(t, stateRoot); got != want {
		t.Fatalf("migration did not atomically repin current identity: got=%+v want=%+v", got, want)
	}
}

func TestPrivateDaemonIdentityDriftIsReadOnly(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	makePrivateTestDirectory(t, privateBase)
	seedPrivateDaemonIdentity(t, privateBase, oldDaemonPath)

	drift, err := PrivateDaemonIdentityDrift(stateRoot, currentDaemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !drift {
		t.Fatal("changed daemon launch provenance was not detected")
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, oldDaemonPath)

	drift, err = PrivateDaemonIdentityDrift(stateRoot, oldDaemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if drift {
		t.Fatal("matching daemon launch provenance was reported as drift")
	}
}

func TestIsPrivateConfigRejectsExistingUnownedSupervisorConfig(t *testing.T) {
	stateRoot := filepath.Join(canonicalTemp(t), "state")
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PrivateConfigPath(stateRoot), []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsPrivateConfig(stateRoot, PrivateConfigPath(stateRoot)) {
		t.Fatal("existing supervisor config without a Delegate state root was treated as private")
	}
}

func TestPrivateSupervisorLogicalDriftIsDetected(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if drift, err := PrivateSupervisorLogicalDrift(fixture.stateRoot, client.Binding()); err != nil || drift {
		t.Fatalf("matching private supervisor was reported as logically drifted: drift=%v err=%v", drift, err)
	}
	drifted := client.Binding()
	drifted.ConfigDigest = strings.Repeat("a", 64)
	if drift, err := PrivateSupervisorLogicalDrift(fixture.stateRoot, drifted); err != nil || !drift {
		t.Fatalf("logical private supervisor drift was not detected: drift=%v err=%v", drift, err)
	}
}

func TestRefreshPrivateSupervisorBindingUsesPublishedIdentity(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	saved, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(canonicalTemp(t), "current-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# current\n")
	seedPrivateDaemonIdentity(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory), currentDaemon)

	refreshed, err := RefreshPrivateSupervisorBinding(fixture.stateRoot, saved.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if !task.SameSupervisorIdentity(refreshed, saved.Binding()) {
		t.Fatalf("refresh changed logical supervisor identity: got=%+v want=%+v", refreshed, saved.Binding())
	}
	if refreshed.DaemonExecutable != canonicalPrivateTestExecutable(t, currentDaemon) {
		t.Fatalf("refresh kept stale daemon executable: %q", refreshed.DaemonExecutable)
	}
}

func TestMigratePrivateSupervisorWithLockRepinsDriftedIdentity(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	if _, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, options); err != nil {
		t.Fatal(err)
	}
	legacyIdentity := readPrivateDaemonIdentity(t, stateRoot)
	current, err := BindPrivate(context.Background(), clientPath, currentDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := readPrivateDaemonIdentity(t, stateRoot); got != legacyIdentity {
		t.Fatalf("reachable upgrade rewrote identity before migration: got=%+v legacy=%+v", got, legacyIdentity)
	}
	if err := WithBootstrapLock(context.Background(), stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); err != nil {
		t.Fatal(err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, currentDaemonPath)
	if _, err := os.Stat(privateRecoveryRecordPath(stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed private migration retained recovery record: %v", err)
	}
	assertPrivateDaemonStartCount(t, stateRoot, 2)
}

func TestPreparePrivateForMigrationDoesNotProbeChangedEndpoint(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	bindPrivateSetupForTest(t, stateRoot, clientPath, daemonPath, Options{ObservationTimeout: privateTestObservationTimeout})
	statusStarted := filepath.Join(base, "status-started")
	releaseStatus := filepath.Join(base, "release-status")
	environment := append(
		os.Environ(),
		"PRIVATE_STATUS_STARTED="+statusStarted,
		"PRIVATE_STATUS_RELEASE="+releaseStatus,
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := WithBootstrapLock(ctx, stateRoot, func() error {
		current, err := PreparePrivateForMigrationWithLock(ctx, clientPath, daemonPath, stateRoot, Options{
			ObservationTimeout: privateTestObservationTimeout,
			Environment:        environment,
		})
		if err != nil {
			return err
		}
		return current.Close()
	}); err != nil {
		t.Fatalf("migration preparation probed an unavailable successor endpoint: %v", err)
	}
	if _, err := os.Stat(statusStarted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("migration preparation launched a successor status probe: %v", err)
	}
}

func TestVerifyPrivateMigrationEndpointFailsClosedWithoutLogicalPredecessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	if err := os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, fixture.stateRoot)
	if err := os.Remove(privateSupervisorBindingPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(filepath.Dir(fixture.stateRoot), "current-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# current bundle\n")

	var current *Client
	err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		var prepareErr error
		current, prepareErr = PreparePrivateForMigrationWithLock(context.Background(), fixture.clientPath, currentDaemon, fixture.stateRoot, options)
		if prepareErr != nil {
			return prepareErr
		}
		defer func() {
			if closeErr := current.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
		return current.VerifyPrivateMigrationEndpoint(context.Background(), fixture.stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unreachable endpoint with persisted identity was accepted: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestVerifyPrivateMigrationEndpointAllowsStoppedMatchingSupervisor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	if err := os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, fixture.stateRoot)

	var current *Client
	err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		var prepareErr error
		current, prepareErr = PreparePrivateForMigrationWithLock(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
		if prepareErr != nil {
			return prepareErr
		}
		defer func() {
			if closeErr := current.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
		if err := current.VerifyPrivateMigrationEndpoint(context.Background(), fixture.stateRoot); err != nil {
			return err
		}
		return current.StartPrivate(context.Background(), fixture.stateRoot)
	})
	if err != nil {
		t.Fatalf("stopped supervisor with matching identity was not restartable: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func TestVerifyPrivateMigrationEndpointFailsClosedWithoutIdentity(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	if _, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, fixture.stateRoot)
	if err := os.Remove(privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); err != nil {
		t.Fatal(err)
	}

	var current *Client
	err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		var prepareErr error
		current, prepareErr = PreparePrivateForMigrationWithLock(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
		if prepareErr != nil {
			return prepareErr
		}
		defer func() {
			if closeErr := current.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
		return current.VerifyPrivateMigrationEndpoint(context.Background(), fixture.stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unreachable endpoint without persisted identity was accepted: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestPendingPrivateMigrationJournalResumesAfterShutdown(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	first, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	current, err := BindPrivate(context.Background(), clientPath, currentDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(first.Binding()),
		Successor:     privateDaemonIdentityForBinding(current.Binding()),
	}
	if err := current.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := current.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateMigrationRecord(filepath.Dir(current.Binding().ConfigPath), pending.Legacy, pending.Successor); err != nil {
		t.Fatal(err)
	}

	if err := WithBootstrapLock(context.Background(), stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); err != nil {
		t.Fatal(err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, currentDaemonPath)
	if _, err := os.Stat(privateMigrationRecordPath(filepath.Dir(current.Binding().ConfigPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed pending migration journal remained: %v", err)
	}
	assertPrivateDaemonStartCount(t, stateRoot, 3)
}

func TestPendingPrivateMigrationCompletesAfterSuccessorLaunchBeforeIdentityPublication(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	legacyDaemonPath := filepath.Join(base, "legacy-pueued")
	successorDaemonPath := filepath.Join(base, "successor-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, legacyDaemonPath, privateDaemonFixture)
	writeExecutable(t, successorDaemonPath, privateDaemonFixture+"\n# successor\n")
	options := privateMigrationTestOptions(stateRoot, DefaultObservationTimeout)
	legacy, err := BindPrivate(context.Background(), clientPath, legacyDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := BindPrivate(context.Background(), clientPath, successorDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	pid, present, err := legacy.privateDaemonPID()
	if err != nil || !present {
		t.Fatalf("predecessor PID: %v, present=%v", err, present)
	}
	if err = legacy.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = legacy.WaitUnavailableWithPID(context.Background(), pid); err != nil {
		t.Fatal(err)
	}
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacy.Binding()),
		Successor:     privateDaemonIdentityForBinding(successor.Binding()),
		LegacyPID:     pid,
		LegacyStopped: true,
	}
	data, err := marshalPrivateMigrationRecord(pending)
	if err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err = publishPrivateRecord(basePath, privateMigrationRecordPath(basePath), data); err != nil {
		t.Fatal(err)
	}

	if err = startDaemon(successor.runtimeBinding.DaemonExecutable, successor.runtimeBinding.DaemonSHA256, successor.runtimeBinding.ConfigPath, basePath, options); err != nil {
		t.Fatal(err)
	}
	if err = waitReady(context.Background(), successor, options.ObservationTimeout); err != nil {
		t.Fatal(err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, legacyDaemonPath)

	if err = WithBootstrapLock(context.Background(), stateRoot, func() error {
		return successor.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); err != nil {
		t.Fatalf("successor launch was not reconciled from the migration phase: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, successorDaemonPath)
	assertPrivateDaemonStartCount(t, stateRoot, 2)
	if _, err = os.Stat(privateMigrationRecordPath(basePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed successor launch migration journal remained: %v", err)
	}
}

func TestPendingPrivateMigrationRetargetsAfterSecondInstallationUpgrade(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	legacyDaemonPath := filepath.Join(base, "legacy-pueued")
	firstDaemonPath := filepath.Join(base, "first-pueued")
	secondDaemonPath := filepath.Join(base, "second-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, legacyDaemonPath, privateDaemonFixture)
	writeExecutable(t, firstDaemonPath, privateDaemonFixture+"\n# first installation\n")
	writeExecutable(t, secondDaemonPath, privateDaemonFixture+"\n# second installation\n")
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	legacy, err := BindPrivate(context.Background(), clientPath, legacyDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	first, err := BindPrivate(context.Background(), clientPath, firstDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	legacyIdentity := privateDaemonIdentityForBinding(legacy.Binding())
	firstIdentity := privateDaemonIdentityForBinding(first.Binding())
	if err = publishPrivateMigrationRecord(filepath.Dir(first.Binding().ConfigPath), legacyIdentity, firstIdentity); err != nil {
		t.Fatal(err)
	}

	second, err := BindPrivate(context.Background(), clientPath, secondDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := WithBootstrapLock(context.Background(), stateRoot, func() error {
		return second.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); err != nil {
		t.Fatalf("pending migration did not retarget to the second installation: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, secondDaemonPath)
	if _, err := os.Stat(privateMigrationRecordPath(filepath.Dir(second.Binding().ConfigPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retargeted migration journal remained: %v", err)
	}
}

func TestPrivateSupervisorGenerationMatchesPublishedMigrationHistory(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	current := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	base := current.Binding()
	first := base
	first.ConfigDigest = task.ComputeSHA256([]byte("first-config"))
	first.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("first-resolved"))
	first.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "first.sock")
	second := first
	second.ConfigDigest = task.ComputeSHA256([]byte("second-config"))
	second.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("second-resolved"))
	second.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "second.sock")
	third := second
	third.ConfigDigest = task.ComputeSHA256([]byte("third-config"))
	third.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("third-resolved"))
	third.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "third.sock")
	basePath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	for _, migration := range []privateMigrationRecord{
		{
			SchemaVersion: task.SchemaVersion,
			Legacy:        privateDaemonIdentityForBinding(first),
			Successor:     privateDaemonIdentityForBinding(second),
			LegacyPID:     101,
			LegacyStopped: true,
		},
		{
			SchemaVersion: task.SchemaVersion,
			Legacy:        privateDaemonIdentityForBinding(second),
			Successor:     privateDaemonIdentityForBinding(third),
			LegacyPID:     102,
			LegacyStopped: true,
		},
	} {
		if err := appendPrivateMigrationHistory(basePath, migration); err != nil {
			t.Fatal(err)
		}
	}

	for name, binding := range map[string]task.SupervisorRef{
		"first predecessor":  first,
		"second predecessor": second,
		"latest successor":   third,
	} {
		t.Run(name, func(t *testing.T) {
			matched, err := PrivateSupervisorGenerationMatches(fixture.stateRoot, binding)
			if err != nil {
				t.Fatal(err)
			}
			if !matched {
				t.Fatalf("published migration history did not match %+v", binding)
			}
		})
	}
	unrelated := third
	unrelated.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "unrelated.sock")
	matched, err := PrivateSupervisorGenerationMatches(fixture.stateRoot, unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("unpublished supervisor generation matched migration history")
	}
}

func TestMigratePrivateKeepsRecoveryAliasUntilSuccessorReady(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, "#!/bin/sh\nexit 0\n")
	options := privateMigrationTestOptions(stateRoot, 100*time.Millisecond)
	legacy := bindPrivateSetupForTest(t, stateRoot, clientPath, oldDaemonPath, options)
	current := bindPrivateSetupForTest(t, stateRoot, clientPath, currentDaemonPath, options)
	if err := legacy.PersistPrivateRecoveryBinding(stateRoot); err != nil {
		t.Fatal(err)
	}
	if err := current.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := current.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateDaemonIdentity(filepath.Dir(current.Binding().ConfigPath), current.Binding()); err != nil {
		t.Fatal(err)
	}

	tryMigration := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return WithBootstrapLock(ctx, stateRoot, func() error {
			return current.MigratePrivateSupervisorWithLock(ctx, stateRoot)
		})
	}
	if err := tryMigration(); err == nil {
		t.Fatal("migration accepted an unavailable successor")
	}
	if _, err := os.Stat(privateRecoveryRecordPath(stateRoot)); err != nil {
		t.Fatalf("recovery alias was removed before successor readiness: %v", err)
	}

	legacyIdentity := privateDaemonIdentity{Executable: legacy.Binding().DaemonExecutable, SHA256: legacy.Binding().DaemonSHA256}
	successorIdentity := privateDaemonIdentity{Executable: current.Binding().DaemonExecutable, SHA256: current.Binding().DaemonSHA256}
	if err := publishPrivateMigrationRecord(filepath.Dir(current.Binding().ConfigPath), legacyIdentity, successorIdentity); err != nil {
		t.Fatal(err)
	}
	if err := tryMigration(); err == nil {
		t.Fatal("pending migration accepted an unavailable successor")
	}
	if _, err := os.Stat(privateRecoveryRecordPath(stateRoot)); err != nil {
		t.Fatalf("recovery alias was removed while pending successor was unavailable: %v", err)
	}
}

func TestPrivateSupervisorMigrationPendingReadsValidatedJournal(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	if err := ensurePrivateBase(filepath.Join(stateRoot, privateSupervisorDirectory)); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(base, "legacy-pueued")
	successorPath := filepath.Join(base, "successor-pueued")
	writeExecutable(t, legacyPath, privateDaemonFixture)
	writeExecutable(t, successorPath, privateDaemonFixture+"\n# successor\n")
	legacy := privateDaemonIdentity{Executable: legacyPath, SHA256: hashPrivateTestExecutable(t, legacyPath)}
	successor := privateDaemonIdentity{Executable: successorPath, SHA256: hashPrivateTestExecutable(t, successorPath)}
	if err := publishPrivateMigrationRecord(filepath.Join(stateRoot, privateSupervisorDirectory), legacy, successor); err != nil {
		t.Fatal(err)
	}
	pending, err := PrivateSupervisorMigrationPending(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("validated private migration journal was not reported as pending")
	}
	if err := os.WriteFile(privateMigrationRecordPath(filepath.Join(stateRoot, privateSupervisorDirectory)), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrivateSupervisorMigrationPending(stateRoot); !errors.Is(err, ErrBinding) {
		t.Fatalf("malformed private migration journal was accepted: %v", err)
	}
}

func TestMigratePrivateSupervisorWithLockRefusesActiveWork(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	options := privateMigrationTestOptions(stateRoot, DefaultObservationTimeout)
	if _, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, options); err != nil {
		t.Fatal(err)
	}
	current, err := BindPrivate(context.Background(), clientPath, currentDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	status := statusTestPayload(t, map[string]any{
		"7": statusTestJob(7, "delegate:active", map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if writeErr := os.WriteFile(filepath.Join(privateBase, "test-status.json"), status, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	err = WithBootstrapLock(context.Background(), stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("active private work was not preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(privateBase, "ready")); err != nil {
		t.Fatalf("active legacy supervisor was stopped: %v", err)
	}
	if _, err := os.Stat(privateRecoveryRecordPath(stateRoot)); err != nil {
		t.Fatalf("active migration did not retain recovery record: %v", err)
	}
	if got := readPrivateDaemonIdentity(t, stateRoot); got.Executable != legacyDaemonExecutable(t, oldDaemonPath) {
		t.Fatalf("active migration replaced legacy identity: %+v", got)
	}
}

func legacyDaemonExecutable(t *testing.T, path string) string {
	t.Helper()
	return canonicalPrivateTestExecutable(t, path)
}

type privateSupervisorTestFixture struct {
	stateRoot  string
	clientPath string
	daemonPath string
}

func newPrivateSupervisorTestFixture(t *testing.T) privateSupervisorTestFixture {
	t.Helper()
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	fixture := privateSupervisorTestFixture{
		stateRoot:  stateRoot,
		clientPath: filepath.Join(base, "pueue"),
		daemonPath: filepath.Join(base, "pueued"),
	}
	writeExecutable(t, fixture.clientPath, privateClientFixture)
	writeExecutable(t, fixture.daemonPath, privateDaemonFixture)
	return fixture
}

func newPrivateSupervisorTestRoots(t *testing.T) (string, string) {
	t.Helper()
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	return base, stateRoot
}

func writePrivateStateRootRecord(t *testing.T, stateRoot string) {
	t.Helper()
	makePrivateTestDirectory(t, stateRoot)
	rootID, err := task.NewRootID()
	if err != nil {
		t.Fatal(err)
	}
	rootData, err := task.MarshalCanonical(task.RootRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        rootID,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "root.json"), rootData, 0o600); err != nil {
		t.Fatal(err)
	}
}

func installPrivateSupervisorPair(t *testing.T, directory string) (string, string) {
	t.Helper()
	makePrivateTestDirectory(t, directory)
	clientPath := filepath.Join(directory, "pueue")
	daemonPath := filepath.Join(directory, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	return clientPath, daemonPath
}

func makePrivateTestDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func bindPrivateForTest(t *testing.T, stateRoot, clientPath, daemonPath string) *Client {
	t.Helper()
	client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{ObservationTimeout: DefaultObservationTimeout, SuccessfulCleanupAuthorizer: allowSuccessfulCleanupForTest})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// bindPrivateSetupForTest separates filesystem/bootstrap setup from the short
// observation deadlines exercised by fault tests.
func bindPrivateSetupForTest(t *testing.T, stateRoot, clientPath, daemonPath string, options Options) *Client {
	t.Helper()
	if options.processObserver == nil {
		options.processObserver = privateTestProcessObserver(stateRoot)
	}
	setup := options
	setup.ObservationTimeout = 10 * time.Second
	client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, setup)
	if err != nil {
		t.Fatal(err)
	}
	client.options = options
	return client
}

func allowSuccessfulCleanupForTest(_ context.Context, _ string, _ task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
	var plan PrivateQueueCleanupPlan
	for _, job := range jobs {
		if job.State == StateEnded && job.Succeeded {
			plan.RemoveIDs = append(plan.RemoveIDs, job.ID)
		}
	}
	return plan, nil
}

func privateTestProcessObserver(stateRoot string) func(int) (privateProcessObservation, error) {
	readyPath := filepath.Join(stateRoot, privateSupervisorDirectory, "ready")
	return func(pid int) (privateProcessObservation, error) {
		if _, err := os.Stat(readyPath); errors.Is(err, os.ErrNotExist) {
			return privateProcessObservation{}, nil
		} else if err != nil {
			return privateProcessObservation{}, err
		}
		return privateProcessObservation{Present: true, StartToken: "fixture:" + strconv.Itoa(pid)}, nil
	}
}

func privateMigrationTestOptions(stateRoot string, timeout time.Duration) Options {
	return Options{
		ObservationTimeout:         timeout,
		PrivateMigrationAuthorizer: allowPrivateMigrationForTest,
		processObserver:            privateTestProcessObserver(stateRoot),
	}
}

func allowPrivateMigrationForTest(_ context.Context, _ string, _ task.SupervisorRef, jobs []Job) error {
	for _, job := range jobs {
		if job.State == StateQueued || job.State == StateRunning || job.State == StateUnknown {
			return ErrPrivateSupervisorMigrationBusy
		}
	}
	return nil
}

func removePrivateTestPID(t *testing.T, stateRoot string) {
	t.Helper()
	path := filepath.Join(stateRoot, privateSupervisorDirectory, "run", "pueue.pid")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func removePrivateTestDaemonState(t *testing.T, stateRoot string) {
	t.Helper()
	readyPath := filepath.Join(stateRoot, privateSupervisorDirectory, "ready")
	if err := os.Remove(readyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("remove private fixture readiness marker: %v", err)
	}
	removePrivateTestPID(t, stateRoot)
}

func recoverPrivateForTest(t *testing.T, stateRoot string, saved task.SupervisorRef, clientPath, daemonPath string) *Client {
	t.Helper()
	client, err := RecoverPrivate(context.Background(), stateRoot, saved, clientPath, daemonPath, Options{ObservationTimeout: DefaultObservationTimeout})
	if err != nil {
		t.Fatalf("private supervisor recovery failed: %v", err)
	}
	return client
}

func readPrivateSupervisorFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func snapshotPrivateTestFile(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return data, true
}

func assertPrivateTestFileUnchanged(t *testing.T, path string, want []byte, wantPresent bool) {
	t.Helper()
	got, gotPresent := snapshotPrivateTestFile(t, path)
	if gotPresent != wantPresent || !bytes.Equal(got, want) {
		t.Fatalf("private test artifact %s changed", path)
	}
}

func snapshotPrivateTestFiles(t *testing.T, paths []string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, found := snapshotPrivateTestFile(t, path)
		if found {
			files[path] = data
		}
	}
	return files
}

func readPrivateDaemonIdentity(t *testing.T, stateRoot string) privateDaemonIdentity {
	t.Helper()
	data := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(stateRoot, privateSupervisorDirectory)))
	var identity privateDaemonIdentity
	if err := task.DecodeStrict(data, &identity); err != nil {
		t.Fatal(err)
	}
	return identity
}

func assertPrivateDaemonIdentityMatchesBinding(t *testing.T, stateRoot string, binding task.SupervisorRef) {
	t.Helper()
	identity := readPrivateDaemonIdentity(t, stateRoot)
	if identity.Executable != binding.DaemonExecutable || identity.SHA256 != binding.DaemonSHA256 {
		t.Fatalf("private daemon identity does not match binding: identity=%+v binding=%+v", identity, binding)
	}
}

func assertPrivateDaemonIdentityMatchesPair(t *testing.T, stateRoot, daemonPath string) {
	t.Helper()
	wantExecutable := canonicalPrivateTestExecutable(t, daemonPath)
	wantSHA256 := hashPrivateTestExecutable(t, daemonPath)
	got := readPrivateDaemonIdentity(t, stateRoot)
	if got.Executable != wantExecutable || got.SHA256 != wantSHA256 {
		t.Fatalf("private daemon identity does not match launched pair: got=%+v want executable=%q sha256=%q", got, wantExecutable, wantSHA256)
	}
}

func assertPrivateConfigRootedInState(t *testing.T, stateRoot string) {
	t.Helper()
	configPath := filepath.Join(stateRoot, privateSupervisorDirectory, "pueue.yml")
	data := readPrivateSupervisorFile(t, configPath)
	want := filepath.Join(stateRoot, privateSupervisorDirectory, "data")
	if !strings.Contains(string(data), want) {
		t.Fatalf("private config is not rooted in state: data=%q", data)
	}
}

func assertPrivateDaemonStartCount(t *testing.T, stateRoot string, want int) {
	t.Helper()
	log := readPrivateSupervisorFile(t, filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if got := strings.Count(string(log), "started\n"); got != want {
		t.Fatalf("private daemon start count=%d, want %d: %q", got, want, log)
	}
}

func appendPrivateSupervisorFile(t *testing.T, path, data string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(data); err != nil {
		closeErr := file.Close()
		if closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writePrivateRunningTaskStatus(t *testing.T, path string, submission *task.SubmitRecord, id int64) {
	t.Helper()
	state := map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}
	job := statusTestJob(id, "delegate:"+submission.RootID+":"+submission.TaskID, state)
	data := statusTestPayload(t, map[string]any{strconv.FormatInt(id, 10): job}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func removePrivateTestPath(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func assertPrivateTestPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path remained available after simulated upgrade: %s err=%v", path, err)
	}
}

func canonicalPrivateTestExecutable(t *testing.T, path string) string {
	t.Helper()
	resolved, err := canonicalExecutable(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func hashPrivateTestExecutable(t *testing.T, path string) string {
	t.Helper()
	digest, err := hashExecutable(canonicalPrivateTestExecutable(t, path))
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestBindPrivateRejectsMalformedSupervisorBindingBeforeStart(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(privateSupervisorBindingPath(stateRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	options := Options{ObservationTimeout: 5 * time.Second}
	if _, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options); !errors.Is(err, ErrBinding) {
		t.Fatalf("bootstrap accepted malformed supervisor binding: %v", err)
	}
	if _, err := os.Stat(filepath.Join(privateBase, "ready")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started despite malformed supervisor binding: %v", err)
	}
	if _, err := os.Stat(filepath.Join(privateBase, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon launch marker exists after malformed supervisor binding: %v", err)
	}
}

func TestPrivateDaemonIdentityReplacementKeepsOtherRecordsCreateOnce(t *testing.T) {
	base := filepath.Join(canonicalTemp(t), "supervisor")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	identityPath := privateDaemonIdentityPath(base)
	if err := publishPrivateRecord(base, identityPath, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateRecord(base, identityPath, []byte("second")); !errors.Is(err, ErrBinding) {
		t.Fatalf("create-once publisher accepted replacement: %v", err)
	}
	if err := replacePrivateDaemonIdentityRecord(base, []byte("second")); err != nil {
		t.Fatalf("deliberate record replacement failed: %v", err)
	}
	data, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("replacement record=%q, want second", data)
	}
}

func TestReplacePrivateSchedulingEvidenceReportsSchedulingContext(t *testing.T) {
	stateRoot := filepath.Join(canonicalTemp(t), "state")
	writePrivateStateRootRecord(t, stateRoot)
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := privateSchedulingEvidencePath(stateRoot)
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}

	err := ReplacePrivateSchedulingEvidence(stateRoot, []byte("evidence"))
	if err == nil {
		t.Fatal("scheduling evidence replacement unexpectedly succeeded over a directory")
	}
	if !errors.Is(err, ErrConfiguration) {
		t.Fatalf("scheduling evidence replacement error = %v, want ErrConfiguration", err)
	}
	if !strings.Contains(err.Error(), "private scheduling evidence") {
		t.Fatalf("scheduling evidence replacement error = %v, want scheduling evidence context", err)
	}
	if strings.Contains(err.Error(), "migration journal") {
		t.Fatalf("scheduling evidence replacement error used migration context: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(base, ".private-scheduling-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 {
		t.Fatalf("scheduling evidence replacement leaked staged files: %v", stages)
	}
}

func TestStartDaemonRevalidatesIdentityBeforeStart(t *testing.T) {
	base := canonicalTemp(t)
	configPath := filepath.Join(base, "pueue.yml")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, daemonPath, privateDaemonFixture)
	originalDigest, err := hashExecutable(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemonPath, []byte(privateDaemonFixture+"\nchanged\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := startDaemon(daemonPath, originalDigest, configPath, base, Options{}); !errors.Is(err, ErrBinding) {
		t.Fatalf("changed daemon identity was started: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started after pre-start identity drift: %v", err)
	}
}

func TestRecoverPrivateRejectsDeletedConfigBeforeRestart(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: 5 * time.Second}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	configPath := first.Binding().ConfigPath
	if err = os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if _, err = RecoverPrivate(context.Background(), stateRoot, first.Binding(), clientPath, daemonPath, options); !errors.Is(err, ErrBinding) {
		t.Fatalf("recovery accepted a deleted saved config: %v", err)
	}
	if _, err = os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery recreated a deleted saved config: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 1 {
		t.Fatalf("private daemon restarted after stale binding rejection %d times, want 1: %q", got, log)
	}
}

func TestBindPrivateSerializesConcurrentBootstrap(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: privateTestObservationTimeout}
	clients := make(chan *Client, 2)
	errorsFound := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
			if err != nil {
				errorsFound <- err
				return
			}
			clients <- client
		}()
	}
	group.Wait()
	close(clients)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if got := len(clients); got != 2 {
		t.Fatalf("concurrent private binds returned %d clients, want 2", got)
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 1 {
		t.Fatalf("private daemon was started %d times, want one: %q", got, log)
	}
}

func writeExecutable(t *testing.T, path, source string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(source), 0o700); err != nil {
		t.Fatal(err)
	}
}

func seedPrivateDaemonIdentity(t *testing.T, base, daemonPath string) {
	t.Helper()
	resolved, err := canonicalExecutable(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hashExecutable(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateDaemonIdentity(base, task.SupervisorRef{DaemonExecutable: resolved, DaemonSHA256: digest}); err != nil {
		t.Fatal(err)
	}
}

const privateClientFixture = `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
	if [ "${PRIVATE_CONTROL_MARKER:-}" != "" ]; then printf '%s\n' "$1" >> "$PRIVATE_CONTROL_MARKER"; fi
  case "$1" in
    -c) config=$2; shift 2 ;;
	    --version)
	      if [ "${PRIVATE_VERSION_MARKER:-}" != "" ]; then printf '%s\n' started >> "$PRIVATE_VERSION_MARKER"; fi
	      if [ "${PRIVATE_VERSION_DELAY:-}" != "" ]; then sleep "$PRIVATE_VERSION_DELAY"; elif [ "${PRIVATE_DELAY_VERSION:-}" = "1" ]; then sleep 0.08; fi
	      if [ "${PRIVATE_VERSION_HOLD:-}" != "" ]; then while [ ! -f "$PRIVATE_VERSION_HOLD" ]; do sleep 0.01; done; fi
	      case "$config" in
	        */.migration-pueue-*) if [ "${PRIVATE_VERSION_RELEASE:-}" != "" ]; then while [ ! -f "$PRIVATE_VERSION_RELEASE" ]; do sleep 0.01; done; fi ;;
	      esac
	      if [ "${PRIVATE_VERSION_MARKER:-}" != "" ]; then printf '%s\n' completed >> "$PRIVATE_VERSION_MARKER"; fi
	      if [ "${PRIVATE_VERSION_FILE:-}" != "" ]; then cat "$PRIVATE_VERSION_FILE"; exit 0; fi
      printf '%s\n' 'pueue 99.7.3'; exit 0 ;;
    parallel)
      [ "$2" = "0" ] || exit 64
      touch "$(dirname "$config")/parallel-unlimited"
      exit 0 ;;
	    group)
	      marker="$(dirname "$config")/ready"
	      [ -f "$marker" ] || exit 1
	      parallel=1
	      [ -f "$(dirname "$config")/parallel-unlimited" ] && parallel=0
	      printf '{"default":{"status":"Running","parallel_tasks":%s}}\n' "$parallel"
	      exit 0 ;;
    shutdown)
      rm -f "$(dirname "$config")/ready"
      rm -f "$(dirname "$config")/run/pueue.pid"
      if [ "${PRIVATE_SHUTDOWN_MARKER:-}" != "" ]; then : > "$PRIVATE_SHUTDOWN_MARKER"; fi
      exit 0 ;;
	clean)
	      [ "$2" = "--successful-only" ] || exit 64
	      if [ "${PRIVATE_CLEAN_MARKER:-}" != "" ]; then : > "$PRIVATE_CLEAN_MARKER"; fi
	      if [ "${PRIVATE_CLEAN_ARGS_MARKER:-}" != "" ]; then printf '%s\n' "$@" | tail -n +2 > "$PRIVATE_CLEAN_ARGS_MARKER"; fi
      rm -f "$(dirname "$config")/test-status.json"
      exit 0 ;;
    status)
      if [ "${PRIVATE_STATUS_DELAY:-}" != "" ]; then sleep "$PRIVATE_STATUS_DELAY"; fi
      if [ "${PRIVATE_STATUS_STARTED:-}" != "" ]; then
        : > "$PRIVATE_STATUS_STARTED"
        while [ ! -f "$PRIVATE_STATUS_RELEASE" ]; do sleep 0.01; done
      fi
      marker="$(dirname "$config")/ready"
      [ -f "$marker" ] || exit 1
      status_file="$(dirname "$config")/test-status.json"
      if [ -f "$status_file" ]; then
        cat "$status_file"
        exit 0
      fi
      if [ "${PRIVATE_INVALID_STATUS:-}" = "1" ]; then
        printf '%s\n' '{"tasks":{}}'
        exit 0
      fi
      printf '%s\n' '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}'
      exit 0
      ;;
    kill|remove)
      if [ "$1" = "remove" ] && [ "${PRIVATE_REMOVE_STARTED:-}" != "" ]; then
        : > "$PRIVATE_REMOVE_STARTED"
        while [ ! -f "$PRIVATE_REMOVE_RELEASE" ]; do sleep 0.01; done
      fi
      if [ -f "$(dirname "$config")/test-record-kill" ]; then shift; printf '%s\n' "$@" >> "$(dirname "$config")/test-kill-task-id"; fi
      exit 0 ;;
    *) exit 64 ;;
  esac
done
exit 64
`

func waitForPrivateFixtureMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect private status start marker: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("private status fixture did not reach its blocking point")
}

func releasePrivateStatus(t *testing.T, marker string) {
	t.Helper()
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func releasePrivateStatusOnCleanup(t *testing.T, marker string) {
	t.Helper()
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Errorf("release private status fixture: %v", err)
	}
}

const privateDaemonFixture = `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c) config=$2; shift 2 ;;
    *) shift ;;
  esac
done
identity="$(dirname "$config")/daemon.identity.json"
if [ -f "$identity" ]; then printf '%s\n' present; else printf '%s\n' absent; fi > "$(dirname "$config")/daemon-identity-at-start"
printf '%s\n' started >> "$(dirname "$config")/daemon-started"
mkdir -p "$(dirname "$config")/run"
printf '%s\n' "$$" > "$(dirname "$config")/run/pueue.pid"
touch "$(dirname "$config")/ready"
`

func TestPendingPrivateMigrationResumesPersistedPID(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := privateMigrationTestOptions(fixture.stateRoot, 10*time.Second)
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "proof-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	var err error
	base := filepath.Dir(current.Binding().ConfigPath)
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(legacy.Binding()), Successor: privateDaemonIdentityForBinding(current.Binding())}
	if err = publishPrivateMigrationRecord(base, pending.Legacy, pending.Successor); err != nil {
		t.Fatal(err)
	}
	saved := pending
	saved.LegacyPID, _, err = legacy.privateDaemonPID()
	if err != nil {
		t.Fatal(err)
	}
	if err = replacePrivateMigrationRecord(base, pending, saved); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	defer cancel()
	err = WithBootstrapLock(ctx, fixture.stateRoot, func() error { return current.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot) })
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, nextPath)
	if _, found, err := loadPrivateMigrationRecord(base); err != nil || found {
		t.Fatalf("journal not cleared: found=%v error=%v", found, err)
	}
}

func TestPendingPrivateMigrationAfterDispatchedShutdownWaitsForRecordedPID(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	shutdownMarker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "shutdown-marker")
	options := privateMigrationTestOptions(fixture.stateRoot, 10*time.Second)
	options.Environment = append(os.Environ(), "PRIVATE_SHUTDOWN_MARKER="+shutdownMarker)
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "proof-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	base := filepath.Dir(current.Binding().ConfigPath)
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacy.Binding()),
		Successor:     privateDaemonIdentityForBinding(current.Binding()),
	}
	if err := publishPrivateMigrationRecord(base, pending.Legacy, pending.Successor); err != nil {
		t.Fatal(err)
	}
	dispatched := pending
	var err error
	process, present, err := legacy.privateDaemonProcessIdentity()
	if err != nil || !present {
		t.Fatal(err)
	}
	dispatched.LegacyPID = process.PID
	dispatched.LegacyProcessStart = process.StartToken
	dispatched.LegacyShutdownRequested = true
	dispatched.LegacyShutdownDispatched = true
	if err = replacePrivateMigrationRecord(base, pending, dispatched); err != nil {
		t.Fatal(err)
	}
	if recorded, found, loadErr := loadPrivateMigrationRecord(base); loadErr != nil || !found || !recorded.LegacyShutdownDispatched {
		t.Fatalf("shutdown phase was not durable before dispatch: record=%+v found=%t err=%v", recorded, found, loadErr)
	}
	if err = legacy.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPath(t, shutdownMarker)

	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	defer cancel()
	err = WithBootstrapLock(ctx, fixture.stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot)
	})
	if err != nil {
		t.Fatalf("resume after dispatched shutdown failed: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, nextPath)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateTestPathMissing(t, shutdownMarker)
	if _, found, err := loadPrivateMigrationRecord(base); err != nil || found {
		t.Fatalf("journal not cleared after dispatched shutdown: found=%v error=%v", found, err)
	}
}

func TestPrivateMigrationRetriesKnownPreStartShutdownFailure(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	var removed atomic.Bool
	var shutdownStarts atomic.Int32
	options := privateMigrationRetryOptions(t, fixture, &removed, &shutdownStarts)
	legacy, current, nextPath := setupPrivateMigrationRetry(t, fixture, options)
	t.Cleanup(func() {
		cleanupPrivateMigrationRetry(t, current)
	})

	if err := runPrivateMigrationRetryAttempt(t, fixture.stateRoot, current, options); err == nil {
		t.Fatal("migration succeeded after a pre-Start shutdown failure")
	}
	assertPrivateMigrationRetryPending(t, fixture.stateRoot, &shutdownStarts)
	restorePrivateMigrationRetryPredecessor(t, fixture, legacy)

	if err := runPrivateMigrationRetryAttempt(t, fixture.stateRoot, current, options); err != nil {
		t.Fatalf("retryable shutdown request did not recover: %v", err)
	}
	assertPrivateMigrationRetryCompleted(t, fixture.stateRoot, nextPath, &shutdownStarts)
}

func privateMigrationRetryOptions(t *testing.T, fixture privateSupervisorTestFixture, removed *atomic.Bool, shutdownStarts *atomic.Int32) Options {
	t.Helper()
	options := privateMigrationTestOptions(fixture.stateRoot, privateTestObservationTimeout)
	options.Observer = privateMigrationRetryObserver(t, fixture, removed, shutdownStarts)
	return options
}

func privateMigrationRetryObserver(t *testing.T, fixture privateSupervisorTestFixture, removed *atomic.Bool, shutdownStarts *atomic.Int32) func(CommandEvent) {
	t.Helper()
	return func(event CommandEvent) {
		observePrivateMigrationRetryEvent(t, fixture, removed, shutdownStarts, event)
	}
}

func observePrivateMigrationRetryEvent(t *testing.T, fixture privateSupervisorTestFixture, removed *atomic.Bool, shutdownStarts *atomic.Int32, event CommandEvent) {
	t.Helper()
	if len(event.Argv) == 0 || event.Argv[len(event.Argv)-1] != "shutdown" {
		return
	}
	if event.Stage == "entry" && removed.CompareAndSwap(false, true) {
		if err := os.Remove(fixture.clientPath); err != nil {
			t.Errorf("remove client before shutdown Start: %v", err)
		}
	}
	if event.Stage == "started" {
		shutdownStarts.Add(1)
	}
}

func setupPrivateMigrationRetry(t *testing.T, fixture privateSupervisorTestFixture, options Options) (*Client, *Client, string) {
	t.Helper()
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "retry-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# retry successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	return legacy, current, nextPath
}

func cleanupPrivateMigrationRetry(t *testing.T, current *Client) {
	t.Helper()
	if err := current.Shutdown(context.Background()); err != nil {
		t.Logf("cleanup successor: %v", err)
	}
}

func runPrivateMigrationRetryAttempt(t *testing.T, stateRoot string, current *Client, options Options) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	err := WithBootstrapLock(ctx, stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(ctx, stateRoot)
	})
	cancel()
	return err
}

func assertPrivateMigrationRetryPending(t *testing.T, stateRoot string, shutdownStarts *atomic.Int32) {
	t.Helper()
	pending, found, loadErr := loadPrivateMigrationRecord(filepath.Join(stateRoot, privateSupervisorDirectory))
	if loadErr != nil || !found {
		t.Fatalf("retryable shutdown request was not retained: record=%+v found=%t err=%v", pending, found, loadErr)
	}
	if !pending.LegacyShutdownRequested || pending.LegacyShutdownDispatched || pending.LegacyStopped || pending.LegacyProcessStart == "" {
		t.Fatalf("pre-Start failure advanced the shutdown phase: %+v", pending)
	}
	if got := shutdownStarts.Load(); got != 0 {
		t.Fatalf("failed shutdown reported %d successful starts, want zero", got)
	}
}

func restorePrivateMigrationRetryPredecessor(t *testing.T, fixture privateSupervisorTestFixture, legacy *Client) {
	t.Helper()
	writeExecutable(t, fixture.clientPath, privateClientFixture)
	reachable, reachableErr := legacy.Reachable(context.Background())
	if reachableErr != nil || !reachable {
		t.Fatalf("predecessor was not preserved after pre-Start failure: reachable=%t err=%v", reachable, reachableErr)
	}
}

func assertPrivateMigrationRetryCompleted(t *testing.T, stateRoot, nextPath string, shutdownStarts *atomic.Int32) {
	t.Helper()
	if got := shutdownStarts.Load(); got != 1 {
		t.Fatalf("recovery started %d shutdown commands, want one", got)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, nextPath)
	if _, found, err := loadPrivateMigrationRecord(filepath.Join(stateRoot, privateSupervisorDirectory)); err != nil || found {
		t.Fatalf("completed retry left migration journal: found=%t err=%v", found, err)
	}
}

func TestRecoverPrivateDoesNotRestartDispatchedPredecessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	shutdownMarker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "shutdown-marker")
	options := privateMigrationTestOptions(fixture.stateRoot, 10*time.Second)
	options.Environment = append(os.Environ(), "PRIVATE_SHUTDOWN_MARKER="+shutdownMarker)
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "proof-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	t.Cleanup(func() {
		if err := current.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(current.Binding().ConfigPath)
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacy.Binding()),
		Successor:     privateDaemonIdentityForBinding(current.Binding()),
	}
	if err := publishPrivateMigrationRecord(base, pending.Legacy, pending.Successor); err != nil {
		t.Fatal(err)
	}
	dispatched := pending
	var err error
	process, present, err := legacy.privateDaemonProcessIdentity()
	if err != nil || !present {
		t.Fatal(err)
	}
	dispatched.LegacyPID = process.PID
	dispatched.LegacyProcessStart = process.StartToken
	dispatched.LegacyShutdownRequested = true
	dispatched.LegacyShutdownDispatched = true
	if err = replacePrivateMigrationRecord(base, pending, dispatched); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPath(t, shutdownMarker)

	if _, err = RecoverPrivate(context.Background(), fixture.stateRoot, legacy.Binding(), fixture.clientPath, fixture.daemonPath, options); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("dispatched predecessor was recovered: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	assertPrivateTestPathMissing(t, shutdownMarker)
	stopped, found, err := loadPrivateMigrationRecord(base)
	if err != nil || !found || !stopped.LegacyStopped {
		t.Fatalf("recovery did not record predecessor absence: record=%+v found=%t error=%v", stopped, found, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	defer cancel()
	if err = WithBootstrapLock(ctx, fixture.stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot)
	}); err != nil {
		t.Fatalf("successor was not launched after predecessor absence: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, nextPath)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateTestPathMissing(t, shutdownMarker)
}

func TestPendingPrivateMigrationResumesPublishedSuccessor(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("later-installation=%v", upgrade), func(t *testing.T) {
			testResumePublishedPrivateSuccessor(t, upgrade)
		})
	}
}

func testResumePublishedPrivateSuccessor(t *testing.T, upgrade bool) {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	options := privateMigrationTestOptions(fixture.stateRoot, 10*time.Second)
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "proof-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	var err error
	base := filepath.Dir(current.Binding().ConfigPath)
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(legacy.Binding()), Successor: privateDaemonIdentityForBinding(current.Binding())}
	if err = publishPrivateMigrationRecord(base, pending.Legacy, pending.Successor); err != nil {
		t.Fatal(err)
	}
	saved := pending
	saved.LegacyPID, _, err = legacy.privateDaemonPID()
	if err != nil {
		t.Fatal(err)
	}
	if err = replacePrivateMigrationRecord(base, pending, saved); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = legacy.WaitUnavailableWithPID(context.Background(), saved.LegacyPID); err != nil {
		t.Fatal(err)
	}
	stopped := saved
	stopped.LegacyStopped = true
	if err = replacePrivateMigrationRecord(base, saved, stopped); err != nil {
		t.Fatal(err)
	}
	if err = current.RestartPrivate(context.Background(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if upgrade {
		latestPath := filepath.Join(filepath.Dir(fixture.daemonPath), "latest-successor")
		writeExecutable(t, latestPath, privateDaemonFixture+"\n# latest successor\n")
		current, err = BindPrivate(context.Background(), fixture.clientPath, latestPath, fixture.stateRoot, options)
		if err != nil {
			t.Fatal(err)
		}
		nextPath = latestPath
	}
	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	defer cancel()
	err = WithBootstrapLock(ctx, fixture.stateRoot, func() error { return current.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot) })
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, nextPath)
	if upgrade {
		assertRecordedPrivateSuccessorCompleted(t, base, stopped)
	}
	if _, found, err := loadPrivateMigrationRecord(base); err != nil || found {
		t.Fatalf("journal not cleared: found=%v error=%v", found, err)
	}
}

func assertRecordedPrivateSuccessorCompleted(t *testing.T, base string, stopped privateMigrationRecord) {
	t.Helper()
	history, found, err := loadPrivateMigrationHistory(base)
	if err != nil || !found || len(history.Migrations) != 2 {
		t.Fatalf("migration history: %+v found=%v err=%v", history, found, err)
	}
	if history.Migrations[0] != stopped {
		t.Fatal("recorded successor was retargeted after shutdown")
	}
}

func TestPrivateMaintenanceRetainsPendingCommandLock(t *testing.T) {
	for _, command := range []string{"status", "remove"} {
		t.Run(command, func(t *testing.T) {
			fixture := newPrivateSupervisorTestFixture(t)
			client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
			row := statusTestJob(0, "finished", statusStreamTestDoneState("Success"))
			data := statusTestPayload(t, map[string]any{"0": row}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
			if err := os.WriteFile(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			base := canonicalTemp(t)
			started, release := filepath.Join(base, "started"), filepath.Join(base, "release")
			defer releasePrivateStatusOnCleanup(t, release)
			client.options.ObservationTimeout = time.Second
			prefix := "PRIVATE_" + strings.ToUpper(command)
			client.options.Environment = append(os.Environ(), prefix+"_STARTED="+started, prefix+"_RELEASE="+release)
			client.options.SuccessfulCleanupAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) (PrivateQueueCleanupPlan, error) {
				return PrivateQueueCleanupPlan{RemoveIDs: []int64{0}}, nil
			}
			cancel, err := configurePrivateTestParallelismAndMaintain(context.Background(), client)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			waitForPrivateFixtureMarker(t, started)
			supervisorLeaseTestBlocked(t, fixture.stateRoot, WithBootstrapLock)
			releasePrivateStatus(t, release)
			supervisorLeaseTestEnters(t, fixture.stateRoot, WithBootstrapLock)
		})
	}
}

func configurePrivateTestParallelismAndMaintain(ctx context.Context, client *Client) (context.CancelFunc, error) {
	controlContext, cancel, err := client.privateControlContext(ctx)
	if err != nil {
		return func() {}, err
	}
	err = client.withPrivateSupervisorLease(controlContext, func(context.Context) error {
		return client.setPrivateDefaultParallelTasksLocked(controlContext, 0)
	})
	if err != nil {
		cancel()
		return func() {}, err
	}
	client.MaintainPrivateQueue(controlContext)
	return cancel, nil
}
