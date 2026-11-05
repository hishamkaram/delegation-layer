package control

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// newCompactRefusedPrivateEndpointFixture is the refused-endpoint fixture
// under a native temporary root padded so the private config owner selects
// its compact endpoint for the current platform.
func newCompactRefusedPrivateEndpointFixture(t *testing.T) refusedPrivateEndpointFixture {
	t.Helper()
	parent, err := os.MkdirTemp("", "ce-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cleanupErr := os.RemoveAll(parent); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	if parent, err = filepath.EvalSymlinks(parent); err != nil {
		t.Fatal(err)
	}
	limit, ok := unixSocketPathLimit(runtime.GOOS)
	if !ok {
		t.Skipf("no native Unix socket limit for %s", runtime.GOOS)
	}
	// A base this long overflows base/run/pueue.sock but still binds base/s.
	targetBaseLength := limit - 8
	padding := targetBaseLength - len(parent) - 2 - len(privateSupervisorDirectory)
	if padding < 1 {
		t.Fatalf("native temporary parent is too long for the compact fixture: %q", parent)
	}
	stateRoot := filepath.Join(parent, strings.Repeat("c", padding))
	writePrivateStateRootRecord(t, stateRoot)
	fixture := privateSupervisorTestFixture{stateRoot: stateRoot, clientPath: filepath.Join(parent, "pueue"), daemonPath: filepath.Join(parent, "pueued")}
	writeExecutable(t, fixture.clientPath, privateClientFixture)
	writeExecutable(t, fixture.daemonPath, privateDaemonFixture)
	client := bindPrivateForTest(t, stateRoot, fixture.clientPath, fixture.daemonPath)
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	socketPath := strings.TrimPrefix(client.runtimeBinding.Endpoint, "unix:")
	if runLayout := filepath.Join(base, "run", "pueue.sock"); len(runLayout) <= limit || socketPath != filepath.Join(base, "s") {
		t.Fatalf("private config owner did not select the compact endpoint: run=%d limit=%d endpoint=%q", len(runLayout), limit, socketPath)
	}
	removePrivateTestPath(t, filepath.Join(base, "ready"))
	return refusedPrivateEndpointFixture{
		privateSupervisorTestFixture: fixture,
		client:                       client,
		socketPath:                   socketPath,
		socket:                       listenRefusedPrivateTestSocket(t, socketPath),
	}
}

func TestBindPrivateClosesRefusedCompactEndpointOfAbsentRecordedDaemon(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	published := fixture.client.Binding()

	client, err := fixture.bind(t)
	if err != nil {
		t.Fatalf("refused compact endpoint of an absent recorded daemon was not recovered: %v", err)
	}
	fixture.assertSocketRemoved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, client.Binding())
	if current, found, readErr := loadPrivateSupervisorBinding(fixture.stateRoot); readErr != nil || !found || current != published {
		t.Fatalf("recovery changed the published generation: %+v found=%t err=%v", current, found, readErr)
	}
}

func TestRecoverPrivateClosesRefusedCompactEndpoint(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	saved := fixture.client.Binding()
	ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
	defer cancel()

	recovered, err := RecoverPrivate(ctx, fixture.stateRoot, saved, fixture.clientPath, fixture.daemonPath, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatalf("saved recovery did not close the refused compact endpoint: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := recovered.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if recovered.Binding() != saved {
		t.Fatalf("recovered client changed the saved binding: %+v", recovered.Binding())
	}
	fixture.assertSocketRemoved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func TestWaitUnavailableRemovesRefusedCompactEndpoint(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
	defer cancel()

	err := WithBootstrapLockContext(ctx, fixture.stateRoot, fixture.client.WaitUnavailable)
	if err != nil {
		t.Fatalf("shutdown wait did not drain the refused compact endpoint: %v", err)
	}
	fixture.assertSocketRemoved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestConcurrentBindPrivateRepairsRefusedCompactEndpointOnce(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	const callers = 4
	errs := make([]error, callers)
	var group sync.WaitGroup
	for index := range callers {
		group.Go(func() {
			client, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout})
			if err == nil {
				err = client.Close()
			}
			errs[index] = err
		})
	}
	group.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("caller %d did not observe the repaired compact endpoint: %v", index, err)
		}
	}
	fixture.assertSocketRemoved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func TestRefusedCompactEndpointClosureFailsClosed(t *testing.T) {
	t.Run("missing PID", testRefusedCompactEndpointMissingPID)
	t.Run("present PID", testRefusedCompactEndpointPresentPID)
	t.Run("config changed", testRefusedCompactEndpointConfigChanged)
	t.Run("unselected sibling", testRefusedCompactEndpointUnselectedSibling)
	t.Run("outside base", testRefusedCompactEndpointOutsideBase)
	t.Run("accepting replacement", testRefusedCompactEndpointAcceptingReplacement)
	t.Run("regular file", testRefusedCompactEndpointRegularFile)
	t.Run("symlink", testRefusedCompactEndpointSymlink)
	t.Run("canceled observation", testRefusedCompactEndpointCanceled)
}

