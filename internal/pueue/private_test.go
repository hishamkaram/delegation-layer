package pueue

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestReadyRequiresTheBoundedQueueSchema(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	if err := os.WriteFile(fake.statusPath, []byte(`{"tasks":{},"groups":{},"future":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fake.client.Ready(context.Background()); !errors.Is(err, ErrBinding) {
		t.Fatalf("malformed supervisor status was accepted: %v", err)
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
}

func TestWaitReadyRejectsParentCancellationAndRetainsPendingOwner(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "delay-status", "pueue "+FixtureVersion)
	fake.environment = append(fake.environment, "FAKE_STATUS_DELAY=1")
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
		if !awaitPendingNaturally(inFlight.Pending, 3*time.Second) {
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
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	marker := filepath.Join(base, "version-marker")
	options := Options{
		ObservationTimeout: 100 * time.Millisecond,
		Environment: append(
			os.Environ(),
			"PRIVATE_VERSION_DELAY=0.5",
			"PRIVATE_VERSION_MARKER="+marker,
		),
	}
	started := time.Now()
	_, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err == nil {
		t.Fatal("timed out version probe unexpectedly bootstrapped the supervisor")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timed out version probe was not bounded: %s", elapsed)
	}
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("timed out version probe lost its pending ownership: %v", err)
	}
	if !awaitPendingNaturally(inFlight.Pending, 2*time.Second) {
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
		ObservationTimeout: 100 * time.Millisecond,
		Environment:        append(os.Environ(), "PRIVATE_STATUS_DELAY=0.25"),
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

func TestBindPrivateBoundsSlowReadinessBeforeReturning(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
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
		ObservationTimeout: 100 * time.Millisecond,
		Environment:        append(os.Environ(), "PRIVATE_STATUS_DELAY=2"),
	}
	started := time.Now()
	client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err == nil || client != nil {
		t.Fatalf("slow readiness unexpectedly completed: client_present=%t err=%v", client != nil, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("slow readiness was not bounded: %s", elapsed)
	}
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("slow readiness lost its pending ownership: %v", err)
	}
	if !awaitPendingNaturally(inFlight.Pending, 3*time.Second) {
		t.Fatal("slow readiness remained in flight after the bounded return")
	}
	if _, err := os.Stat(filepath.Join(privateBase, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started despite a successful slow readiness result: %v", err)
	}
}

func TestBindPrivateDoesNotRestartOnMalformedReadyResponse(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
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
		ObservationTimeout: time.Second,
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
	if _, err = BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{ObservationTimeout: time.Second}); err != nil {
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

	second := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if second.Binding().ConfigPath != first.Binding().ConfigPath || second.Binding().Endpoint != first.Binding().Endpoint {
		t.Fatalf("private binding changed across reuse: first=%+v second=%+v", first.Binding(), second.Binding())
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestRecoverPrivateRestartsStoppedSupervisor(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: time.Second}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	saved := first.Binding()
	saved.ResolutionCwd = filepath.Join(base, "removed-before-recovery")
	recovered, err := RecoverPrivate(context.Background(), stateRoot, saved, clientPath, daemonPath, options)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Binding() != saved {
		t.Fatalf("recovery changed the saved supervisor binding: saved=%+v recovered=%+v", saved, recovered.Binding())
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 2 {
		t.Fatalf("private daemon was restarted %d times, want 2: %q", got, log)
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
	identityPath := privateDaemonIdentityPath(filepath.Join(stateRoot, privateSupervisorDirectory))
	oldIdentity := readPrivateSupervisorFile(t, identityPath)
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
	identityAfterAdoption := readPrivateSupervisorFile(t, identityPath)
	if !bytes.Equal(identityAfterAdoption, oldIdentity) {
		t.Fatal("recovery rewrote launch provenance while adopting the existing endpoint")
	}

	removePrivateTestPath(t, filepath.Join(stateRoot, privateSupervisorDirectory, "ready"))
	restarted := recoverPrivateForTest(t, stateRoot, first.Binding(), currentClientPath, currentDaemonPath)
	if restarted.Binding() != first.Binding() {
		t.Fatalf("restart changed the durable task supervisor identity: first=%+v restarted=%+v", first.Binding(), restarted.Binding())
	}
	if restarted.runtimeBinding.ClientExecutable != currentClientPath || restarted.runtimeBinding.DaemonExecutable != currentDaemonPath {
		t.Fatalf("restarted client did not use the current executable pair: %+v", restarted.runtimeBinding)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, currentDaemonPath)
	assertPrivateDaemonStartCount(t, stateRoot, 2)
}

func TestRecoveredPrivateClientCanStopTaskWithOriginalSupervisorReference(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldClientPath, oldDaemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "old"))
	currentClientPath, currentDaemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "current"))
	privateRoot := filepath.Join(stateRoot, privateSupervisorDirectory)
	statusPath := filepath.Join(privateRoot, "test-status.json")
	killMarker := filepath.Join(privateRoot, "test-kill-task-id")
	options := Options{ObservationTimeout: time.Second}
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
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{ObservationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}

	startedMarker := filepath.Join(base, "status-started")
	releaseMarker := filepath.Join(base, "status-release")
	options := Options{
		// Leave enough time for RecoverPrivate's version preflight to finish
		// before the deliberately blocked status command consumes the budget.
		ObservationTimeout: 250 * time.Millisecond,
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
	waitForPrivateStatusStart(t, startedMarker)

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
	if !awaitPendingNaturally(inFlight.Pending, 3*time.Second) {
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
	removePrivateTestPath(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready"))
	appendPrivateSupervisorFile(t, fixture.daemonPath, "\n")
	recoverPrivateForTest(t, fixture.stateRoot, first.Binding(), fixture.clientPath, fixture.daemonPath)

	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	identity := readPrivateDaemonIdentity(t, fixture.stateRoot)
	want := privateDaemonIdentity{
		Executable: canonicalPrivateTestExecutable(t, fixture.daemonPath),
		SHA256:     hashPrivateTestExecutable(t, fixture.daemonPath),
	}
	if identity != want {
		t.Fatalf("stopped endpoint did not publish new launch provenance: got=%+v want=%+v", identity, want)
	}
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
	makePrivateTestDirectory(t, stateRoot)
	return base, stateRoot
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
	client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{ObservationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func recoverPrivateForTest(t *testing.T, stateRoot string, saved task.SupervisorRef, clientPath, daemonPath string) *Client {
	t.Helper()
	client, err := RecoverPrivate(context.Background(), stateRoot, saved, clientPath, daemonPath, Options{ObservationTimeout: time.Second})
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
	want := privateDaemonIdentity{
		Executable: canonicalPrivateTestExecutable(t, daemonPath),
		SHA256:     hashPrivateTestExecutable(t, daemonPath),
	}
	if got := readPrivateDaemonIdentity(t, stateRoot); got != want {
		t.Fatalf("private daemon identity does not match launched pair: got=%+v want=%+v", got, want)
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
	data := statusTestPayload(t, map[string]any{strconv.FormatInt(id, 10): job}, map[string]any{})
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

func TestBindPrivateAdoptsReadySupervisorAfterProvenancePublicationFailure(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(privateDaemonIdentityPath(privateBase), 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	options := Options{ObservationTimeout: time.Second}
	if _, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options); err == nil {
		t.Fatal("bootstrap succeeded despite failing to publish launch provenance")
	}
	if _, err := os.Stat(filepath.Join(privateBase, "ready")); err != nil {
		t.Fatalf("test daemon did not reach readiness before provenance failure: %v", err)
	}
	if err := os.Remove(privateDaemonIdentityPath(privateBase)); err != nil {
		t.Fatal(err)
	}
	if _, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options); err != nil {
		t.Fatalf("next invocation did not adopt the ready endpoint after publication failure: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(privateBase, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 1 {
		t.Fatalf("daemon was relaunched after provenance publication failure %d times: %q", got, log)
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
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: time.Second}
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
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: time.Second}
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
  case "$1" in
    -c) config=$2; shift 2 ;;
    --version)
      if [ "${PRIVATE_VERSION_MARKER:-}" != "" ]; then printf '%s\n' started >> "$PRIVATE_VERSION_MARKER"; fi
      if [ "${PRIVATE_VERSION_DELAY:-}" != "" ]; then sleep "$PRIVATE_VERSION_DELAY"; elif [ "${PRIVATE_DELAY_VERSION:-}" = "1" ]; then sleep 0.08; fi
      if [ "${PRIVATE_VERSION_MARKER:-}" != "" ]; then printf '%s\n' completed >> "$PRIVATE_VERSION_MARKER"; fi
      printf '%s\n' 'pueue 99.7.3'; exit 0 ;;
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
      printf '%s\n' '{"tasks":{},"groups":{}}'
      exit 0
      ;;
    kill|remove)
      if [ -f "$(dirname "$config")/test-record-kill" ]; then printf '%s\n' "$2" >> "$(dirname "$config")/test-kill-task-id"; fi
      exit 0 ;;
    *) exit 64 ;;
  esac
done
exit 64
`

func waitForPrivateStatusStart(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
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
touch "$(dirname "$config")/ready"
`
