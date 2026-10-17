package control

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
