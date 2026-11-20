package gatewaycommand

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

// This fixture lends start/publication authority only for its own scratch root.
// The command under test uses the default production reconciliation gate.
// Pueue owns Start and the sole Wait; the fixture joins its registered completion
// before removing any paths. No provider or task worker is started.
type nativeReconciliationFixture struct {
	base           string
	baseInfo       os.FileInfo
	root           string
	store          *taskdir.Store
	client         *pueue.Client
	waits          []<-chan struct{}
	stopped        bool
	historicalBoot string
	environment    []string
	dependencies   Dependencies
	sdk            *nativeReconciliationClient
	task           *taskdir.TaskDir
}

func (f *nativeReconciliationFixture) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	if root != f.root || start == nil {
		return task.ErrInvalidPermit
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return start()
}

func (f *nativeReconciliationFixture) GuardPrivateGenerationCommit(ctx context.Context, root string, commit func() error) error {
	if root != f.root || commit == nil {
		return task.ErrInvalidPermit
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return commit()
}

func (f *nativeReconciliationFixture) RegisterDaemonWait(root string, pid int, done <-chan struct{}) error {
	if root != f.root || pid <= 0 || done == nil {
		return task.ErrIdentityMismatch
	}
	f.waits = append(f.waits, done)
	return nil
}

// The SDK edge is a test double. It verifies that the actual private profile
// directory lock is held when the public command checks connector inactivity.
type nativeReconciliationClient struct {
	reconciliationClient
	profile      string
	lockObserved bool
}

func (c *nativeReconciliationClient) RuntimeInactive(ctx context.Context, profile string) (inactive bool, resultErr error) {
	if profile != c.profile {
		return false, task.ErrIdentityMismatch
	}
	lock, err := taskdir.OpenDirectoryLock(profile, taskdir.LockLevelNone)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	if err = lock.LockEXNonblockingContext(ctx); !errors.Is(err, task.ErrLockBusy) {
		return false, errors.Join(errors.New("profile coordination lock was not held"), err)
	}
	c.lockObserved = true
	return true, nil
}

func newNativeReconciliationFixture(t *testing.T) *nativeReconciliationFixture {
	t.Helper()
	clientPath, daemonPath := os.Getenv("DELEGATE_TEST_PUEUE"), os.Getenv("DELEGATE_TEST_PUEUED")
	if clientPath == "" || daemonPath == "" {
		t.Skip("native Pueue testkit pair is not configured")
	}
	base, err := os.MkdirTemp("", "dgr-")
	if err != nil {
		t.Fatal(err)
	}
	f := &nativeReconciliationFixture{base: base}
	t.Cleanup(func() { f.cleanup(t) })
	f.baseInfo, err = os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	f.base, f.root = canonical, filepath.Join(canonical, "state")
	f.environment = []string{"HOME=" + canonical, "XDG_DATA_HOME=" + filepath.Join(canonical, "data"), "XDG_CONFIG_HOME=" + filepath.Join(canonical, "config")}
	f.store, err = taskdir.InitializeConnectorStoreContext(boundedTestContext(t), f.root, predicate.Default(), true)
	if err != nil {
		t.Fatal(err)
	}
	f.prepareHistoricalProfile(t)
	f.start(t, clientPath, daemonPath)
	f.prepareClosedTask(t)
	f.dependencies = commandDependencies(t, f.sdk)
	f.dependencies.Environment = append([]string(nil), f.environment...)
	if f.dependencies.RootContextGate != nil {
		t.Fatal("native test substituted the default reconciliation gate")
	}
	return f
}

func (f *nativeReconciliationFixture) prepareHistoricalProfile(t *testing.T) {
	t.Helper()
	pin, err := f.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	origin := task.RootContextCoordinates{Device: pin.Device() + 1, Inode: pin.Inode()}
	token, err := task.ConnectorRootToken(pin.RootID(), origin)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.historicalBoot = nativeReconciliationPreviousBoot(t)
	inputs := infrastructure.StartupInputs{
		Kind: infrastructure.StartupRootOwner, Root: pin.Root(), RootID: pin.RootID(),
		RootDevice: origin.Device, RootInode: origin.Inode, BootID: f.historicalBoot,
		Artifacts: infrastructure.Artifacts{Runner: runner}, Context: infrastructure.LaunchContextReference{SHA256: strings.Repeat("d", 64)},
	}
	digest, err := inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	intent := infrastructure.StartupIntent{Protocol: infrastructure.RecordProtocolVersion, Sequence: 1, Epoch: strings.Repeat("e", 32), Inputs: inputs, InputsSHA256: digest, AcceptedAt: "2026-11-03T00:00:00Z", Deadline: "2026-11-03T00:00:30Z"}
	if err = intent.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(intent)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(f.root, "infrastructure")
	if err = os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "intent-00000000000000000001.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(f.root, "gateway")
	if err = os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(profile, "profile.json"), []byte("retained-native-profile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.sdk = &nativeReconciliationClient{profile: profile, reconciliationClient: reconciliationClient{inactive: true, commandClient: commandClient{local: deviceclient.LocalContextInfo{DelegateRoot: pin.Root(), DelegateRootContext: token}}}}
}

func (f *nativeReconciliationFixture) start(t *testing.T, clientPath, daemonPath string) {
	t.Helper()
	options := pueue.Options{Environment: f.environment, ObservationTimeout: 15 * time.Second, WaitForPendingClose: true, DaemonStartGuard: f, PrivateGenerationGuard: f, DaemonWaitOwner: f}
	err := pueue.WithBootstrapLockContext(boundedTestContext(t), f.root, func(ctx context.Context) error {
		var err error
		f.client, err = pueue.PreparePrivateWithLock(ctx, clientPath, daemonPath, f.root, options)
		if err != nil {
			return err
		}
		return f.client.StartPrivate(ctx, f.root)
	})
	if err != nil {
		t.Fatal(err)
	}
	complete, err := pueue.PrivateSupervisorMigrationComplete(f.root, f.client.Binding())
	if err != nil || !complete || len(f.waits) != 1 {
		t.Fatalf("native generation is incomplete: complete=%t waits=%d err=%v", complete, len(f.waits), err)
	}
	snapshot, err := f.client.Snapshot(boundedTestContext(t))
	if err != nil || len(snapshot.Jobs) != 0 {
		t.Fatalf("native scratch queue was not ready and empty: jobs=%d err=%v", len(snapshot.Jobs), err)
	}
}

func (f *nativeReconciliationFixture) prepareClosedTask(t *testing.T) {
	t.Helper()
	f.task, _ = lifecyclefixture.NewAppTaskInStoreWithSupervisorConfig(t, f.store, strings.Repeat("c", 32), false, nil, f.client.Binding().ConfigPath)
	// These fixture records represent an already closed worker. They create no
	// OS task scope; the real gate must still validate their complete closure.
	lifecyclefixture.RecordRecoverableLifetime(t, f.task, f.historicalBoot)
	owner, err := f.task.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	worker, err := f.task.ReadLifetimeWorkerContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(worker)
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	if _, err = f.task.RecordLifetimeWorkerWaitContext(t.Context(), taskdir.LifetimeWorkerWaitFacts{WorkerSHA256: task.ComputeSHA256(data), ChildPID: worker.PID, Result: task.LifetimeWorkerWaitSuccess, ExitCode: &code}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.task.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationCompleted); err != nil {
		t.Fatal(err)
	}
}

func (f *nativeReconciliationFixture) stop(ctx context.Context) error {
	if f.stopped || len(f.waits) == 0 {
		return nil
	}
	err := pueue.WithExistingBootstrapLockContext(ctx, f.root, func(locked context.Context) error { return f.client.Shutdown(locked) })
	if err != nil {
		return err
	}
	for _, done := range f.waits {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("native daemon Wait did not close: %w", ctx.Err())
		}
	}
	f.stopped = true
	return nil
}

func (f *nativeReconciliationFixture) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stopErr := f.stop(ctx)
	var closeErr error
	if f.client != nil {
		closeErr = f.client.CloseAfterPending(stopErr)
	}
	if f.store != nil {
		closeErr = errors.Join(closeErr, f.store.Close())
	}
	if err := errors.Join(stopErr, closeErr); err != nil {
		t.Errorf("retained native reconciliation fixture after unresolved closure at %s: %v", f.base, err)
		return
	}
	current, err := os.Stat(f.base)
	if err != nil || f.baseInfo == nil || !os.SameFile(current, f.baseInfo) {
		t.Errorf("retained replaced native reconciliation fixture %s: %v", f.base, err)
		return
	}
	if err = os.RemoveAll(f.base); err != nil {
		t.Error(err)
	}
	t.Log("native scratch daemon shutdown completed, canonical Wait joined, clients closed and owned root removed")
}

