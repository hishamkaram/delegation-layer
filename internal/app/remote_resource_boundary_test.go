package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app/remotecommands"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// These cases exercise the actual wire, admission, journal and CLI projection.
// The resident is a native component child. Its EntryHost accepts the test image
// and returns a controlled supervisor-preparation result. This proves native
// resident authentication and app projection, not actual Pueue readiness,
// installed managed-image eligibility or provider/SSH execution.
func TestRemoteResourcePressurePreservesUnknownAdmission(t *testing.T) {
	fixture := newRemoteResourceBoundary(t)
	ownRemoteResourceResident(t, fixture)
	resources := &recordingResources{t: t, assessments: []func() resourceadmission.Assessment{cpuPressureNow}}
	fixture.deps.ResourceChecker = resources
	reference, origin := resourceBoundaryReference(t, fixture)
	exchange := &resourceBoundaryExchange{server: remoteServerExchange{deps: fixture.deps}}
	coordinator := remote.Coordinator{Exchange: exchange}
	response, err := coordinator.Dispatch(t.Context(), reference)
	assertRemotePressureBoundary(t, fixture, response, err)
	reference = reopenResourceReference(t, origin, reference, fixture.intent.TaskID)
	_, err = coordinator.Dispatch(t.Context(), reference)
	if !errors.Is(err, remote.ErrAcceptanceUnknown) {
		t.Fatalf("restart lost uncertainty: %v", err)
	}
	_, err = coordinator.Reconcile(t.Context(), reference)
	if !errors.Is(err, remote.ErrAcceptanceUnknown) {
		t.Fatalf("explicit reconcile lost uncertainty: %v", err)
	}
	if want := []remote.Operation{remote.Admit, remote.Reconcile, remote.Reconcile}; !reflect.DeepEqual(exchange.operations, want) {
		t.Fatalf("operations=%v want%v", exchange.operations, want)
	}
	checks, _ := resources.calls()
	if checks != 1 {
		t.Fatalf("existing-only reconciliation sampled resources or retried admission: %d", checks)
	}
	assertResourceBoundaryAbsent(t, fixture, reference)
}

func TestRemoteMissingResidentPersistsDefiniteRejection(t *testing.T) {
	fixture := newRemoteResourceBoundary(t)
	reference, origin := resourceBoundaryReference(t, fixture)
	exchange := &resourceBoundaryExchange{server: remoteServerExchange{deps: fixture.deps}}
	coordinator := remote.Coordinator{Exchange: exchange}
	response, err := coordinator.Dispatch(t.Context(), reference)
	if !errors.Is(err, remote.ErrAdmissionRejected) || response.TaskRecord != "not_created" || response.Failure == nil || response.Failure.Code != "resident_not_ready" {
		t.Fatalf("missing resident was not definite refusal: %+v %v", response, err)
	}
	if code := remoteCLIResultCode("dispatch", remotecommands.Report{Remote: &response}, err); code != 1 {
		t.Fatalf("definite refusal exit%d", code)
	}
	reference = reopenResourceReference(t, origin, reference, fixture.intent.TaskID)
	for _, operation := range []func(context.Context, *remote.Reference) (remote.Response, error){coordinator.Dispatch, coordinator.Reconcile} {
		replay, replayErr := operation(t.Context(), reference)
		if !errors.Is(replayErr, remote.ErrAdmissionRejected) || replay.TaskRecord != "not_created" {
			t.Fatalf("durable rejection lost after reopen: %+v %v", replay, replayErr)
		}
	}
	if len(exchange.operations) != 1 {
		t.Fatalf("durably rejected reference contacted destination again: %v", exchange.operations)
	}
	assertResourceBoundaryAbsent(t, fixture, reference)
}

type resourceBoundaryExchange struct {
	server     remoteServerExchange
	operations []remote.Operation
}

func (e *resourceBoundaryExchange) Call(ctx context.Context, request remote.Request) (remote.Response, error) {
	e.operations = append(e.operations, request.Operation)
	return e.server.Call(ctx, request)
}

type remoteResourceBoundaryFixture struct {
	*remoteServerFixture
	provider *preflightFixture
	retain   bool
}

