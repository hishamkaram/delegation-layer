package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

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

func TestEnsurePrivateSupervisorBindingAdoptsLiveDaemonWithoutReplacingIdentity(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	binding := client.Binding()
	identityPath := privateDaemonIdentityPath(filepath.Dir(binding.ConfigPath))
	writePrivateSuccessorTestRecord(t, identityPath, privateDaemonIdentity{Executable: binding.DaemonExecutable, SHA256: binding.DaemonSHA256})
	if err := os.Remove(privateSupervisorBindingPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	identityBefore := readPrivateSupervisorFile(t, identityPath)
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		if startErr := client.StartPrivate(ctx, fixture.stateRoot); startErr != nil {
			return startErr
		}
		if _, found, readErr := loadPrivateSupervisorBinding(fixture.stateRoot); readErr != nil {
			return readErr
		} else if found {
			return errors.New("legacy ready branch unexpectedly published a binding")
		}
		return client.EnsurePrivateSupervisorBindingWithLock(ctx, fixture.stateRoot)
	})
	if err != nil {
		t.Fatalf("adopt live daemon: %v", err)
	}
	published, found, err := loadPrivateSupervisorBinding(fixture.stateRoot)
	if err != nil || !found || published != binding {
		t.Fatalf("live owner not published exactly: found=%t binding=%+v err=%v", found, published, err)
	}
	if complete, checkErr := PrivateSupervisorMigrationComplete(fixture.stateRoot, binding); checkErr != nil || !complete {
		t.Fatalf("adopted owner incomplete: complete=%t err=%v", complete, checkErr)
	}
	if got := readPrivateSupervisorFile(t, identityPath); !bytes.Equal(got, identityBefore) {
		t.Fatal("adoption replaced daemon identity")
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestEnsurePrivateSupervisorBindingRejectsUnprovenOwnerBeforePublication(t *testing.T) {
	for _, fault := range []string{"provenance", "logical-config", "missing-identity", "malformed-identity", "unavailable"} {
		t.Run(fault, func(t *testing.T) {
			fixture := newPrivateSupervisorTestFixture(t)
			client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Error(err)
				}
			})
			binding := client.Binding()
			identityPath := privateDaemonIdentityPath(filepath.Dir(binding.ConfigPath))
			injectPrivateBindingAdoptionFault(t, binding, fault)
			if err := os.Remove(privateSupervisorBindingPath(fixture.stateRoot)); err != nil {
				t.Fatal(err)
			}
			before := snapshotPrivateTestFiles(t, []string{identityPath, binding.ConfigPath})
			err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
				return client.EnsurePrivateSupervisorBindingWithLock(ctx, fixture.stateRoot)
			})
			if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
				t.Fatalf("unproven owner accepted or misclassified: %v", err)
			}
			if _, found, readErr := loadPrivateSupervisorBinding(fixture.stateRoot); readErr != nil || found {
				t.Fatalf("failed adoption published identity: found=%t err=%v", found, readErr)
			}
			identityBefore, identityPresent := before[identityPath]
			assertPrivateTestFileUnchanged(t, identityPath, identityBefore, identityPresent)
			assertPrivateTestFileUnchanged(t, binding.ConfigPath, before[binding.ConfigPath], true)
			assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
		})
	}
}

func injectPrivateBindingAdoptionFault(t *testing.T, binding task.SupervisorRef, fault string) {
	t.Helper()
	base := filepath.Dir(binding.ConfigPath)
	identityPath := privateDaemonIdentityPath(base)
	identity := privateDaemonIdentityForBinding(binding)
	switch fault {
	case "provenance":
		identity.SHA256 = strings.Repeat("f", 64)
		writePrivateSuccessorTestRecord(t, identityPath, identity)
	case "logical-config":
		identity.ConfigDigest = strings.Repeat("f", 64)
		writePrivateSuccessorTestRecord(t, identityPath, identity)
	case "missing-identity":
		if err := os.Remove(identityPath); err != nil {
			t.Fatal(err)
		}
	case "malformed-identity":
		if err := os.WriteFile(identityPath, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "unavailable":
		if err := os.Remove(filepath.Join(base, "ready")); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown adoption fault %q", fault)
	}
}

func TestEnsurePrivateSupervisorBindingPreservesPublishedGeneration(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	published := client.Binding()
	published.ClientSHA256 = strings.Repeat("f", 64)
	path := privateSupervisorBindingPath(fixture.stateRoot)
	writePrivateSuccessorTestRecord(t, path, privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: published})
	before := readPrivateSupervisorFile(t, path)
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		return client.EnsurePrivateSupervisorBindingWithLock(ctx, fixture.stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("different published generation accepted: %v", err)
	}
	if got := readPrivateSupervisorFile(t, path); !bytes.Equal(got, before) {
		t.Fatal("existing generation replaced")
	}
}

