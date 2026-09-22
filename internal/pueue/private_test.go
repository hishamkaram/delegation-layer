package pueue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestWaitReadyRejectsParentCancellationAfterReaping(t *testing.T) {
	fake := newFakeSupervisor(t, "delay-status")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	if err := waitReady(ctx, fake.client, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitReady accepted readiness after caller cancellation: %v", err)
	}
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
		Environment: append(os.Environ(),
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

func TestBindPrivateRejectsReadyDaemonIdentityDrift(t *testing.T) {
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
	if _, err = BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{ObservationTimeout: time.Second}); !errors.Is(err, ErrBinding) {
		t.Fatalf("ready daemon identity drift was accepted: %v", err)
	}
	if _, err = os.Stat(filepath.Join(privateBase, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started after ready identity drift: %v", err)
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
	if got := first.Binding().ObservedVersion; got != "pueue 99.7.3" {
		t.Fatalf("runtime version was not preserved: %q", got)
	}
	if first.Binding().DaemonExecutable != daemonPath || first.Binding().DaemonSHA256 == "" {
		t.Fatalf("private daemon identity was not persisted: %+v", first.Binding())
	}
	configPath := filepath.Join(stateRoot, privateSupervisorDirectory, "pueue.yml")
	if _, statErr := os.Stat(configPath); statErr != nil {
		t.Fatalf("private config was not created: %v", statErr)
	}
	data, readErr := os.ReadFile(configPath)
	if readErr != nil || !strings.Contains(string(data), filepath.Join(stateRoot, privateSupervisorDirectory, "data")) {
		t.Fatalf("private config is not rooted in state: data=%q err=%v", data, readErr)
	}

	second, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if second.Binding().ConfigPath != first.Binding().ConfigPath || second.Binding().Endpoint != first.Binding().Endpoint {
		t.Fatalf("private binding changed across reuse: first=%+v second=%+v", first.Binding(), second.Binding())
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 1 {
		t.Fatalf("private daemon was started %d times, want one: %q", got, log)
	}
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
	recovered, err := RecoverPrivate(context.Background(), stateRoot, saved, options)
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

	options := Options{
		ObservationTimeout: 20 * time.Millisecond,
		Environment:        append(os.Environ(), "PRIVATE_STATUS_DELAY=0.5"),
	}
	started := time.Now()
	_, err = RecoverPrivate(context.Background(), stateRoot, first.Binding(), options)
	if err == nil {
		t.Fatal("hung private bootstrap unexpectedly recovered")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("private recovery was not bounded: %s", elapsed)
	}
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("private recovery lost its pending ownership: %v", err)
	}

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
	if !awaitPendingNaturally(inFlight.Pending, time.Second) {
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

func TestRecoverPrivateRejectsDaemonIdentityDriftBeforeRestart(t *testing.T) {
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
	if _, err = RecoverPrivate(context.Background(), stateRoot, first.Binding(), options); !errors.Is(err, ErrBinding) {
		t.Fatalf("recovery accepted daemon identity drift: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 1 {
		t.Fatalf("daemon restarted after identity drift %d times, want 1: %q", got, log)
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
	if _, err = RecoverPrivate(context.Background(), stateRoot, first.Binding(), options); !errors.Is(err, ErrBinding) {
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
      marker="$(dirname "$config")/ready"
      [ -f "$marker" ] || exit 1
      if [ "${PRIVATE_INVALID_STATUS:-}" = "1" ]; then
        printf '%s\n' '{"tasks":{}}'
        exit 0
      fi
      printf '%s\n' '{"tasks":{},"groups":{}}'
      exit 0
      ;;
    *) exit 64 ;;
  esac
done
exit 64
`

const privateDaemonFixture = `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c) config=$2; shift 2 ;;
    *) shift ;;
  esac
done
[ -f "$(dirname "$config")/daemon.identity.json" ]
printf '%s\n' started >> "$(dirname "$config")/daemon-started"
touch "$(dirname "$config")/ready"
`
