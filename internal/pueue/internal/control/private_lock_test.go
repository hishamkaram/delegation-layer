package control

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// bootstrapPendingTestFixture owns the lock, children and transferred cleanup.
type bootstrapPendingTestFixture struct {
	lock        *bootstrapLock
	pending     []*Pending
	resultErr   error
	releasePath string
}

func (fixture *bootstrapPendingTestFixture) cleanup(t *testing.T, scope *testCallScope) {
	t.Helper()
	for _, pending := range fixture.pending {
		if !waitUntil(pending.Done(), scope.final) {
			t.Error("bootstrap child did not join before cleanup deadline")
			<-pending.Done()
		}
	}
	var owner *BootstrapCleanupError
	if errors.As(fixture.resultErr, &owner) {
		if !waitUntil(owner.Done(), scope.final) {
			t.Error("bootstrap lock owner did not finish before cleanup deadline")
			<-owner.Done()
		}
		if owner.Err() != nil {
			t.Error(owner.Err())
		}
	}
	if fixture.lock != nil {
		if closeErr := closeBootstrapLock(fixture.lock); closeErr != nil {
			t.Error(closeErr)
		}
	}
}

func TestFinishPrivateBootstrapRetainsLockForEveryPendingCommand(t *testing.T) {
	base := canonicalTemp(t)
	privateBase := filepath.Join(base, "state", privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(privateBase, "bootstrap.lock")
	lock, err := acquireBootstrapLock(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &bootstrapPendingTestFixture{lock: lock, releasePath: filepath.Join(base, "release")}
	scope := newTestCallScope(t, Options{})
	scope.onRelease(func() {
		if writeErr := os.WriteFile(fixture.releasePath, nil, 0o600); writeErr != nil {
			t.Error(writeErr)
		}
	})
	scope.joins = append(scope.joins, func() { fixture.cleanup(t, scope) })
	waitScript := `while [ ! -f "$1" ]; do sleep 0.01; done`
	for range 2 {
		fixture.pending = append(fixture.pending, transport.StartOwned(exec.Command("sh", "-c", waitScript, "wait", fixture.releasePath)))
	}
	fixture.resultErr = errors.Join(&InFlightError{Pending: fixture.pending[0]}, &InFlightError{Pending: fixture.pending[1]})
	finishPrivateBootstrap(&fixture.lock, &fixture.resultErr)

	lockContext, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	deferredLock, lockErr := acquireBootstrapLock(lockContext, lockPath)
	cancel()
	if deferredLock != nil {
		if closeErr := closeBootstrapLock(deferredLock); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !errors.Is(lockErr, context.DeadlineExceeded) {
		t.Fatalf("bootstrap lock was released before every pending command completed: %v", lockErr)
	}
	if writeErr := os.WriteFile(fixture.releasePath, nil, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	for _, pending := range fixture.pending {
		if !awaitPendingNaturally(pending, time.Second) {
			t.Fatal("pending bootstrap command did not complete")
		}
	}
	fixture.lock, err = acquireBootstrapLock(scope.context(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRetainBootstrapLockReportsDeferredReleaseFailure(t *testing.T) {
	base, err := os.MkdirTemp("", "closed-bootstrap-lock-")
	if err != nil {
		t.Fatal(err)
	}
	var file *os.File
	t.Cleanup(func() {
		if file != nil {
			if _, statErr := file.Stat(); !errors.Is(statErr, os.ErrClosed) {
				t.Errorf("retained bootstrap fixture at %s: %v", base, statErr)
				return
			}
		}
		if removeErr := os.RemoveAll(base); removeErr != nil {
			t.Error(removeErr)
		}
	})
	file, err = os.CreateTemp(base, "bootstrap-")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	lock := &bootstrapLock{file: file}
	reported := make(chan error, 1)

	owner := retainBootstrapLock(&lock, []*Pending{transport.NewCompletedPending(nil, CommandResult{})}, func(releaseErr error) {
		reported <- releaseErr
	})
	if owner == nil || lock != nil {
		t.Fatal("retained bootstrap lock ownership was not transferred")
	}
	awaitBootstrapCleanup(t, owner)
	if !errors.Is(owner.Err(), os.ErrClosed) || owner.CleanupPending() || !errors.Is(owner.Close(), os.ErrClosed) {
		t.Fatalf("failed native close lost its release fault or retained an unusable handle: %v", owner.Err())
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
	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, Environment: append(os.Environ(), "PRIVATE_INVALID_STATUS=1")}
	if _, _, recoverErr := RecoverSaved(context.Background(), fixture.stateRoot, saved, options); recoverErr == nil {
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
		PrivateGenerationGuard: allowPrivateGenerationForTest{},
		DaemonStartGuard:       allowDaemonStartForTest{},
		ObservationTimeout:     time.Second,
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
	_, _, recoverErr := RecoverSaved(context.Background(), fixture.stateRoot, saved, options)
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
		PrivateGenerationGuard: allowPrivateGenerationForTest{},
		DaemonStartGuard:       allowDaemonStartForTest{},
		ObservationTimeout:     DefaultObservationTimeout,
		Environment:            fake.environment,
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
		PrivateGenerationGuard: allowPrivateGenerationForTest{},
		DaemonStartGuard:       allowDaemonStartForTest{},
		ObservationTimeout:     DefaultObservationTimeout,
		Environment:            fake.environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.options.ObservationTimeout = 20 * time.Millisecond
	ready, err := client.availableForBootstrap(context.Background())
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
		PrivateGenerationGuard: allowPrivateGenerationForTest{},
		DaemonStartGuard:       allowDaemonStartForTest{},
		ObservationTimeout:     DefaultObservationTimeout,
		Environment:            fake.environment,
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
	statusGate := filepath.Join(filepath.Dir(fake.configPath), "status-release")
	fake.environment = append(
		fake.environment,
		"FAKE_STATUS_GATE="+statusGate,
		"FAKE_SHUTDOWN_MARKER="+shutdownMarker,
	)
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{
		PrivateGenerationGuard: allowPrivateGenerationForTest{},
		DaemonStartGuard:       allowDaemonStartForTest{},
		ObservationTimeout:     DefaultObservationTimeout,
		Environment:            fake.environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	scope := newTestCallScope(t, client.options)
	scope.onRelease(func() {
		if releaseErr := os.WriteFile(statusGate, nil, 0o600); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	ctx, cancel := scope.cancelableContext()
	ready := startTestCall(scope, func() privateTaskObservationResult {
		readyErr := waitReady(ctx, client, time.Second)
		return privateTaskObservationResult{observation: Observation{Pending: pendingFrom(readyErr)}, err: readyErr}
	})
	waitForFakeInvocation(t, fake.logPath, "arg=status\n")
	cancel()
	readinessErr := awaitCall(scope, ready, "canceled readiness probe").err
	var inFlight *InFlightError
	if !errors.As(readinessErr, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("waitReady did not return the active probe: %v", readinessErr)
	}

	shutdownBeforeProbe := make(chan bool, 1)
	client.options.Observer = func(event CommandEvent) {
		if event.Stage == "entry" && slices.Contains(event.Argv, "shutdown") {
			select {
			case <-inFlight.Pending.Done():
				shutdownBeforeProbe <- false
			default:
				shutdownBeforeProbe <- true
			}
		}
	}
	cleanup := startTestCall(scope, func() lockCallResult {
		return lockCallResult{err: stopRecoveredSupervisor(context.Background(), client, 2*time.Second, readinessErr)}
	})
	select {
	case <-cleanup.done:
		t.Fatalf("cleanup returned while its readiness probe was gated: %v", cleanup.result.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(statusGate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	awaitLockCall(scope, cleanup, "recovered supervisor cleanup")
	select {
	case before := <-shutdownBeforeProbe:
		if before {
			t.Fatal("shutdown entered before the exact pending probe completed")
		}
	default:
		t.Fatal("cleanup did not enter shutdown")
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
		PrivateGenerationGuard: allowPrivateGenerationForTest{},
		DaemonStartGuard:       allowDaemonStartForTest{},
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
