package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInstallStateRunnerKeepsExecutableVersionsImmutable(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, source, "old runner")

	oldRunner := installStateRunnerFixture(t, root, source)
	if !IsStateRunnerPath(root, oldRunner) || oldRunner == filepath.Join(root, ".supervisor", stateRunnerName) {
		t.Fatalf("state runner path = %q, want an immutable content-addressed path", oldRunner)
	}
	supervisorfixture.RequireExecutable(t, oldRunner, "old runner")

	supervisorfixture.WriteExecutable(t, source, "upgraded runner")
	newRunner := installStateRunnerFixture(t, root, source)
	if newRunner == oldRunner {
		t.Fatal("different runner bytes reused an immutable path")
	}
	supervisorfixture.RequireExecutable(t, oldRunner, "old runner")
	supervisorfixture.RequireExecutable(t, newRunner, "upgraded runner")
	info, err := os.Stat(newRunner)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("state runner permissions = %o, want 700", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(newRunner))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("immutable runner installation left unexpected files: %+v", entries)
	}
	if !managedRunnerExecutable(root, oldRunner, task.RunnerOwnershipManaged) {
		t.Fatal("old content-addressed state runner was not recognized as managed")
	}
	if !managedRunnerExecutable(root, newRunner, task.RunnerOwnershipManaged) {
		t.Fatal("new content-addressed state runner was not recognized as managed")
	}
}

func TestInstallStateTerminalEventRunnerAtomicallyRefreshesCompatibleHelper(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, source, "old callback helper")
	destination, err := prepareStateTerminalEventRunnerRevision(context.Background(), root, source, 1)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := opened.Close(); closeErr != nil {
			t.Errorf("close prior callback helper: %v", closeErr)
		}
	}()

	supervisorfixture.WriteExecutable(t, source, "current callback helper")
	refreshed, err := prepareStateTerminalEventRunnerRevision(context.Background(), root, source, 2)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != destination || refreshed != pueue.PrivateTerminalEventRunnerPath(root) {
		t.Fatalf("terminal callback path changed across compatible refresh: before=%q after=%q", destination, refreshed)
	}
	supervisorfixture.RequireExecutable(t, refreshed, "current callback helper")
	prior, err := io.ReadAll(opened)
	if err != nil || string(prior) != "old callback helper" {
		t.Fatalf("running callback inode changed during refresh: content=%q err=%v", prior, err)
	}
	info, err := os.Stat(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("refreshed callback mode=%v, want 0700", info.Mode().Perm())
	}
}

func TestPrepareStateTerminalEventRunnerRetainsEqualRevisionBuild(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, source, "published callback helper")
	published := filepath.Join(supervisorfixture.CanonicalTempDir(t), "published")
	supervisorfixture.WriteExecutable(t, published, "published callback helper")
	wantDigest, err := stateExecutableSourceDigest(published)
	if err != nil {
		t.Fatal(err)
	}
	if _, prepareErr := prepareStateTerminalEventRunnerRevision(context.Background(), root, source, 7); prepareErr != nil {
		t.Fatal(prepareErr)
	}

	supervisorfixture.WriteExecutable(t, source, "different build of same callback helper")
	destination, err := prepareStateTerminalEventRunnerRevision(context.Background(), root, source, 7)
	if err != nil {
		t.Fatal(err)
	}
	supervisorfixture.RequireExecutable(t, destination, "published callback helper")

	record, found, err := readStateTerminalEventRunnerRecord(filepath.Join(root, ".supervisor"))
	if err != nil || !found {
		t.Fatalf("callback publication record found=%t err=%v", found, err)
	}
	if record.ImplementationRevision != 7 {
		t.Fatalf("callback publication revision=%d, want 7", record.ImplementationRevision)
	}
	if record.BuildSHA256 != wantDigest {
		t.Fatalf("callback publication digest=%q, want %q (err=%v)", record.BuildSHA256, wantDigest, err)
	}
}