func newRemoteResourceBoundary(t *testing.T) *remoteResourceBoundaryFixture {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Keep the owner endpoint short, matching the native infrastructure fixtures.
	root, err := os.MkdirTemp(home, ".rr-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &remoteResourceBoundaryFixture{provider: &preflightFixture{}}
	t.Cleanup(func() {
		if fixture.retain {
			t.Logf("retained unresolved resident root %s", root)
			return
		}
		if removeErr := os.RemoveAll(root); removeErr != nil {
			t.Error(removeErr)
		}
	})
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	identity := remote.Identity{Host: "worker.example", Port: 22, User: "runner", HostKey: "SHA256:fixture", Root: root, RootID: store.RootID, RootDevice: pin.Device(), RootInode: pin.Inode()}
	brief := []byte("bounded resource boundary")
	intent := remote.Intent{Version: remote.ProtocolVersion, OriginRootID: strings.Repeat("a", 32), TaskID: strings.Repeat("d", 32), Device: "fixture", Executable: "delegate", Destination: identity, Provider: "fixture:preflight", RemoteCwd: t.TempDir(), Config: task.TaskConfig{Permission: "read-only", Budget: "1m"}, Brief: remote.Descriptor{Name: "brief", Length: int64(len(brief)), SHA256: task.ComputeSHA256(brief)}}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	fixture.remoteServerFixture = &remoteServerFixture{store: store, intent: intent, digest: digest, brief: brief, deps: Dependencies{ResourceChecker: fixtureResources, Catalog: preflightFixtureCatalog(t, fixture.provider.candidate)}}
	return fixture
}

func resourceBoundaryReference(t *testing.T, fixture *remoteResourceBoundaryFixture) (*remote.Reference, *taskdir.Store) {
	t.Helper()
	store, err := openStore(filepath.Join(t.TempDir(), "origin"), fixture.deps.storeDependencies(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	reference, err := remote.OpenReference(t.Context(), store, fixture.intent.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reference.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = reference.SaveIntent(t.Context(), fixture.intent, fixture.brief); err != nil {
		t.Fatal(err)
	}
	return reference, store
}

func reopenResourceReference(t *testing.T, store *taskdir.Store, prior *remote.Reference, id string) *remote.Reference {
	t.Helper()
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := remote.OpenReference(t.Context(), store, id, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	})
	return next
}

func assertRemotePressureBoundary(t *testing.T, fixture *remoteResourceBoundaryFixture, response remote.Response, err error) {
	t.Helper()
	if !errors.Is(err, remote.ErrAcceptanceUnknown) || response.TaskRecord != "unknown" || response.Existing || response.Identity != fixture.intent.Destination {
		t.Fatalf("pressure changed bound admission facts: %+v %v", response, err)
	}
	assertRemotePressureEnvelope(t, fixture, response)
	if code := remoteCLIResultCode("dispatch", remotecommands.Report{Remote: &response}, err); code != 3 {
		t.Fatalf("uncertain resource refusal exit%d", code)
	}
}

func assertRemotePressureEnvelope(t *testing.T, fixture *remoteResourceBoundaryFixture, response remote.Response) {
	t.Helper()
	envelope := remoteServerEnvelope(t, response)
	if envelope.TaskID != fixture.intent.TaskID || envelope.RootID != fixture.store.RootID || envelope.TaskRecord != "unknown" || envelope.Failure == nil || envelope.Failure.Code != "resource_pressure" || envelope.Failure.ReasonCode != "host_cpu_sustained" || envelope.Failure.NextAction != "stop_and_report" || envelope.ResourceAdmission == nil || envelope.ResourceAdmission.Status != "pressure" {
		t.Fatalf("resource refusal envelope lost: %+v", envelope)
	}
	assertRemotePressureDiagnostics(t, envelope.ResourceAdmission.Diagnostics)
}

func assertRemotePressureDiagnostics(t *testing.T, diagnostics *ResourceAdmissionDiagnostics) {
	t.Helper()
	if diagnostics == nil {
		t.Fatal("remote envelope lost CPU diagnostic evidence")
	}
	// Literal fixture facts form the oracle; do not derive expected output from
	// the presentation mapper. This proves transport, not native CPU sampling.
	want := &HostCPUDiagnosticsResponse{
		CounterUnit: "ticks", BusyThreshold: 0.95, RequiredStreak: 3,
		Intervals: []HostCPUIntervalResponse{
			{ElapsedMilliseconds: 1000, BusyDelta: cpuDiagnosticNumber(95), TotalDelta: cpuDiagnosticNumber(100), BusyFraction: cpuDiagnosticNumber(0.95), Classification: "hot", UnavailableReason: ""},
			{ElapsedMilliseconds: 1002, BusyDelta: cpuDiagnosticNumber(198), TotalDelta: cpuDiagnosticNumber(200), BusyFraction: cpuDiagnosticNumber(0.99), Classification: "hot", UnavailableReason: ""},
			{ElapsedMilliseconds: 1001, BusyDelta: cpuDiagnosticNumber(297), TotalDelta: cpuDiagnosticNumber(300), BusyFraction: cpuDiagnosticNumber(0.99), Classification: "hot", UnavailableReason: ""},
		},
	}
	if !reflect.DeepEqual(diagnostics.HostCPU, want) {
		t.Fatalf("remote CPU evidence changed: got%+v want%+v", diagnostics.HostCPU, want)
	}
}

func assertResourceBoundaryAbsent(t *testing.T, fixture *remoteResourceBoundaryFixture, reference *remote.Reference) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(fixture.store.Root, "tasks", fixture.intent.TaskID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal created task/submission/provider authority: %v", err)
	}
	if fixture.provider.prepareCalls != 0 || fixture.provider.finalizeCalls != 0 {
		t.Fatalf("refusal prepared provider work: prepare%d finalize%d", fixture.provider.prepareCalls, fixture.provider.finalizeCalls)
	}
	if _, err := reference.Accepted(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal recorded acceptance: %v", err)
	}
	intent, err := reference.Intent()
	if err != nil || !reflect.DeepEqual(intent, fixture.intent) {
		t.Fatalf("original reference changed: %+v %v", intent, err)
	}
}

// This existing EntryHost component port models completed supervisor preparation
// with a controlled WarmReuse result. It does not prove actual Pueue readiness.
// Native birth, lease, publication, authenticated IPC and stop remain real.
type resourceBoundaryComponentHost struct{}

func (resourceBoundaryComponentHost) ValidateEstablishment(_ context.Context, _ *taskdir.Store, initial infrastructure.InitialOperation) error {
	if initial.Kind == infrastructure.InitialRuntimeRestart {
		return errors.New("unexpected runtime restart establishment")
	}
	return nil
}

func (resourceBoundaryComponentHost) ValidateRunner(ctx context.Context, root, runner string, info *debug.BuildInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if root == "" || runner == "" || info == nil {
		return os.ErrInvalid
	}
	return nil
}

func (resourceBoundaryComponentHost) EnsureOwnRoot(context.Context, infrastructure.Operation, infrastructure.LaunchContext, infrastructure.ResidentGuards, infrastructure.DaemonWaitOwner) (infrastructure.Disposition, error) {
	return infrastructure.Disposition{Kind: infrastructure.DispositionWarmReuse}, nil
}

func (resourceBoundaryComponentHost) RecoverSaved(context.Context, infrastructure.Operation, infrastructure.LaunchContext, infrastructure.SavedBindingSource, infrastructure.ResidentGuards, infrastructure.DaemonWaitOwner) (infrastructure.Disposition, error) {
	return infrastructure.Disposition{}, errors.New("unexpected saved recovery")
}

func (resourceBoundaryComponentHost) PrepareContinuation(context.Context, infrastructure.Operation, infrastructure.LaunchContext, infrastructure.SavedBindingSource, infrastructure.ResidentGuards, infrastructure.DaemonWaitOwner) (infrastructure.Disposition, error) {
	return infrastructure.Disposition{}, errors.New("unexpected continuation")
}

func (resourceBoundaryComponentHost) MigrateOwnRoot(context.Context, infrastructure.Operation, infrastructure.LaunchContext, infrastructure.ResidentGuards, infrastructure.DaemonWaitOwner) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
	return infrastructure.Disposition{}, infrastructure.ManualMigrationReport{}, errors.New("unexpected migration")
}

func (resourceBoundaryComponentHost) PrepareRuntimeRestart(context.Context, infrastructure.Operation, infrastructure.LaunchContext, infrastructure.StartupReference, infrastructure.ResidentGuards, infrastructure.DaemonWaitOwner) (infrastructure.Disposition, infrastructure.RuntimeRestartReport, error) {
	return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, errors.New("unexpected runtime restart")
}

func (resourceBoundaryComponentHost) ObserveRuntimeRestartCustody(context.Context, infrastructure.Operation, infrastructure.LaunchContext, infrastructure.DaemonWaitOwner) (infrastructure.RuntimeRestartCustody, error) {
	return infrastructure.RuntimeRestartCustodyUnknown, errors.New("unexpected runtime restart preflight")
}

func (resourceBoundaryComponentHost) ClassifyPreparation(error) infrastructure.PreparationFailure {
	return infrastructure.PreparationFailure{}
}

func TestRemoteResourceResidentProcess(t *testing.T) {
	for index, arg := range os.Args {
		if arg != "--remote-resource-resident" {
			continue
		}
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		handled, err := infrastructure.RunOwnerMode(ctx, os.Args[index+1:], resourceBoundaryComponentHost{})
		if !handled || err != nil {
			t.Fatalf("resident child: handled%t %v", handled, err)
		}
		return
	}
}

func ownRemoteResourceResident(t *testing.T, fixture *remoteResourceBoundaryFixture) {
	t.Helper()
	runner := canonicalAppTestExecutable(t)
	intent := resourceResidentIntent(t, fixture.store, runner)
	reference, err := infrastructure.ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	lease, acquired, err := infrastructure.TryAcquireLease(ctx, filepath.Join(fixture.store.Root, ".infrastructure-owner.lock"))
	if err != nil || !acquired {
		t.Fatalf("resident election: %t %v", acquired, err)
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, fixture.store.Root, fixture.deps.storeDependencies().registry())
	if err != nil {
		t.Fatal(errors.Join(err, lease.Close()))
	}
	command := exec.Command(runner, "-test.run=^TestRemoteResourceResidentProcess$", "-test.timeout=30s", "--", "--remote-resource-resident", "--infrastructure-owner", "--root", fixture.store.Root, "--root-id", fixture.store.RootID, "--sequence", "1", "--epoch", reference.Epoch, "--intent-sha256", reference.IntentSHA256)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	done := make(chan error, 1)
	started := false
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cleanup, finish := context.WithTimeout(context.Background(), 30*time.Second)
		defer finish()
		if started {
			closeRemoteResourceResident(t, cleanup, fixture, reader, reference, done, &output)
		}
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = publishResourceResidentIntent(ctx, fixture.store, intent); err != nil {
		t.Fatal(err)
	}
	if err = lifetime.ConfigureIndependentSession(command); err != nil {
		t.Fatal(err)
	}
	fixture.retain = true
	err = lease.WithHeldFile(ctx, func(file *os.File) error { command.ExtraFiles = []*os.File{file}; return command.Start() })
	if err != nil {
		fixture.retain = false
		t.Fatal(err)
	}
	started = true
	go func() { done <- command.Wait() }()
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	awaitResourceResident(t, ctx, fixture)
}

func awaitResourceResident(t *testing.T, ctx context.Context, fixture *remoteResourceBoundaryFixture) {
	t.Helper()
	for {
		reader, err := (remoteLocalAdapter{deps: fixture.deps}).Read(ctx, fixture.store.Root, nil)
		if err == nil {
			process, processErr := infrastructure.ObserveOwnerProcess(ctx, reader)
			if processErr == nil {
				reference, referenceErr := process.Reference()
				processErr = referenceErr
				if processErr == nil {
					processErr = lifetime.RequirePersistentProcess(reference)
				}
			}
			err = errors.Join(processErr, reader.Close())
		}
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("resident did not become authenticated: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func publishResourceResidentIntent(ctx context.Context, store *taskdir.Store, intent infrastructure.StartupIntent) error {
	journal, err := store.OpenInfrastructureContext(ctx, true)
	if err != nil {
		return err
	}
	_, putErr := journal.PutContext(ctx, "intent-00000000000000000001.json", intent)
	return errors.Join(putErr, journal.Close())
}

func resourceResidentIntent(t *testing.T, store *taskdir.Store, runner string) infrastructure.StartupIntent {
	t.Helper()
	selected := infrastructure.LaunchContext{Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS, ControlEnvironment: []string{}, MaintenanceEnvironment: []string{}, SupervisorExecutable: filepath.Join(store.Root, "unused-pueue")}
	contextRef, err := infrastructure.StageLaunchContext(t.Context(), store, selected)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	inputs := infrastructure.StartupInputs{Kind: infrastructure.StartupRootOwner, Root: store.Root, RootID: store.RootID, BootID: boot, RootDevice: pin.Device(), RootInode: pin.Inode(), Artifacts: infrastructure.Artifacts{Runner: runner}, Context: contextRef}
	digest, err := inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	intent := infrastructure.StartupIntent{Protocol: infrastructure.RecordProtocolVersion, Sequence: 1, Epoch: strings.Repeat("b", 32), Inputs: inputs, InputsSHA256: digest, AcceptedAt: now.Format(time.RFC3339Nano), Deadline: now.Add(15 * time.Second).Format(time.RFC3339Nano)}
	if err = intent.Validate(); err != nil {
		t.Fatal(err)
	}
	return intent
}

func closeRemoteResourceResident(t *testing.T, ctx context.Context, fixture *remoteResourceBoundaryFixture, reader *taskdir.ReadOnlyStore, reference infrastructure.StartupReference, done <-chan error, output *bytes.Buffer) {
	t.Helper()
	if _, err := infrastructure.StopOwnerServingAt(ctx, reader, reference); err != nil {
		t.Errorf("stop owned resident: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("resident completion: %v: %s", err, output.String())
		}
	case <-ctx.Done():
		t.Errorf("resident completion unproven: %v", ctx.Err())
		return
	}
	closed, err := infrastructure.WaitOwnerProcessClosed(ctx, reader, reference)
	if err != nil || closed.ProcessClosure != infrastructure.OwnerProcessClosed {
		t.Errorf("resident closure unproven: %+v %v", closed, err)
		return
	}
	fixture.retain = false
}

func TestRemoteResourceGuardOverrideReachesDestinationGate(t *testing.T) {
	fixture := newRemoteResourceBoundary(t)
	ownRemoteResourceResident(t, fixture)
	fixture.deps.ResourceChecker = nil
	fixture.provider.prepareFault = errors.New("controlled provider preparation stop after resource gate")
	reference, _ := resourceBoundaryReference(t, fixture)
	exchange := &resourceBoundaryExchange{server: remoteServerExchange{deps: fixture.deps}}
	coordinator := remote.Coordinator{Exchange: exchange, ResourceGuards: resourceadmission.GuardPolicy{DisableCPU: true, DisableMemory: true}}
	response, err := coordinator.Dispatch(t.Context(), reference)
	if err == nil {
		t.Fatal("controlled provider preparation should fail")
	}
	envelope := remoteServerEnvelope(t, response)
	if envelope.ResourceAdmission == nil || envelope.ResourceAdmission.Status != "clear" || !reflect.DeepEqual(envelope.ResourceAdmission.DisabledGuards, []string{"cpu", "memory"}) || len(envelope.ResourceAdmission.Warnings) != 0 {
		t.Fatalf("destination lost invocation guard policy: %+v", envelope.ResourceAdmission)
	}
	if fixture.provider.prepareCalls != 1 || fixture.provider.finalizeCalls != 0 {
		t.Fatalf("gate did not reach controlled preparation: %d/%d", fixture.provider.prepareCalls, fixture.provider.finalizeCalls)
	}
	if !reflect.DeepEqual(exchange.operations, []remote.Operation{remote.Hello, remote.Admit}) {
		t.Fatalf("override protocol=%v", exchange.operations)
	}
}