func TestGatewayRootContextNativeDefaultGate(t *testing.T) {
	for _, scenario := range []string{"quiet", "native-work-pending", "maintenance-lock-pending", "unreadable-closure", "snapshot-fault"} {
		t.Run(scenario, func(t *testing.T) { testGatewayRootContextNativeDefaultGate(t, scenario) })
	}
}

func testGatewayRootContextNativeDefaultGate(t *testing.T, scenario string) {
	t.Helper()
	f := newNativeReconciliationFixture(t)
	prepared := preparedReconciliation(t, f.root, f.dependencies)
	rootPath := filepath.Join(f.root, "root.json")
	beforeRoot := nativeReconciliationRead(t, rootPath)
	profilePath, requestPath := filepath.Join(f.sdk.profile, "profile.json"), filepath.Join(f.task.Dir, "task.json")
	beforeProfile, beforeTask := nativeReconciliationRead(t, profilePath), nativeReconciliationRead(t, requestPath)
	f.prepareScenario(t, scenario)
	confirmed, code := f.confirmScenario(t, scenario, prepared.RootContext.Challenge)
	if !f.sdk.lockObserved {
		t.Fatal("confirmation bypassed the real profile directory lock")
	}
	if scenario == "quiet" {
		if code != 0 || confirmed.RootContext == nil || confirmed.NextAction != "run_gateway_doctor" {
			t.Fatalf("default real-Pueue gate did not confirm: exit%d %+v", code, confirmed)
		}
		actual, err := f.store.RootTokenContext(t.Context())
		if err != nil || actual != f.sdk.local.DelegateRootContext {
			t.Fatalf("real-gate repair lost the saved token: %v", err)
		}
	} else {
		requireNativeReconciliationRefusal(t, scenario, confirmed, code)
		if !bytes.Equal(beforeRoot, nativeReconciliationRead(t, rootPath)) {
			t.Fatal("refused confirmation changed root.json")
		}
	}
	if !bytes.Equal(beforeProfile, nativeReconciliationRead(t, profilePath)) || !bytes.Equal(beforeTask, nativeReconciliationRead(t, requestPath)) {
		t.Fatal("default gate confirmation changed saved profile or immutable task request")
	}
}