func TestPrepareStateTerminalEventRunnerConcurrentRevisionsDoNotRollback(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	oldSource := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run-old")
	newSource := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run-new")
	supervisorfixture.WriteExecutable(t, oldSource, "old callback helper")
	supervisorfixture.WriteExecutable(t, newSource, "new callback helper")

	start := make(chan struct{})
	errorsFound := make(chan error, 2)
	var writers sync.WaitGroup
	writers.Add(2)
	go func() {
		defer writers.Done()
		<-start
		_, err := prepareStateTerminalEventRunnerRevision(context.Background(), root, oldSource, 1)
		errorsFound <- err
	}()
	go func() {
		defer writers.Done()
		<-start
		_, err := prepareStateTerminalEventRunnerRevision(context.Background(), root, newSource, 2)
		errorsFound <- err
	}()
	close(start)
	writers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}

	destination := pueue.PrivateTerminalEventRunnerPath(root)
	supervisorfixture.RequireExecutable(t, destination, "new callback helper")
	record, found, err := readStateTerminalEventRunnerRecord(filepath.Join(root, ".supervisor"))
	if err != nil || !found {
		t.Fatalf("callback publication record found=%t err=%v", found, err)
	}
	if record.ImplementationRevision != 2 {
		t.Fatalf("callback publication revision=%d, want 2", record.ImplementationRevision)
	}
}

func TestPrepareStateTerminalEventRunnerReadyPathDoesNotWaitForTransition(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, source, "callback helper")
	destination, err := prepareStateTerminalEventRunnerRevision(context.Background(), root, source, 1)
	if err != nil {
		t.Fatal(err)
	}
	releaseTransition := holdPrivateTransitionForRunnerTest(t, root)
	assertRunnerReadyCallersCompleteDuringTransition(t, root, source, destination, releaseTransition)
}

func holdPrivateTransitionForRunnerTest(t *testing.T, root string) func() error {
	t.Helper()
	transitionHeld := make(chan struct{})
	releaseTransition := make(chan struct{})
	transitionDone := make(chan error, 1)
	go func() {
		acquired, transitionErr := pueue.TryWithPrivateTransition(context.Background(), root, func() error {
			close(transitionHeld)
			<-releaseTransition
			return nil
		})
		if transitionErr == nil && !acquired {
			transitionErr = errors.New("transition owner was not acquired")
		}
		transitionDone <- transitionErr
	}()
	<-transitionHeld
	var releaseOnce sync.Once
	var transitionErr error
	return func() error {
		releaseOnce.Do(func() {
			close(releaseTransition)
			transitionErr = <-transitionDone
		})
		return transitionErr
	}
}

func assertRunnerReadyCallersCompleteDuringTransition(
	t *testing.T,
	root, source, destination string,
	releaseTransition func() error,
) {
	t.Helper()
	probeContext, cancelProbe := context.WithCancel(t.Context())
	// This test owns transition ordering. Directory-sync behavior has dedicated
	// coverage below and must not turn the deadlock watchdog into a disk-latency
	// assertion when every ready-path caller reaches the same durable directory.
	syncDirectory := func(string) error { return nil }
	var probes sync.WaitGroup
	var cleanupOnce sync.Once
	cleanupProbes := func() {
		cleanupOnce.Do(func() {
			cancelProbe()
			if err := releaseTransition(); err != nil {
				t.Errorf("release transition owner: %v", err)
			}
			probes.Wait()
		})
	}
	t.Cleanup(func() {
		cleanupProbes()
	})

	type probeResult struct {
		path string
		err  error
	}
	const callers = 16
	results := make(chan probeResult, callers)
	probes.Add(callers)
	for range callers {
		go func() {
			defer probes.Done()
			ready, probeErr := prepareStateTerminalEventRunnerRevisionWithSync(
				probeContext, root, source, 1, syncDirectory,
			)
			results <- probeResult{path: ready, err: probeErr}
		}()
	}
	// Completion while the transition remains held is the assertion. This
	// timer is only a deadlock diagnostic; it never supplies the call context or
	// turns elapsed time into a successful result.
	watchdog := time.NewTimer(30 * time.Second)
	defer watchdog.Stop()
	var firstFailure string
	for range callers {
		select {
		case result := <-results:
			if result.err != nil && firstFailure == "" {
				firstFailure = fmt.Sprintf("prepared helper waited for unrelated transition: %v", result.err)
			}
			if result.err == nil && result.path != destination && firstFailure == "" {
				firstFailure = fmt.Sprintf("prepared helper=%q, want %q", result.path, destination)
			}
		case <-watchdog.C:
			cleanupProbes()
			t.Fatal("prepared helper did not finish while unrelated transition remained held")
		}
	}
	cleanupProbes()
	if firstFailure != "" {
		t.Fatal(firstFailure)
	}
}

