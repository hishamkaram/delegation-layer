package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
		_, recoverErr := recoverPrivateIdentityPublication(fixture.stateRoot)
		return recoverErr
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
