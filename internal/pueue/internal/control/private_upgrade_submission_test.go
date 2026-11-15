package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
			t.Run(kind+map[bool]string{false: "/ordinary", true: "/staged"}[staged], func(t *testing.T) { testUpgradeTransportSubmissionFence(t, kind, staged) })
		}
	}
}

func testUpgradeTransportSubmissionFence(t *testing.T, kind string, staged bool) {
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
	commitUpgradeSubmissionFence(t, f)
	launch := Launch{RunnerExecutable: f.fake.executable, RootPath: fixture.store.Root, Supervised: true}
	if staged {
		_, err = f.client.SubmitStagedWithGate(t.Context(), permit, launch, nil, func(Observation) error { t.Fatal("Drain reached durable staged commit"); return nil })
	} else {
		_, err = f.client.SubmitWithGate(t.Context(), permit, launch, nil)
	}
	if !errors.Is(err, ErrPrivateUpgradePending) {
		t.Fatalf("logical private %s crossed Drain through transport config: %v", kind, err)
	}
	if permit.IsConsumed() {
		t.Fatal("Drain consumed the submission permit")
	}
	assertUpgradeNoSubmissionCommands(t, f.fake)
}

func TestUpgradeDrainFencesRetainedTransportReplay(t *testing.T) {
	for _, kind := range []string{"alias", "temporary"} {
		t.Run(kind, func(t *testing.T) {
			f := newUpgradeSubmissionTransportFixture(t, kind)
			fixture := newTaskFixture(t, f.client.Binding())
			submission := createSubmission(t, fixture, f.client.Binding())
			identity := identityFromRecord(submission)
			writeStagedOrdinaryStatus(t, f.fake, identity, 7, StateStashed, "", "default")
			commitUpgradeSubmissionFence(t, f)
			_, err := f.client.RecoverStagedWithGate(t.Context(), identity, 7, nil, func(Observation) error { t.Fatal("Drain reached replay commit"); return nil })
			if !errors.Is(err, ErrPrivateUpgradePending) {
				t.Fatalf("stashed replay crossed logical Drain: %v", err)
			}
			assertUpgradeNoSubmissionCommands(t, f.fake)
		})
	}
}

func TestUpgradeDrainFencesRetainedTransportInspection(t *testing.T) {
	for _, kind := range []string{"alias", "temporary"} {
		t.Run(kind, func(t *testing.T) {
			f := newUpgradeSubmissionTransportFixture(t, kind)
			identity := InspectionIdentity{RootID: f.pin.RootID(), TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
			launch := Launch{RunnerExecutable: f.fake.executable, RootPath: f.pin.Root(), Supervised: true}
			permit := &concurrentInspectionTestPermit{inspectionTestPermit: &inspectionTestPermit{submissionRoot: identity.RootID, submissionTask: identity.TaskID, submissionSupervisor: f.client.Binding()}, queueGroup: "default"}
			commitUpgradeSubmissionFence(t, f)
			_, err := f.client.SubmitConcurrentInspectionStagedWithGate(t.Context(), permit, identity, launch, nil, func(InspectionObservation) error { t.Fatal("Drain reached inspection commit"); return nil })
			if !errors.Is(err, ErrPrivateUpgradePending) {
				t.Fatalf("inspection crossed logical Drain: %v", err)
			}
			assertUpgradeNoSubmissionCommands(t, f.fake)
		})
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
