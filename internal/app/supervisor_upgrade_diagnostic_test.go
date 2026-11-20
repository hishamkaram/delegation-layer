package app

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSupervisorUpgradeStatusUnavailablePredecessorPreservesDrain(t *testing.T) {
	for _, endpoint := range []string{"absent", "refused"} {
		t.Run(endpoint, func(t *testing.T) {
			fixture := newUnavailableUpgradeDiagnosticFixture(t, endpoint)
			var stdout, stderr bytes.Buffer
			code := Run([]string{"--root", fixture.root, "supervisor", "status", "--json"}, &stdout, &stderr, fixture.dependencies())
			response := decodeHistoricalResponse(t, stdout.Bytes(), stderr.Bytes(), code)
			if code != 0 || response.Error != "" {
				t.Fatalf("clean unavailable predecessor is not a status fault: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
			}
			assertUnavailableUpgradeDiagnostic(t, response.SupervisorControl)
		})
	}
}

func TestSupervisorEnsureOwnerUnavailablePredecessorPreservesDrain(t *testing.T) {
	cleanupFault := errors.Join(pueue.ErrPrivateUpgradePending, os.ErrPermission)
	for _, scenario := range []struct {
		name     string
		endpoint string
		cause    error
		code     int
	}{
		{name: "absent", endpoint: "absent", code: 3},
		{name: "absent/cleanup-fault", endpoint: "absent", cause: cleanupFault, code: 1},
		{name: "refused", endpoint: "refused", code: 3},
		{name: "refused/cleanup-fault", endpoint: "refused", cause: cleanupFault, code: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newUnavailableUpgradeDiagnosticFixture(t, scenario.endpoint)
			routing := &supervisorUpgradeReplyRouting{
				componentPreparationRouting: componentPreparationRouting{test: t, owner: fixture.component, client: fixture.clientB, options: fixture.options},
				outcome: infrastructure.UpgradeOutcome{
					Condition: infrastructure.UpgradePending,
					Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed},
					Report: infrastructure.UpgradeReport{
						OperationID: infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("9", 32)),
						Mode:        infrastructure.UpgradeOperationRequired,
						Phase:       infrastructure.UpgradeDraining,
						Predecessor: fixture.predecessor,
					},
				},
				cause: scenario.cause,
			}
			deps := fixture.dependencies()
			deps.ownerRouting = routing
			result := supervisorCommand(Arguments{Root: fixture.root, SupervisorAction: "ensure-owner"}, deps)
			if routing.calls != 1 || result.code != scenario.code {
				t.Fatalf("ensure-owner route/result changed: calls=%d code=%d want=%d err=%v", routing.calls, result.code, scenario.code, result.err)
			}
			control := result.response.SupervisorControl
			assertUnavailableUpgradeDiagnostic(t, control)
			if scenario.cause == nil {
				if result.err != nil {
					t.Fatalf("unavailable diagnostic changed the pending drain: code=%d control=%+v err=%v", result.code, control, result.err)
				}
				return
			}
			if !errors.Is(result.err, os.ErrPermission) || !errors.Is(result.err, pueue.ErrPrivateUpgradePending) {
				t.Fatalf("unavailable diagnostic hid the concrete cleanup fault: code=%d control=%+v err=%v", result.code, control, result.err)
			}
		})
	}
}

func assertUnavailableUpgradeDiagnostic(t *testing.T, control *SupervisorControlResponse) {
	t.Helper()
	if control == nil || control.Upgrade == nil {
		t.Fatalf("unavailable diagnostic lost the upgrade report: %+v", control)
	}
	if control.Upgrade.OperationID != "upgrade-v1:"+strings.Repeat("9", 32) || control.Upgrade.Mode != "operation_required" || control.Upgrade.Phase != "draining" || control.MigrationPhase != "draining" || control.Upgrade.Completed {
		t.Fatalf("unavailable diagnostic changed upgrade identity or progress: %+v", control)
	}
	if control.ActiveWork != "unknown" || control.MigrationReadiness != "unknown" || control.DiagnosticSnapshotAt == "" {
		t.Fatalf("unavailable queue was presented as observed work or clear maintenance: %+v", control)
	}
	blocker := supervisorcore.MaintenanceBlocker{Kind: "observation", ReasonCode: "predecessor_unavailable", NextAction: "stop_and_report"}
	if control.NextAction != "stop_and_report" || control.FirstBlocker == nil || *control.FirstBlocker != blocker {
		t.Fatalf("unavailable predecessor lost its observation blocker: control=%+v blocker=%+v", control, control.FirstBlocker)
	}
}

