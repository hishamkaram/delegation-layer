package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

func legacyDaemonExecutable(t *testing.T, path string) string {
	t.Helper()
	return canonicalPrivateTestExecutable(t, path)
}

func newPrivateSupervisorTestFixture(t *testing.T) privateSupervisorTestFixture {
	t.Helper()
	return newPrivateSupervisorTestFixtureAtBase(t, canonicalTemp(t))
}

// The caller owns base and must join its clients and daemon waits before removal.
func newPrivateSupervisorTestFixtureAtBase(t *testing.T, base string) privateSupervisorTestFixture {
	t.Helper()
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
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
	writePrivateStateRootRecord(t, stateRoot)
	return base, stateRoot
}

func writePrivateStateRootRecord(t *testing.T, stateRoot string) {
	t.Helper()
	makePrivateTestDirectory(t, stateRoot)
	rootID, err := randomid.NewRootID()
	if err != nil {
		t.Fatal(err)
	}
	rootData, err := task.MarshalCanonical(task.RootRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        rootID,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "root.json"), rootData, 0o600); err != nil {
		t.Fatal(err)
	}
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
	return bindPrivateWithDaemonWaitOwnerForTest(t, stateRoot, clientPath, daemonPath, privateFixtureDaemonWaitOwner(t))
}

func bindPrivateWithDaemonWaitOwnerForTest(t *testing.T, stateRoot, clientPath, daemonPath string, waitOwner DaemonWaitOwner) *Client {
	t.Helper()
	client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: DefaultObservationTimeout, SuccessfulCleanupAuthorizer: allowSuccessfulCleanupForTest, PrivateTerminalEventRunner: privateInstalledCallbackForTest(t, stateRoot), DaemonWaitOwner: waitOwner})
	t.Cleanup(func() {
		if closeErr := client.CloseAfterPending(err); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// These component daemons do not execute callbacks. The original authored
// installed role qualifies callback metadata; an unexpected invocation fails.
func privateInstalledCallbackForTest(t *testing.T, stateRoot string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(stateRoot), "delegate-run-terminal-fixture")
	writeExecutable(t, path, "#!/bin/sh\nprintf 'unexpected component callback execution\\n' >&2\nexit 97\n")
	return path
}

func privateFixtureDaemonWaitOwner(t *testing.T) DaemonWaitOwner {
	t.Helper()
	return daemonWaitOwnerFunc(func(_ string, _ int, done <-chan struct{}) error {
		t.Cleanup(func() {
			// The authored setup daemon exits after publishing readiness. Join
			// its actual Wait before the fixture's existing path cleanup runs.
			<-done
		})
		return nil
	})
}

// bindPrivateSetupForTest separates filesystem/bootstrap setup from the short
// observation deadlines exercised by fault tests.
func bindPrivateSetupForTest(t *testing.T, stateRoot, clientPath, daemonPath string, options Options) *Client {
	t.Helper()
	if options.PrivateTerminalEventRunner == "" {
		options.PrivateTerminalEventRunner = privateInstalledCallbackForTest(t, stateRoot)
	}
	if options.DaemonWaitOwner == nil {
		options.DaemonWaitOwner = privateFixtureDaemonWaitOwner(t)
	}
	if options.PrivateGenerationGuard == nil {
		options.PrivateGenerationGuard = allowPrivateGenerationForTest{}
	}
	if options.processObserver == nil {
		options.processObserver = privateTestProcessObserver(stateRoot)
	}
	setup := options
	setup.ObservationTimeout = 10 * time.Second
	client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, setup)
	if err != nil {
		t.Fatal(err)
	}
	client.options = options
	return client
}