func requireNativeReconciliationRefusal(t *testing.T, scenario string, response Response, code int) {
	t.Helper()
	wantFailure, wantAction, wantCode := "root_context_evidence_unreadable", "stop_and_report", 1
	if scenario == "native-work-pending" || scenario == "maintenance-lock-pending" {
		wantFailure, wantAction, wantCode = "root_context_native_work_pending", "wait_for_native_work", 3
	}
	if code != wantCode || response.Failure != wantFailure || response.NextAction != wantAction {
		t.Fatalf("default gate %s: exit%d %+v", scenario, code, response)
	}
}

func (f *nativeReconciliationFixture) confirmScenario(t *testing.T, scenario, challenge string) (response Response, code int) {
	t.Helper()
	run := func() {
		response, code = runReconciliation(t, f.root, Arguments{Action: "repair-context", ConfirmContext: challenge, OfflineConfirmed: true}, f.dependencies)
	}
	if scenario != "maintenance-lock-pending" {
		run()
		return response, code
	}
	// A separate normal reader owns real maintenance SH while confirmation
	// takes its own bootstrap EX and attempts maintenance EX nonblocking.
	if err := f.store.WithExistingReadObservation(boundedTestContext(t), func(context.Context) error { run(); return nil }); err != nil {
		t.Fatal(err)
	}
	return response, code
}

func (f *nativeReconciliationFixture) prepareScenario(t *testing.T, scenario string) {
	t.Helper()
	switch scenario {
	case "quiet", "maintenance-lock-pending":
	case "native-work-pending":
		owner, err := f.task.AcquireLifetimeOwner(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if releaseErr := owner.Release(); releaseErr != nil {
				t.Error(releaseErr)
			}
		})
	case "unreadable-closure":
		if err := os.WriteFile(filepath.Join(f.task.Dir, "lifetime", "terminated.json"), []byte("{\"unreadable\":"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "snapshot-fault":
		if err := f.stop(boundedTestContext(t)); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown native reconciliation scenario %q", scenario)
	}
}

func nativeReconciliationRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func nativeReconciliationPreviousBoot(t *testing.T) string {
	t.Helper()
	current, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	separator := strings.LastIndex(current, ":")
	if separator >= 0 {
		for _, uuid := range []string{"00000000-0000-0000-0000-000000000000", "11111111-1111-1111-1111-111111111111"} {
			previous := current[:separator+1] + uuid
			if lifetime.CompareBootIDs(previous, current) == lifetime.BootDifferent {
				return previous
			}
		}
	}
	t.Fatal("native boot identity cannot establish a previous-boot fixture")
	return ""
}
