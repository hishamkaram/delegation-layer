package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type upgradeSubmissionTransportFixture struct {
	private generationGuardFixture
	fake    *fakeSupervisor
	client  *Client
	pin     taskdir.RootPin
}

// Actual saved-binding constructors retain a logical private owner while
// selecting another config for finite transport. No daemon/provider starts.
func newUpgradeSubmissionTransportFixture(t *testing.T, kind string) upgradeSubmissionTransportFixture {
	t.Helper()
	private, _ := runtimeRestartJournalFixture(t)
	fake := newFakeSupervisor(t, "ok")
	t.Cleanup(func() {
		if err := fake.client.Close(); err != nil {
			t.Error(err)
		}
	})
	options := private.client.options
	options.Environment = fake.environment
	options.ControlOwnerExecutable = ""
	options.RuntimeClientExecutable = fake.executable
	var client *Client
	var err error
	switch kind {
	case "alias":
		data, readErr := readRegular(private.client.Binding().ConfigPath, MaxControlBytes)
		if readErr != nil {
			t.Fatal(readErr)
		}
		path := privateRecoveryConfigPath(private.root)
		if err = os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		record := privateRecoveryBindingRecord{SchemaVersion: task.SchemaVersion, Supervisor: private.client.Binding(), ConfigPath: path, ConfigSHA256: task.ComputeSHA256(data)}
		client, err = bindPrivateRecoveryTransport(t.Context(), record, record.Supervisor, fake.executable, record.Supervisor.DaemonExecutable, options)
	case "temporary":
		current, bindErr := Bind(t.Context(), fake.executable, private.client.Binding().ConfigPath, options)
		if bindErr != nil {
			t.Fatal(bindErr)
		}
		client, err = bindSavedEndpointMigration(t.Context(), private.client.Binding(), current)
		err = errors.Join(err, current.CloseAfterPending(err))
	default:
		t.Fatalf("unknown transport fixture %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if client.Binding() != private.client.Binding() || client.RuntimeBinding().ConfigPath == client.Binding().ConfigPath {
		t.Fatal("constructor did not retain logical owner with separate transport")
	}
	if _, privateRuntime := privateStateRoot(client.RuntimeBinding().ConfigPath); privateRuntime {
		t.Fatal("fixture transport unexpectedly classifies as private primary config")
	}
	if err = client.verify(t.Context()); err != nil {
		t.Fatalf("retained transport does not verify before Drain: %v", err)
	}
	pin, err := readControlRootPin(t.Context(), private.root)
	if err != nil {
		t.Fatal(err)
	}
	return upgradeSubmissionTransportFixture{private: private, fake: fake, client: client, pin: pin}
}

func commitUpgradeSubmissionFence(t *testing.T, f upgradeSubmissionTransportFixture) {
	t.Helper()
	drain := privateUpgradeDrain{Root: upgradeRootSubject(f.pin), Predecessor: f.client.Binding(), OperationID: UpgradeOperationID("upgrade-v1:" + strings.Repeat("c", 32)), Mode: UpgradeOperationRequired, Phase: UpgradeDraining}
	err := WithExistingBootstrapLockContext(t.Context(), f.pin.Root(), func(ctx context.Context) error {
		return guardPrivateGeneration(ctx, f.pin.Root(), f.private.client.options.PrivateGenerationGuard, func() error { return writePrivateUpgrade(f.pin.Root(), drain, false) })
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeDrainFencesRetainedAliasAndTemporarySubmissions(t *testing.T) {
	for _, kind := range []string{"alias", "temporary"} {
		for _, staged := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/ordinary", true: "/staged"}[staged], func(t *testing.T) { testRetainedTransportSubmissionFence(t, kind, staged, true) })
		}
	}
}

func testRetainedTransportSubmissionFence(t *testing.T, kind string, staged, draining bool) {
	t.Helper()
	f := newUpgradeSubmissionTransportFixture(t, kind)
	fixture := newTaskFixture(t, f.client.Binding())
	permit, err := fixture.taskDir.PrepareSubmission(f.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	refusal := retainedTransportSubmissionRefusal(t, f, draining)
	launch := Launch{RunnerExecutable: f.fake.executable, RootPath: fixture.store.Root, Supervised: true}
	if staged {
		_, err = f.client.SubmitStagedWithGate(t.Context(), permit, launch, nil, func(Observation) error { t.Fatal("Drain reached durable staged commit"); return nil })
	} else {
		_, err = f.client.SubmitWithGate(t.Context(), permit, launch, nil)
	}
	if !errors.Is(err, refusal) {
		t.Fatalf("logical private %s admitted through transport config: %v", kind, err)
	}
	if permit.IsConsumed() {
		t.Fatal("Drain consumed the submission permit")
	}
	assertUpgradeNoSubmissionCommands(t, f.fake)
}

func TestUpgradeDrainFencesRetainedTransportReplay(t *testing.T) {
	for _, kind := range []string{"alias", "temporary"} {
		t.Run(kind, func(t *testing.T) { testRetainedTransportReplayFence(t, kind, true) })
	}
}

func testRetainedTransportReplayFence(t *testing.T, kind string, draining bool) {
	t.Helper()
	f := newUpgradeSubmissionTransportFixture(t, kind)
	fixture := newTaskFixture(t, f.client.Binding())
	submission := createSubmission(t, fixture, f.client.Binding())
	identity := identityFromRecord(submission)
	writeStagedOrdinaryStatus(t, f.fake, identity, 7, StateStashed, "", "default")
	refusal := retainedTransportSubmissionRefusal(t, f, draining)
	_, err := f.client.RecoverStagedWithGate(t.Context(), identity, 7, nil, func(Observation) error { t.Fatal("retained transport reached replay commit"); return nil })
	if !errors.Is(err, refusal) {
		t.Fatalf("stashed replay admitted through retained transport: %v", err)
	}
	assertUpgradeNoSubmissionCommands(t, f.fake)
}

func TestUpgradeDrainFencesRetainedTransportInspection(t *testing.T) {
	for _, kind := range []string{"alias", "temporary"} {
		t.Run(kind, func(t *testing.T) { testRetainedTransportInspectionFence(t, kind, true) })
	}
}

func testRetainedTransportInspectionFence(t *testing.T, kind string, draining bool) {
	t.Helper()
	f := newUpgradeSubmissionTransportFixture(t, kind)
	identity := InspectionIdentity{RootID: f.pin.RootID(), TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	launch := Launch{RunnerExecutable: f.fake.executable, RootPath: f.pin.Root(), Supervised: true}
	permit := &concurrentInspectionTestPermit{inspectionTestPermit: &inspectionTestPermit{submissionRoot: identity.RootID, submissionTask: identity.TaskID, submissionSupervisor: f.client.Binding()}, queueGroup: "default"}
	refusal := retainedTransportSubmissionRefusal(t, f, draining)
	_, err := f.client.SubmitConcurrentInspectionStagedWithGate(t.Context(), permit, identity, launch, nil, func(InspectionObservation) error { t.Fatal("retained transport reached inspection commit"); return nil })
	if !errors.Is(err, refusal) {
		t.Fatalf("inspection admitted through retained transport: %v", err)
	}
	if permit.calls != 0 {
		t.Fatal("retained transport consumed the inspection permit")
	}
	assertUpgradeNoSubmissionCommands(t, f.fake)
}

func retainedTransportSubmissionRefusal(t *testing.T, f upgradeSubmissionTransportFixture, draining bool) error {
	t.Helper()
	if draining {
		commitUpgradeSubmissionFence(t, f)
		return ErrPrivateUpgradePending
	}
	return ErrPrivateSupervisorSubmissionIncomplete
}

func TestRetainedPrivateTransportCannotAdmit(t *testing.T) {
	for _, kind := range []string{"alias", "temporary"} {
		t.Run(kind, func(t *testing.T) {
			t.Run("ordinary", func(t *testing.T) { testRetainedTransportSubmissionFence(t, kind, false, false) })
			t.Run("staged", func(t *testing.T) { testRetainedTransportSubmissionFence(t, kind, true, false) })
			t.Run("replay", func(t *testing.T) { testRetainedTransportReplayFence(t, kind, false) })
			t.Run("inspection", func(t *testing.T) { testRetainedTransportInspectionFence(t, kind, false) })
		})
	}
}

func TestRetainedTransportRefusalPreservesBootstrapReadError(t *testing.T) {
	f := newUpgradeSubmissionTransportFixture(t, "alias")
	if err := os.WriteFile(privateBootstrapRecordPath(f.private.root), []byte("}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.client.checkSubmissionGate(t.Context(), nil)
	if !errors.Is(err, ErrBinding) || errors.Is(err, ErrPrivateSupervisorSubmissionIncomplete) {
		t.Fatalf("transport mismatch hid the canonical bootstrap read error: %v", err)
	}
}

func TestRetainedTransportAdmissionGuardKeepsPrimaryAndExternalSubmission(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "external", true: "same-config-compatible-executable"}[private], func(t *testing.T) {
			client, fake := newAdmissibleSubmissionTransport(t, private)
			fixture := newTaskFixture(t, client.Binding())
			permit, err := fixture.taskDir.PrepareSubmission(client.Binding())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if releaseErr := permit.Release(); releaseErr != nil {
					t.Error(releaseErr)
				}
			})
			calls := 0
			gate := func(_ context.Context, runtime task.SupervisorRef) (SubmissionAuthorization, error) {
				calls++
				if runtime != client.RuntimeBinding() {
					t.Fatal("admission guard changed runtime capability subject")
				}
				return SubmissionAuthorization{}, nil
			}
			launch := Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root, Supervised: true}
			_, err = client.SubmitWithGate(t.Context(), permit, launch, gate)
			if err != nil || calls != 1 || !permit.IsConsumed() || countCommand(readInvocations(t, fake.logPath), "add") != 1 {
				t.Fatalf("supported transport lost admission: calls=%d consumed=%t err=%v", calls, permit.IsConsumed(), err)
			}
		})
	}
}

func newAdmissibleSubmissionTransport(t *testing.T, private bool) (*Client, *fakeSupervisor) {
	t.Helper()
	fake := newFakeSupervisor(t, "ok")
	t.Cleanup(func() {
		if err := fake.client.Close(); err != nil {
			t.Error(err)
		}
	})
	if !private {
		return fake.client, fake
	}
	f, _ := runtimeRestartJournalFixture(t)
	options := f.client.options
	options.Environment = fake.environment
	options.ControlOwnerExecutable = ""
	options.RuntimeClientExecutable = fake.executable
	if err := os.Remove(f.client.Binding().ClientExecutable); err != nil {
		t.Fatal(err)
	}
	client, err := BindSavedForMigrationWithClient(t.Context(), f.client.Binding(), fake.executable, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if client.RuntimeBinding().ConfigPath != client.Binding().ConfigPath || client.RuntimeBinding().ClientExecutable == client.Binding().ClientExecutable {
		t.Fatal("fixture did not select another executable for the exact primary config")
	}
	return client, fake
}

func TestPrivateSupervisorNativePrimaryTransportAdmissionGate(t *testing.T) {
	f := newNativePrivateSupervisorFixture(t)
	client, err := f.bind(t)
	if err != nil {
		t.Fatal(err)
	}
	if client.RuntimeBinding().ConfigPath != client.Binding().ConfigPath {
		t.Fatal("native primary transport changed config spelling")
	}
	if runtime.GOOS == "darwin" && !strings.HasPrefix(client.Binding().ConfigPath, "/private/") {
		t.Fatalf("native macOS fixture did not use canonical /private path: %s", client.Binding().ConfigPath)
	}
	calls := 0
	err = client.withPrivateSupervisorLease(t.Context(), func(ctx context.Context) error {
		_, gateErr := client.checkSubmissionGate(ctx, func(_ context.Context, actual task.SupervisorRef) (SubmissionAuthorization, error) {
			calls++
			if actual != client.RuntimeBinding() {
				t.Fatal("native gate changed runtime subject")
			}
			return SubmissionAuthorization{}, nil
		})
		return gateErr
	})
	if err != nil || calls != 1 {
		t.Fatalf("native primary transport lost admission gate: calls=%d err=%v", calls, err)
	}
}

func assertUpgradeNoSubmissionCommands(t *testing.T, fake *fakeSupervisor) {
	t.Helper()
	for _, command := range []string{"add", "enqueue"} {
		if countCommand(readInvocations(t, fake.logPath), command) != 0 {
			t.Fatalf("Drain reached %s", command)
		}
	}
}

func TestUpgradeDrainLookupPreservesExternalSupervisorAndRuntimeGate(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	t.Cleanup(func() {
		if err := fake.client.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture := newTaskFixture(t, fake.client.Binding())
	base := filepath.Join(fixture.store.Root, privateSupervisorDirectory)
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, privateBootstrapRecordName), []byte("unreadable unrelated fence"), 0o600); err != nil {
		t.Fatal(err)
	}
	permit, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	refused := errors.New("external capability gate refusal")
	calls := 0
	gate := func(_ context.Context, runtime task.SupervisorRef) (SubmissionAuthorization, error) {
		calls++
		if runtime != fake.client.RuntimeBinding() {
			t.Fatal("logical ownership changed runtime capability argument")
		}
		return SubmissionAuthorization{}, refused
	}
	_, err = fake.client.SubmitWithGate(t.Context(), permit, Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root}, gate)
	if !errors.Is(err, refused) || calls != 1 || permit.IsConsumed() {
		t.Fatalf("external supervisor inherited unrelated Drain: calls=%d consumed=%t err=%v", calls, permit.IsConsumed(), err)
	}
	assertUpgradeNoSubmissionCommands(t, fake)
}