func newUnavailableUpgradeDiagnosticFixture(t *testing.T, endpoint string) *historicalSupervisorFixture {
	t.Helper()
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	waits := fixture.component.daemonWaits()
	if err := fixture.component.stopDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
	if err := awaitComponentDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
	socketPath := strings.TrimPrefix(fixture.predecessor.Endpoint, "unix:")
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped historical fixture left an unexpected endpoint: %v", err)
	}
	if endpoint == "refused" {
		refused := leaveRefusedUpgradeDiagnosticEndpoint(t, socketPath)
		t.Cleanup(func() { refused.assertSocketPreserved(t) })
	} else {
		t.Cleanup(func() {
			if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("diagnostic recreated the absent endpoint: %v", err)
			}
		})
	}
	supervisorfixture.WriteUpgradeDrain(t, fixture.store, fixture.predecessor)
	journalPath := filepath.Join(fixture.root, ".supervisor", "bootstrap.pending")
	journal := readSupervisorProbeTestFile(t, journalPath)
	before := snapshotObservationPhaseRecords(t, fixture)
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	commands := observePhaseStatusCommands(fixture)
	t.Cleanup(func() {
		assertSupervisorProbeTestFile(t, journalPath, journal)
		assertObservationPhaseRecords(t, before)
		if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(taskBefore, after) {
			t.Error("diagnostic changed or closed the admitted task")
		}
		assertObservationPhaseNoMutation(t, commands())
	})
	return fixture
}

func leaveRefusedUpgradeDiagnosticEndpoint(t *testing.T, path string) refusedEndpointAppFixture {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	connection, dialErr := dialer.DialContext(t.Context(), "unix", path)
	if connection != nil {
		if closeErr := connection.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Fatalf("closed predecessor endpoint is not a proven refusal: %v", dialErr)
	}
	return refusedEndpointAppFixture{socketPath: path, socket: info}
}

func TestSupervisorUpgradeDiagnosticUsesInstalledPairAfterOldRemoval(t *testing.T) {
	store, deps, original, owner := newOwnedSupervisorProbeTestFixture(t)
	old := original.Binding()
	currentClient, currentDaemon := writePublicContinuationPair(t, owner.base, "D")
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(map[string]any{"schema_version": task.SchemaVersion, "drain_v1": map[string]any{"root": map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}, "predecessor": old, "operation_id": "upgrade-v1:" + strings.Repeat("9", 32), "mode": "reactive", "phase": "draining"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "bootstrap.pending"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{old.ClientExecutable, old.DaemonExecutable} {
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := supervisorMaintenanceClient(ctx, store, old, currentClient, currentDaemon, deps.SupervisorOptions)
	defer func() { settleSupervisorProbeTestClient(t, client, err) }()
	if err != nil || client == nil {
		t.Fatalf("deleted predecessor pair blocked installed diagnostic transport: %v", err)
	}
	if client.Binding() != old || client.RuntimeBinding().ClientExecutable != currentClient || client.RuntimeBinding().DaemonExecutable != currentDaemon {
		t.Fatalf("diagnostic changed logical provenance or selected a mixed pair: logical=%+v runtime=%+v", client.Binding(), client.RuntimeBinding())
	}
	if _, err = client.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(old.ClientExecutable); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("diagnostic recreated predecessor client")
	}
	if _, err = os.Stat(old.DaemonExecutable); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("diagnostic recreated predecessor daemon")
	}
}