// closeCompactUnderLock enters the canonical closure the way its callers do:
// under the exclusive bootstrap lock with a canonical refusal observation.
func closeCompactUnderLock(t *testing.T, fixture refusedPrivateEndpointFixture) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
	defer cancel()
	observation, exitErr := canonicalRefusedObservation(t, fixture)
	err := WithBootstrapLockContext(ctx, fixture.stateRoot, func(lockedContext context.Context) error {
		return fixture.client.closeRefusedPrivateEndpoint(lockedContext, observation)
	})
	if err != nil && !errors.Is(err, exitErr) {
		t.Fatalf("closure dropped the refusal command cause: %v", err)
	}
	return err
}

func assertCompactRefusalPreserved(t *testing.T, fixture refusedPrivateEndpointFixture, err error) {
	t.Helper()
	if !errors.Is(err, ErrBinding) || !errors.Is(err, ErrPrivateEndpointRefused) {
		t.Fatalf("compact closure lost the refusal identity: %v", err)
	}
	fixture.assertSocketPreserved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func testRefusedCompactEndpointMissingPID(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	removePrivateTestPID(t, fixture.stateRoot)
	_, err := fixture.bind(t)
	assertRefusedEndpointReason(t, err, privateEndpointOwnerUnknownReason)
	assertCompactRefusalPreserved(t, fixture, err)
}

func testRefusedCompactEndpointPresentPID(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	process, _, finishProcess := startBlockingProcess(t)
	pidPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(process.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.bind(t)
	if finishErr := finishProcess(); finishErr != nil {
		t.Fatal(finishErr)
	}
	assertRefusedEndpointReason(t, err, privateEndpointRefusedLiveReason)
	assertCompactRefusalPreserved(t, fixture, err)
}

func testRefusedCompactEndpointConfigChanged(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	observation, exitErr := canonicalRefusedObservation(t, fixture)
	configPath := fixture.client.runtimeBinding.ConfigPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// The changed config still resolves to the same compact socket, but it is
	// no longer the generated config bound by this client.
	if err = os.WriteFile(configPath, append(data, []byte("# changed\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	err = WithBootstrapLockContext(context.Background(), fixture.stateRoot, func(lockedContext context.Context) error {
		return fixture.client.closeRefusedPrivateEndpoint(lockedContext, observation)
	})
	if !errors.Is(err, exitErr) || !strings.Contains(errorText(err), "outside its runtime directory") {
		t.Fatalf("changed config authorized the compact endpoint: %v", err)
	}
	assertCompactRefusalPreserved(t, fixture, err)
}

func testRefusedCompactEndpointUnselectedSibling(t *testing.T) {
	t.Run("compact root", func(t *testing.T) {
		fixture := newCompactRefusedPrivateEndpointFixture(t)
		sibling := filepath.Join(filepath.Dir(fixture.socketPath), "t")
		assertForgedEndpointRefused(t, fixture, sibling)
	})
	t.Run("run layout root", func(t *testing.T) {
		// A compact-looking base/s is not the endpoint the owner selected for
		// a root whose run layout fits the native limit.
		fixture := newRefusedPrivateEndpointFixture(t)
		forged := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "s")
		assertForgedEndpointRefused(t, fixture, forged)
	})
}

func testRefusedCompactEndpointOutsideBase(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	assertForgedEndpointRefused(t, fixture, filepath.Join(filepath.Dir(fixture.stateRoot), "s"))
}

// assertForgedEndpointRefused points the runtime binding at a refused socket
// the config owner did not select and proves the closure keeps both inodes.
func assertForgedEndpointRefused(t *testing.T, fixture refusedPrivateEndpointFixture, forged string) {
	t.Helper()
	forgedInfo := listenRefusedPrivateTestSocket(t, forged)
	fixture.client.runtimeBinding.Endpoint = "unix:" + forged
	err := fixture.client.closeRefusedPrivateEndpoint(context.Background(), staleRefusedObservation(t))
	if !errors.Is(err, ErrBinding) || !errors.Is(err, ErrPrivateEndpointRefused) || !strings.Contains(err.Error(), "outside its runtime directory") {
		t.Fatalf("forged endpoint was not refused: %v", err)
	}
	if info, statErr := os.Lstat(forged); statErr != nil || !os.SameFile(info, forgedInfo) {
		t.Fatalf("forged endpoint changed: %v", statErr)
	}
	fixture.assertSocketPreserved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func testRefusedCompactEndpointAcceptingReplacement(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	removePrivateTestPath(t, fixture.socketPath)
	listener, err := net.Listen("unix", fixture.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
	})
	err = fixture.client.closeRefusedPrivateEndpoint(context.Background(), staleRefusedObservation(t))
	if !errors.Is(err, ErrBinding) || !errors.Is(err, ErrPrivateEndpointRefused) {
		t.Fatalf("closure accepted a changed live compact endpoint: %v", err)
	}
	connection, err := net.Dial("unix", fixture.socketPath)
	if err != nil {
		t.Fatalf("closure disturbed the accepting compact endpoint: %v", err)
	}
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func testRefusedCompactEndpointRegularFile(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	removePrivateTestPath(t, fixture.socketPath)
	if err := os.WriteFile(fixture.socketPath, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := fixture.client.closeRefusedPrivateEndpoint(context.Background(), staleRefusedObservation(t))
	if !errors.Is(err, ErrBinding) || !errors.Is(err, ErrPrivateEndpointRefused) {
		t.Fatalf("regular file at the compact endpoint was not refused: %v", err)
	}
	if data, readErr := os.ReadFile(fixture.socketPath); readErr != nil || string(data) != "retained" {
		t.Fatalf("regular file at the compact endpoint changed: %q %v", data, readErr)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func testRefusedCompactEndpointSymlink(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	target := filepath.Join(filepath.Dir(fixture.stateRoot), "f")
	targetInfo := listenRefusedPrivateTestSocket(t, target)
	removePrivateTestPath(t, fixture.socketPath)
	if err := os.Symlink(target, fixture.socketPath); err != nil {
		t.Fatal(err)
	}
	err := fixture.client.closeRefusedPrivateEndpoint(context.Background(), staleRefusedObservation(t))
	if !errors.Is(err, ErrPrivateEndpointRefused) {
		t.Fatalf("symlinked compact endpoint was not refused: %v", err)
	}
	if info, statErr := os.Lstat(fixture.socketPath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlinked compact endpoint changed: %v", statErr)
	}
	if info, statErr := os.Lstat(target); statErr != nil || !os.SameFile(info, targetInfo) {
		t.Fatalf("symlink target changed: %v", statErr)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func testRefusedCompactEndpointCanceled(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := fixture.client.closeRefusedPrivateEndpoint(ctx, staleRefusedObservation(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled closure did not keep its cause: %v", err)
	}
	assertCompactRefusalPreserved(t, fixture, err)
}

func TestRefusedCompactEndpointClosureUnderLock(t *testing.T) {
	fixture := newCompactRefusedPrivateEndpointFixture(t)
	if err := closeCompactUnderLock(t, fixture); err != nil {
		t.Fatalf("canonical closure did not remove the refused compact endpoint: %v", err)
	}
	fixture.assertSocketRemoved(t)
	// Reentry observes the drained endpoint as a no-op closure.
	if err := fixture.client.closeRefusedPrivateEndpoint(context.Background(), PrivateEndpointObservation{}); err != nil {
		t.Fatalf("closure reentry after repair failed: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// bindAcceptedCompactPrivate binds a root whose accepted config selects base/s
// although its run layout fits the native limit, as an earlier generation or a
// migration may have written it.
func bindAcceptedCompactPrivate(t *testing.T, fixture privateSupervisorTestFixture, clientPath string, options Options) *Client {
	t.Helper()
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		t.Fatal(err)
	}
	if err := markPrivateBootstrapPending(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if err := createPrivateConfig(PrivateConfigPath(fixture.stateRoot), base, privateConfigYAMLWithSocket(base, filepath.Join(base, "s"))); err != nil {
		t.Fatal(err)
	}
	client := bindPrivateSetupForTest(t, fixture.stateRoot, clientPath, fixture.daemonPath, options)
	if client.runtimeBinding.Endpoint != "unix:"+filepath.Join(base, "s") {
		t.Fatalf("accepted compact config was not bound: %q", client.runtimeBinding.Endpoint)
	}
	return client
}

// bindMigratedCompactPrivate changes the published config after binding, then
// binds the saved compact generation through its isolated migration config.
func bindMigratedCompactPrivate(t *testing.T) (refusedPrivateEndpointFixture, task.SupervisorRef) {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	first := bindAcceptedCompactPrivate(t, fixture, fixture.clientPath, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout})
	saved := first.Binding()
	base := filepath.Dir(saved.ConfigPath)
	data, err := os.ReadFile(saved.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := configWithSocketPath(data, filepath.Join(base, "t"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(saved.ConfigPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	migration, err := BindSavedForMigration(context.Background(), saved, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := migration.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if migration.runtimeBinding.ConfigPath == saved.ConfigPath || migration.runtimeBinding.Endpoint != saved.Endpoint {
		t.Fatalf("migration did not bind the saved compact endpoint: %+v", migration.runtimeBinding)
	}
	socketPath := filepath.Join(base, "s")
	removePrivateTestPath(t, filepath.Join(base, "ready"))
	return refusedPrivateEndpointFixture{
		privateSupervisorTestFixture: fixture,
		client:                       migration,
		socketPath:                   socketPath,
		socket:                       listenRefusedPrivateTestSocket(t, socketPath),
	}, saved
}

func TestMigrationConfigClosesRefusedCompactEndpoint(t *testing.T) {
	fixture, saved := bindMigratedCompactPrivate(t)
	if err := closeCompactUnderLock(t, fixture); err != nil {
		t.Fatalf("migration config did not close its refused compact endpoint: %v", err)
	}
	fixture.assertSocketRemoved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if current, found, err := loadPrivateSupervisorBinding(fixture.stateRoot); err != nil || !found || current != saved {
		t.Fatalf("closure changed the published generation: %+v found=%t err=%v", current, found, err)
	}
}

func TestPersistedRecoveryConfigClosesRefusedCompactEndpoint(t *testing.T) {
	fixture, saved := bindMigratedCompactPrivate(t)
	if err := fixture.client.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
	defer cancel()

	recovered, err := RecoverPrivate(ctx, fixture.stateRoot, saved, fixture.clientPath, fixture.daemonPath, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatalf("persisted compact recovery config did not close its refused endpoint: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := recovered.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if recovered.runtimeBinding.Endpoint != saved.Endpoint || recovered.runtimeBinding.ConfigPath == saved.ConfigPath {
		t.Fatalf("recovery did not use the persisted compact config: %+v", recovered.runtimeBinding)
	}
	fixture.assertSocketRemoved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	if current, found, loadErr := loadPrivateSupervisorBinding(fixture.stateRoot); loadErr != nil || !found || current != saved {
		t.Fatalf("recovery changed the published generation: %+v found=%t err=%v", current, found, loadErr)
	}
}

// TestDispatchedShutdownClosesRefusedEndpointWithoutLegacyClient resumes a
// dispatched predecessor shutdown after an upgrade removed the predecessor's
// client. The run layout needs only the absent recorded process; the accepted
// compact layout must not additionally require the removed executable.
func TestDispatchedShutdownClosesRefusedEndpointWithoutLegacyClient(t *testing.T) {
	t.Run("run layout", func(t *testing.T) { testDispatchedShutdownWithoutLegacyClient(t, false) })
	t.Run("compact layout", func(t *testing.T) { testDispatchedShutdownWithoutLegacyClient(t, true) })
}

func testDispatchedShutdownWithoutLegacyClient(t *testing.T, compact bool) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := privateMigrationTestOptions(fixture.stateRoot, 10*time.Second)
	legacyClient := filepath.Join(filepath.Dir(fixture.clientPath), "legacy-pueue")
	writeExecutable(t, legacyClient, privateClientFixture)
	var legacy *Client
	if compact {
		legacy = bindAcceptedCompactPrivate(t, fixture, legacyClient, options)
	} else {
		legacy = bindPrivateSetupForTest(t, fixture.stateRoot, legacyClient, fixture.daemonPath, options)
	}
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "proof-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	base := filepath.Dir(current.Binding().ConfigPath)
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(legacy.Binding()), Successor: privateDaemonIdentityForBinding(current.Binding())}
	if err := publishPrivateMigrationRecord(base, pending.Legacy, pending.Successor); err != nil {
		t.Fatal(err)
	}
	pid, _, err := legacy.privateDaemonPID()
	if err != nil {
		t.Fatal(err)
	}
	dispatched := pending
	dispatched.LegacyPID, dispatched.LegacyProcessStart = pid, "fixture:"+strconv.Itoa(pid)
	dispatched.LegacyShutdownRequested, dispatched.LegacyShutdownDispatched = true, true
	if err = replacePrivateMigrationRecord(base, pending, dispatched); err != nil {
		t.Fatal(err)
	}
	// The predecessor exited without unlinking its socket, and the upgrade
	// removed the predecessor's client before the migration resumed.
	removePrivateTestPath(t, filepath.Join(base, "ready"))
	socketPath := strings.TrimPrefix(legacy.runtimeBinding.Endpoint, "unix:")
	listenRefusedPrivateTestSocket(t, socketPath)
	removePrivateTestPath(t, legacyClient)

	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	defer cancel()
	err = WithBootstrapLock(ctx, fixture.stateRoot, func() error { return current.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot) })
	if err != nil {
		t.Fatalf("dispatched shutdown did not resume through the refused endpoint: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, nextPath)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateTestPathMissing(t, socketPath)
	if _, found, loadErr := loadPrivateMigrationRecord(base); loadErr != nil || found {
		t.Fatalf("journal not cleared: found=%t err=%v", found, loadErr)
	}
}