func TestPrepareStateTerminalEventRunnerRecoversDesiredRecordBeforeRename(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	oldSource := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run-old")
	newSource := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run-new")
	supervisorfixture.WriteExecutable(t, oldSource, "old callback helper")
	supervisorfixture.WriteExecutable(t, newSource, "new callback helper")
	if _, err := prepareStateTerminalEventRunnerRevision(context.Background(), root, oldSource, 1); err != nil {
		t.Fatal(err)
	}
	newDigest, err := stateExecutableSourceDigest(newSource)
	if err != nil {
		t.Fatal(err)
	}
	record := stateTerminalEventRunnerRecord{
		SchemaVersion:          task.SchemaVersion,
		ProtocolPath:           stateTerminalEventRunnerName,
		ImplementationRevision: 2,
		BuildSHA256:            newDigest,
	}
	if err = publishStateTerminalEventRunnerRecord(filepath.Join(root, ".supervisor"), record, syncRunnerDirectory); err != nil {
		t.Fatal(err)
	}
	// Simulate a process crash after the desired record barrier and before the
	// executable rename: the old bytes remain until the matching source retries.
	if _, err = prepareStateTerminalEventRunnerRevision(context.Background(), root, oldSource, 1); !errors.Is(err, errStateTerminalEventRunnerRevisionDeferred) {
		t.Fatalf("older source recovery error=%v, want deferred revision", err)
	}
	supervisorfixture.RequireExecutable(t, pueue.PrivateTerminalEventRunnerPath(root), "old callback helper")
	if _, err = prepareStateTerminalEventRunnerRevision(context.Background(), root, newSource, 2); err != nil {
		t.Fatal(err)
	}
	supervisorfixture.RequireExecutable(t, pueue.PrivateTerminalEventRunnerPath(root), "new callback helper")
}

func TestPrepareStateTerminalEventRunnerRetriesMatchingDirectorySync(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, source, "callback helper")
	if _, err := PrepareStateTerminalEventRunner(context.Background(), root, source); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("directory sync failed once")
	var calls int
	syncDirectory := func(path string) error {
		calls++
		if path != filepath.Join(root, ".supervisor") {
			t.Fatalf("directory sync path=%q", path)
		}
		if calls == 1 {
			return wantErr
		}
		return nil
	}
	if _, err := prepareStateTerminalEventRunnerRevisionWithSync(context.Background(), root, source, stateTerminalEventRunnerRevision, syncDirectory); !errors.Is(err, wantErr) {
		t.Fatalf("first matching publication error=%v, want %v", err, wantErr)
	}
	if _, err := prepareStateTerminalEventRunnerRevisionWithSync(context.Background(), root, source, stateTerminalEventRunnerRevision, syncDirectory); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("matching publication directory sync calls=%d, want 2", calls)
	}
}

func TestUnprotectedStateRunnerIsRejectedByInstallationAndScanners(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, source, "runner")
	runner := installStateRunnerFixture(t, root, source)
	digest, err := stateExecutableSourceDigest(runner)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(runner, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err = stateExecutableMatches(runner, digest); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("state executable matcher accepted mode 0755: %v", err)
	}
	if _, err = InstallStateRunner(root, runner); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("state runner installer accepted its content-addressed source with mode 0755: %v", err)
	}
	if _, err = InstallStateRunner(root, source); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("state runner installation accepted mode 0755: %v", err)
	}
	if CurrentManagedRunnerExecutable(root, runner) {
		t.Fatal("managed runner scanner accepted mode 0755")
	}
	if _, err = managedStateRunnerPaths(root); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("state runner path scan accepted mode 0755: %v", err)
	}
}