func preparePrivateCandidateForTest(t *testing.T, stateRoot, clientPath, daemonPath string, options Options) *Client {
	t.Helper()
	if options.PrivateGenerationGuard == nil {
		options.PrivateGenerationGuard = allowPrivateGenerationForTest{}
	}
	if options.processObserver == nil {
		options.processObserver = privateTestProcessObserver(stateRoot)
	}
	var client *Client
	err := WithBootstrapLockContext(t.Context(), stateRoot, func(ctx context.Context) error {
		var prepareErr error
		client, prepareErr = PreparePrivateForMigrationWithLock(ctx, clientPath, daemonPath, stateRoot, options)
		return prepareErr
	})
	t.Cleanup(func() {
		if closeErr := client.CloseAfterPending(err); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func allowSuccessfulCleanupForTest(_ context.Context, _ string, _ task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
	var plan PrivateQueueCleanupPlan
	for _, job := range jobs {
		if job.State == StateEnded && job.Succeeded {
			plan.RemoveIDs = append(plan.RemoveIDs, job.ID)
		}
	}
	return plan, nil
}

func privateTestProcessObserver(stateRoot string) func(int) (privateProcessObservation, error) {
	readyPath := filepath.Join(stateRoot, privateSupervisorDirectory, "ready")
	return func(pid int) (privateProcessObservation, error) {
		if _, err := os.Stat(readyPath); errors.Is(err, os.ErrNotExist) {
			return privateProcessObservation{}, nil
		} else if err != nil {
			return privateProcessObservation{}, err
		}
		return privateProcessObservation{Present: true, StartToken: "fixture:" + strconv.Itoa(pid)}, nil
	}
}

func privateMigrationTestOptions(stateRoot string, timeout time.Duration) Options {
	return Options{
		PrivateGenerationGuard:     allowPrivateGenerationForTest{},
		DaemonStartGuard:           allowDaemonStartForTest{},
		ObservationTimeout:         timeout,
		PrivateMigrationAuthorizer: allowPrivateMigrationForTest,
		processObserver:            privateTestProcessObserver(stateRoot),
	}
}

func allowPrivateMigrationForTest(_ context.Context, _ string, _ task.SupervisorRef, jobs []Job) error {
	for _, job := range jobs {
		if job.State == StateQueued || job.State == StateRunning || job.State == StateUnknown {
			return ErrPrivateSupervisorMigrationBusy
		}
	}
	return nil
}

func removePrivateTestPID(t *testing.T, stateRoot string) {
	t.Helper()
	path := filepath.Join(stateRoot, privateSupervisorDirectory, "run", "pueue.pid")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func removePrivateTestDaemonState(t *testing.T, stateRoot string) {
	t.Helper()
	readyPath := filepath.Join(stateRoot, privateSupervisorDirectory, "ready")
	if err := os.Remove(readyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("remove private fixture readiness marker: %v", err)
	}
	removePrivateTestPID(t, stateRoot)
}

func assertPrivateRecoverySucceeded(t *testing.T, recovery PrivateRecovery, err error) {
	t.Helper()
	if err != nil || recovery.PairUnavailable() || recovery.Client() == nil || recovery.Cause() != nil {
		t.Fatalf("private recovery did not succeed: result=%+v err=%v", recovery, err)
	}
}

func assertPrivateRecoveryHasNoResult(t *testing.T, recovery PrivateRecovery) {
	t.Helper()
	if recovery.PairUnavailable() || recovery.Client() != nil || recovery.Cause() != nil {
		t.Fatalf("operational failure returned a private recovery result: %+v", recovery)
	}
}

func recoverPrivateForTest(t *testing.T, stateRoot string, saved task.SupervisorRef, clientPath, daemonPath string) *Client {
	t.Helper()
	recovery, err := AttemptPrivateRecovery(context.Background(), stateRoot, saved, clientPath, daemonPath, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: DefaultObservationTimeout})
	client := recovery.Client()
	assertPrivateRecoverySucceeded(t, recovery, err)
	return client
}

func readPrivateSupervisorFile(t *testing.T, path string) []byte {
	t.Helper()
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("executable contents are not fixture evidence: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func snapshotPrivateTestFile(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("executable contents are not fixture evidence: %s", path)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return data, true
}

func assertPrivateTestFileUnchanged(t *testing.T, path string, want []byte, wantPresent bool) {
	t.Helper()
	got, gotPresent := snapshotPrivateTestFile(t, path)
	if gotPresent != wantPresent || !bytes.Equal(got, want) {
		t.Fatalf("private test artifact %s changed", path)
	}
}

func snapshotPrivateTestFiles(t *testing.T, paths []string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, found := snapshotPrivateTestFile(t, path)
		if found {
			files[path] = data
		}
	}
	return files
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
	if identity != privateDaemonIdentityForBinding(binding) {
		t.Fatalf("private daemon identity does not match binding: identity=%+v binding=%+v", identity, binding)
	}
}

func assertPrivateDaemonIdentityMatchesPair(t *testing.T, stateRoot, daemonPath string) {
	t.Helper()
	wantExecutable := canonicalPrivateTestExecutable(t, daemonPath)
	got := readPrivateDaemonIdentity(t, stateRoot)
	if got.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || got.Executable != wantExecutable || got.SHA256 != "" || !privateLogicalIdentityPresent(got) {
		t.Fatalf("private daemon identity does not match launched pair: got=%+v want installed executable=%q", got, wantExecutable)
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
	data := statusTestPayload(t, map[string]any{strconv.FormatInt(id, 10): job}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
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

// allowPrivateGenerationForTest supplies explicit resident publication authority
// to fixture setup. Production options never receive a default grant.
type allowPrivateGenerationForTest struct{}

func (allowPrivateGenerationForTest) GuardPrivateGenerationCommit(_ context.Context, _ string, commit func() error) error {
	return commit()
}
