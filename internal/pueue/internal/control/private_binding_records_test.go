package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: 5 * time.Second}
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

func TestStartDaemonRejectsNonExecutableBeforeStart(t *testing.T) {
	base := canonicalTemp(t)
	daemonPath := filepath.Join(base, "pueued")
	if err := os.WriteFile(daemonPath, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	guard := daemonStartGuardFunc(func(context.Context, string, func() error) error { called = true; return nil })
	if err := startDaemon(t.Context(), base, daemonPath, filepath.Join(base, "pueue.yml"), base, Options{DaemonStartGuard: guard}); !errors.Is(err, ErrBinding) {
		t.Fatalf("non-executable daemon accepted: %v", err)
	}
	if called {
		t.Fatal("invalid metadata reached native start authority")
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

	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: 5 * time.Second}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	configPath := first.Binding().ConfigPath
	if err = os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	recovery, err := AttemptPrivateRecovery(context.Background(), stateRoot, first.Binding(), clientPath, daemonPath, options)
	assertPrivateRecoveryHasNoResult(t, recovery)
	if !errors.Is(err, ErrBinding) {
		t.Fatalf("recovery accepted a deleted saved config: result=%+v err=%v", recovery, err)
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

	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout}
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

func seedPrivateDaemonIdentity(t *testing.T, base, clientPath, daemonPath string) {
	t.Helper()
	resolution, err := resolutionContext(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	configPath, _, err := ensurePrivateConfigContext(t.Context(), filepath.Dir(base), resolution, allowPrivateGenerationForTest{}, "")
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{runtimeBinding: task.SupervisorRef{ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, ClientExecutable: canonicalPrivateTestExecutable(t, clientPath), ConfigPath: configPath, ObservedVersion: FixtureVersion}, options: Options{Resolution: &resolution}}
	binding, err := client.readBinding()
	if err != nil {
		t.Fatal(err)
	}
	binding.DaemonExecutable = canonicalPrivateTestExecutable(t, daemonPath)
	if err = publishPrivateDaemonIdentity(base, binding); err != nil {
		t.Fatal(err)
	}
	if err = clearPrivateBootstrapPending(filepath.Dir(base)); err != nil {
		t.Fatal(err)
	}
}

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

// privateStatusFixture owns release and command/bootstrap completion before its
// earlier-registered directory cleanup may remove the fixture.
type privateStatusFixture struct {
	base            string
	releasePath     string
	retained        bool
	clients         []*Client
	resultErr       error
	lateDiagnostics error
	daemonMu        sync.Mutex
	daemonWaits     []<-chan struct{}
}

func newPrivateStatusFixture(t *testing.T) *privateStatusFixture {
	t.Helper()
	fixture := &privateStatusFixture{retained: true}
	fixture.base = canonicalTempWithRetention(t, &fixture.retained)
	fixture.releasePath = filepath.Join(fixture.base, "status-release")
	t.Cleanup(func() {
		if err := fixture.finish(time.Now().Add(10 * time.Second)); err != nil {
			t.Errorf("retained private status fixture %s: %v", fixture.base, err)
		}
		if fixture.lateDiagnostics != nil {
			t.Logf("released private status command diagnostics: %v", fixture.lateDiagnostics)
		}
	})
	return fixture
}

func (fixture *privateStatusFixture) capture(client *Client, err error) {
	if client != nil && !slices.Contains(fixture.clients, client) {
		fixture.clients = append(fixture.clients, client)
	}
	fixture.resultErr = errors.Join(fixture.resultErr, err)
}

// bind transfers client and daemon cleanup to this fixture. It deliberately
// registers no later client cleanup that could run before marker release.
func (fixture *privateStatusFixture) bind(t *testing.T, ctx context.Context, options Options) (privateSupervisorTestFixture, *Client) {
	t.Helper()
	supervisor := newPrivateSupervisorTestFixtureAtBase(t, fixture.base)
	options.PrivateGenerationGuard = allowPrivateGenerationForTest{}
	options.DaemonStartGuard = allowDaemonStartForTest{}
	options.PrivateTerminalEventRunner = privateInstalledCallbackForTest(t, supervisor.stateRoot)
	options.DaemonWaitOwner = fixture
	client, err := BindPrivate(ctx, supervisor.clientPath, supervisor.daemonPath, supervisor.stateRoot, options)
	fixture.capture(client, err)
	if err != nil {
		t.Fatalf("bind supervisor fixture before lease fault: %v", err)
	}
	return supervisor, client
}

func (fixture *privateStatusFixture) RegisterDaemonWait(_ string, _ int, done <-chan struct{}) error {
	fixture.daemonMu.Lock()
	defer fixture.daemonMu.Unlock()
	fixture.daemonWaits = append(fixture.daemonWaits, done)
	return nil
}

func (fixture *privateStatusFixture) finish(deadline time.Time) error {
	if !fixture.retained {
		return nil
	}
	releaseErr := os.WriteFile(fixture.releasePath, nil, 0o600)
	if err := fixture.joinCommands(deadline); err != nil {
		return errors.Join(releaseErr, err)
	}
	releaseErr = errors.Join(releaseErr, fixture.joinBootstrap(deadline))
	for _, client := range fixture.clients {
		releaseErr = errors.Join(releaseErr, client.Close())
	}
	// Client closure prevents further work before taking the registered Wait
	// snapshot. Wait registration is synchronous with every daemon start.
	fixture.daemonMu.Lock()
	waits := slices.Clone(fixture.daemonWaits)
	fixture.daemonMu.Unlock()
	for _, done := range waits {
		if !waitUntil(done, deadline) {
			releaseErr = errors.Join(releaseErr, fmt.Errorf("private fixture daemon did not join: %w", context.DeadlineExceeded))
		}
	}
	if releaseErr != nil {
		return releaseErr
	}
	fixture.retained = false
	return nil
}

func (fixture *privateStatusFixture) joinCommands(deadline time.Time) error {
	results := newBootstrapPendingResults(pendingHandles(fixture.resultErr))
	for {
		results.collectSettled()
		fixture.lateDiagnostics = results.err
		if len(results.pending) == 0 {
			break
		}
		if !waitUntil(results.pending[0].Done(), deadline) {
			return fmt.Errorf("private status command did not join: %w", context.DeadlineExceeded)
		}
	}
	return nil
}

func (fixture *privateStatusFixture) joinBootstrap(deadline time.Time) error {
	var releaseErr error
	var owner *BootstrapCleanupError
	if errors.As(fixture.resultErr, &owner) {
		if !waitUntil(owner.Done(), deadline) {
			return fmt.Errorf("bootstrap cleanup did not join: %w", context.DeadlineExceeded)
		}
		if owner.CleanupPending() {
			releaseErr = errors.Join(releaseErr, errors.New("bootstrap cleanup still owns its native lock"))
		}
		// Command failures are diagnostics after positive join. Only the
		// bootstrap owner's release fault prevents native closure proof.
		if cause := errors.Unwrap(owner); !errors.Is(cause, ErrInFlight) {
			releaseErr = errors.Join(releaseErr, fmt.Errorf("bootstrap release failed: %w", cause))
		}
	}
	return releaseErr
}

// The call scope joins the synchronous caller; the fixture's Client.Close owns
// the maintenance command that may remain pending after the boolean return.
type privateMaintenanceCallResult struct{ available bool }

func (privateMaintenanceCallResult) pendingCommand() *Pending { return nil }