func installStateRunnerFixture(t *testing.T, root, source string) string {
	t.Helper()
	runner, err := InstallStateRunner(root, source)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestRemoveStateExecutableStageSyncsAfterUnlink(t *testing.T) {
	base := supervisorfixture.CanonicalTempDir(t)
	stagePath := filepath.Join(base, ".delegate-run-stage")
	if err := os.WriteFile(stagePath, []byte("stage"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("directory barrier failed")
	called := false
	err := removeStateExecutableStage(base, stagePath, func(path string) error {
		called = true
		if path != base {
			t.Fatalf("directory barrier path = %q, want %q", path, base)
		}
		if _, statErr := os.Lstat(stagePath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("directory barrier ran before stage unlink: %v", statErr)
		}
		return wantErr
	})
	if !called || !errors.Is(err, wantErr) {
		t.Fatalf("stage cleanup result = %v, barrier called=%v", err, called)
	}
}

func TestInstallStateRunnerRejectsSupervisorSymlink(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	target := supervisorfixture.CanonicalTempDir(t)
	if err := os.Symlink(target, filepath.Join(root, ".supervisor")); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, source, "runner")
	if _, err := InstallStateRunner(root, source); err == nil {
		t.Fatal("state runner accepted a symlinked supervisor directory")
	}
}

func TestResolvePrivateSupervisorExecutablesPrefersStateRootPairToPATH(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	client, daemon := writeSavedSupervisorPair(t)
	if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}

	pathDir := supervisorfixture.CanonicalTempDir(t)
	pathClient := filepath.Join(pathDir, "pueue")
	pathDaemon := filepath.Join(pathDir, "pueued")
	supervisorfixture.WriteExecutable(t, pathClient, "unrelated PATH client")
	supervisorfixture.WriteExecutable(t, pathDaemon, "unrelated PATH daemon")
	t.Setenv("PATH", pathDir)

	gotClient, gotDaemon, err := ResolvePrivateSupervisorExecutables(root, "")
	if err != nil {
		t.Fatalf("resolve private supervisor pair: %v", err)
	}
	wantClient, wantDaemon, found, err := ResolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if err != nil || !found {
		t.Fatalf("resolve verified state-root pair: found=%v err=%v", found, err)
	}
	if gotClient != wantClient || gotDaemon != wantDaemon {
		t.Fatalf("private pair = %q / %q, want state-root pair %q / %q", gotClient, gotDaemon, wantClient, wantDaemon)
	}
	if gotClient == pathClient || gotDaemon == pathDaemon {
		t.Fatal("private supervisor selected unrelated PATH executables")
	}
}

func TestResolvePrivateSupervisorExecutablesPrefersStateRootPairToCurrentBundle(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	legacyClient, legacyDaemon := writeSavedSupervisorPair(t)
	if err := installStateSupervisorPair(context.Background(), root, legacyClient, legacyDaemon); err != nil {
		t.Fatal(err)
	}

	bundle := supervisorfixture.CanonicalTempDir(t)
	currentClient := filepath.Join(bundle, "pueue")
	currentDaemon := filepath.Join(bundle, "pueued")
	supervisorfixture.WriteExecutable(t, currentClient, "current client")
	supervisorfixture.WriteExecutable(t, currentDaemon, "current daemon")
	delegate := filepath.Join(bundle, "delegate")
	supervisorfixture.WriteExecutable(t, delegate, "delegate")

	gotClient, gotDaemon, err := ResolvePrivateSupervisorExecutables(root, delegate)
	if err != nil {
		t.Fatalf("resolve private supervisor pair: %v", err)
	}
	wantClient, wantDaemon, found, err := ResolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if err != nil || !found {
		t.Fatalf("resolve verified state-root pair: found=%v err=%v", found, err)
	}
	if gotClient != wantClient || gotDaemon != wantDaemon {
		t.Fatalf("private pair = %q / %q, want state-root pair %q / %q", gotClient, gotDaemon, wantClient, wantDaemon)
	}
}

func TestResolveCurrentPrivateSupervisorExecutablesPrefersCurrentBundle(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	legacyClient, legacyDaemon := writeSavedSupervisorPair(t)
	if err := installStateSupervisorPair(context.Background(), root, legacyClient, legacyDaemon); err != nil {
		t.Fatal(err)
	}

	bundle := supervisorfixture.CanonicalTempDir(t)
	currentClient := filepath.Join(bundle, "pueue")
	currentDaemon := filepath.Join(bundle, "pueued")
	supervisorfixture.WriteExecutable(t, currentClient, "current client")
	supervisorfixture.WriteExecutable(t, currentDaemon, "current daemon")
	delegate := filepath.Join(bundle, "delegate")
	supervisorfixture.WriteExecutable(t, delegate, "delegate")

	gotClient, gotDaemon, err := ResolveCurrentPrivateSupervisorExecutables(root, delegate)
	if err != nil {
		t.Fatalf("resolve current private supervisor pair: %v", err)
	}
	if gotClient != currentClient || gotDaemon != currentDaemon {
		t.Fatalf("current private pair = %q / %q, want current bundle %q / %q", gotClient, gotDaemon, currentClient, currentDaemon)
	}
}