func TestEnsurePrivateSupervisorBindingRejectsIncompleteLifecycle(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := markPrivateBootstrapPending(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		return client.EnsurePrivateSupervisorBindingWithLock(ctx, fixture.stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("incomplete lifecycle accepted: %v", err)
	}
	if pending, readErr := PrivateBootstrapPending(fixture.stateRoot); readErr != nil || !pending {
		t.Fatalf("observation cleared pending lifecycle: pending=%t err=%v", pending, readErr)
	}
}

func TestEnsurePrivateSupervisorBindingPreservesCanceledContext(t *testing.T) {
	var client *Client
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := client.EnsurePrivateSupervisorBindingWithLock(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved before IO: %v", err)
	}
	var missing context.Context
	if err := client.EnsurePrivateSupervisorBindingWithLock(missing, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil context not rejected before IO: %v", err)
	}
}

func TestEnsurePrivateSupervisorBindingRejectsCancellationAfterReady(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	bindingPath := privateSupervisorBindingPath(fixture.stateRoot)
	if err := os.Remove(bindingPath); err != nil {
		t.Fatal(err)
	}
	identityPath := privateDaemonIdentityPath(filepath.Dir(bindingPath))
	identityBefore := readPrivateSupervisorFile(t, identityPath)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	readyCompleted := false
	err := WithBootstrapLockContext(ctx, fixture.stateRoot, func(lockedContext context.Context) error {
		return client.ensurePrivateSupervisorBindingWithReady(lockedContext, fixture.stateRoot, func(probeContext context.Context) error {
			if readyErr := client.Ready(probeContext); readyErr != nil {
				return readyErr
			}
			readyCompleted = true
			cancel()
			return nil
		})
	})
	if !readyCompleted {
		t.Fatalf("fixture never completed its real readiness check: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("adoption after completed readiness lost cancellation: %v", err)
	}
	assertPrivateTestFileUnchanged(t, bindingPath, nil, false)
	assertPrivateTestFileUnchanged(t, identityPath, identityBefore, true)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestPublishPrivateSupervisorBindingHonorsContextBeforeStaging(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	bindingPath := privateSupervisorBindingPath(fixture.stateRoot)
	if err := os.Remove(bindingPath); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, release := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer release()
	for _, test := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "canceled", ctx: canceled, want: context.Canceled},
		{name: "expired", ctx: expired, want: context.DeadlineExceeded},
		{name: "missing", want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := PublishPrivateSupervisorBindingIfAbsentContext(test.ctx, fixture.stateRoot, client.Binding()); !errors.Is(err, test.want) {
				t.Fatalf("publication error=%v, want %v", err, test.want)
			}
			assertPrivateTestFileUnchanged(t, bindingPath, nil, false)
			entries, err := os.ReadDir(filepath.Dir(bindingPath))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), privateRecordStagePrefix) {
					t.Fatalf("canceled publication staged a private record: %s", entry.Name())
				}
			}
		})
	}
}

func TestRestorePrivateSupervisorBindingHonorsCanceledContext(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	binding := client.Binding()
	bindingPath := privateSupervisorBindingPath(fixture.stateRoot)
	if err := os.Remove(bindingPath); err != nil {
		t.Fatal(err)
	}
	identityPath := privateDaemonIdentityPath(filepath.Dir(bindingPath))
	identityBefore := readPrivateSupervisorFile(t, identityPath)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := restoreExpectedPrivateSupervisorBinding(ctx, fixture.stateRoot, &binding); !errors.Is(err, context.Canceled) {
		t.Errorf("restoration lost cancellation before publication: %v", err)
	}
	assertPrivateTestFileUnchanged(t, bindingPath, nil, false)
	assertPrivateTestFileUnchanged(t, identityPath, identityBefore, true)
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
	args := inFlight.Pending.Args()
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
