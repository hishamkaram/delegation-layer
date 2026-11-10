package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app/infrastructurehost"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestPreparePrivateAdmissionSupervisorRecoversPendingPublication(t *testing.T) {
	store, deps, original, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	base := filepath.Dir(original.Binding().ConfigPath)
	identityPath := filepath.Join(base, "supervisor.identity.json")
	pendingPath := filepath.Join(base, "identity.pending.json")
	identityBefore := readSupervisorProbeTestFile(t, identityPath)
	daemonPath := filepath.Join(base, "daemon.identity.json")
	daemonBefore := readSupervisorProbeTestFile(t, daemonPath)
	startLog := filepath.Join(base, "daemon-started")
	assertSupervisorProbeTestFile(t, startLog, []byte("started\n"))
	assertSupervisorProbeTestNoAdmissionAuthority(t, store)
	successor, pendingData := stageSupervisorProbeTestPublication(t, original.Binding(), deps.SupervisorOptions, identityBefore)

	probeContext, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	probed, err := supervisorcore.ProbePrivateAdmissionSupervisor(probeContext, store, successor.ClientExecutable, successor.DaemonExecutable, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, probed, err)
	if err != nil || probed != nil {
		t.Fatalf("app probe did not defer the pending publication to recovery: client=%v err=%v", probed, err)
	}
	assertSupervisorProbeTestFile(t, identityPath, identityBefore)
	assertSupervisorProbeTestFile(t, pendingPath, pendingData)
	assertSupervisorProbeTestFile(t, daemonPath, daemonBefore)
	assertSupervisorProbeTestFile(t, startLog, []byte("started\n"))

	// Exercise the application admission entrypoint. It must choose its own
	// exclusive fallback; the test does not call PreparePrivateWithLock.
	recovered, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, recovered, err)
	if err != nil || recovered == nil {
		t.Fatalf("app admission did not recover the pending publication: client=%v err=%v", recovered, err)
	}
	if got := recovered.Binding(); got != successor {
		t.Fatalf("admission returned the wrong recorded successor: got=%+v want=%+v", got, successor)
	}
	published := readSupervisorProbeTestPublication(t, readSupervisorProbeTestFile(t, identityPath))
	var binding task.SupervisorRef
	if decodeErr := task.DecodeStrict(published["binding"], &binding); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if binding != successor {
		t.Fatalf("admission did not publish the recorded successor: got=%+v want=%+v", binding, successor)
	}
	if _, statErr := os.Stat(pendingPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("pending publication survived admission recovery: %v", statErr)
	}
	if complete, migrationErr := pueue.PrivateSupervisorMigrationComplete(store.Root, successor); migrationErr != nil || !complete {
		t.Fatalf("recovered generation is still incomplete: complete=%t err=%v", complete, migrationErr)
	}
	assertSupervisorProbeTestDaemonMatchesBinding(t, daemonPath, successor)
	assertSupervisorProbeTestFile(t, startLog, []byte("started\n"))
	assertSupervisorProbeTestNoAdmissionAuthority(t, store)
}

func TestPreparePrivateAdmissionSupervisorElectsOneColdPolicyTransition(t *testing.T) {
	store, deps, original, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	base := filepath.Dir(original.Binding().ConfigPath)
	parallelLog := filepath.Join(base, "parallel")
	clearSupervisorProbeTestStatus(t, base)
	clearSupervisorProbeTestGroup(t, base)
	const callers = 24
	for _, client := range prepareSupervisorProbeAdmissions(t, store, deps, callers) {
		if client == nil {
			t.Fatal("concurrent private admission returned a nil supervisor")
		}
		if closeErr := client.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	assertSupervisorProbeTestNoStatus(t, base)
	assertSupervisorProbeTestGroupCount(t, base, 3)
	parallel, err := os.ReadFile(parallelLog)
	if err != nil {
		t.Fatalf("read scheduling command log: %v", err)
	}
	if count := bytes.Count(parallel, []byte("parallel\n")); count != 1 {
		t.Fatalf("parallel policy command count = %d, want 1; log=%q", count, parallel)
	}
	assertSupervisorProbeTestDaemonStartCount(t, base, 1)
	snapshot, err := original.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("observe configured private scheduling: %v", err)
	}
	configured, err := supervisorcore.OrdinarySchedulingConfiguredGroups(store.Root, original.Binding(), snapshot.Groups)
	if err != nil || !configured {
		t.Fatalf("ordinary scheduling receipt was not published: configured=%t err=%v", configured, err)
	}

	// A warm root only observes its already-published scheduling policy. It
	// must not reconfigure Pueue or launch another daemon.
	warm, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, warm, err)
	if err != nil {
		t.Fatalf("warm private admission failed: %v", err)
	}
	parallel, err = os.ReadFile(parallelLog)
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(parallel, []byte("parallel\n")); count != 1 {
		t.Fatalf("warm private admission reconfigured ordinary scheduling: count=%d log=%q", count, parallel)
	}
	assertSupervisorProbeTestGroupCount(t, base, 3)
	assertSupervisorProbeTestDaemonStartCount(t, base, 1)
}

func TestPreparePrivateAdmissionSupervisorWarmConcurrencyUsesNoControlCommands(t *testing.T) {
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	base := filepath.Dir(initial.Binding().ConfigPath)
	clearSupervisorProbeTestStatus(t, base)
	clearSupervisorProbeTestGroup(t, base)

	const callers = 64
	for _, client := range prepareSupervisorProbeAdmissions(t, store, deps, callers) {
		if client == nil {
			t.Fatal("concurrent warm admission returned a nil supervisor")
		}
		if closeErr := client.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	assertSupervisorProbeTestNoStatus(t, base)
	assertSupervisorProbeTestGroupCount(t, base, 0)
	assertSupervisorProbeTestDaemonStartCount(t, base, 1)
}

func TestPreparePrivateAdmissionSupervisorRepairsMissingTaskObservationLock(t *testing.T) {
	store, deps, original, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	base := filepath.Dir(original.Binding().ConfigPath)
	if err := os.Remove(filepath.Join(base, "task-observation.lock")); err != nil {
		t.Fatal(err)
	}
	if ready, err := pueue.PrivateTaskObservationReady(store.Root); err != nil || ready {
		t.Fatalf("missing task observation lock reported ready: ready=%t err=%v", ready, err)
	}
	clearSupervisorProbeTestStatus(t, base)
	clearSupervisorProbeTestGroup(t, base)

	repaired, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, repaired, err)
	if err != nil || repaired == nil {
		t.Fatalf("admission did not repair task observation coordination: client=%v err=%v", repaired, err)
	}
	if ready, readyErr := pueue.PrivateTaskObservationReady(store.Root); readyErr != nil || !ready {
		t.Fatalf("task observation coordination remains unavailable: ready=%t err=%v", ready, readyErr)
	}
	assertSupervisorProbeTestNoStatus(t, base)
	assertSupervisorProbeTestDaemonStartCount(t, base, 1)
}

func TestPreparePrivateAdmissionSupervisorRepairsStaleSchedulingWithoutQueueScan(t *testing.T) {
	store, deps, original, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	base := filepath.Dir(original.Binding().ConfigPath)
	parallelLog := filepath.Join(base, "parallel")
	clearSupervisorProbeTestStatus(t, base)
	clearSupervisorProbeTestGroup(t, base)

	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	staleSupervisorProbeTestOrdinaryScheduling(t, store.Root)
	if recorded, receiptErr := supervisorcore.OrdinarySchedulingRecorded(store.Root, original.Binding()); receiptErr != nil || recorded {
		t.Fatalf("stale scheduling receipt = %t, %v; want false, nil", recorded, receiptErr)
	}
	err = os.WriteFile(filepath.Join(base, "reject-status"), nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	const callers = 64
	for _, client := range prepareSupervisorProbeAdmissions(t, store, deps, callers) {
		if client == nil {
			t.Fatal("concurrent stale-scheduling repair returned a nil supervisor")
		}
		if closeErr := client.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	assertSupervisorProbeTestNoStatus(t, base)
	assertSupervisorProbeTestGroupCount(t, base, 6)
	parallel, err := os.ReadFile(parallelLog)
	if err != nil {
		t.Fatalf("read stale-scheduling command log: %v", err)
	}
	if count := bytes.Count(parallel, []byte("parallel\n")); count != 2 {
		t.Fatalf("stale scheduling policy command count = %d, want 2; log=%q", count, parallel)
	}
	assertSupervisorProbeTestDaemonStartCount(t, base, 1)
}

func prepareSupervisorProbeAdmissions(t *testing.T, store *taskdir.Store, deps Dependencies, callers int) []*pueue.Client {
	t.Helper()
	start := make(chan struct{})
	errs := make(chan error, callers)
	clients := make(chan *pueue.Client, callers)
	var calls sync.WaitGroup
	for range callers {
		calls.Add(1)
		go func() {
			defer calls.Done()
			<-start
			client, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
			if err == nil {
				clients <- client
			}
			errs <- err
		}()
	}
	close(start)
	calls.Wait()
	close(errs)
	close(clients)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent private admission failed: %v", err)
		}
	}
	result := make([]*pueue.Client, 0, callers)
	for client := range clients {
		result = append(result, client)
	}
	return result
}

func TestPrepareAdmissionSupervisorConfiguresContinuationScheduling(t *testing.T) {
	store, deps, original := newSupervisorProbeTestFixture(t)
	base := filepath.Dir(original.Binding().ConfigPath)
	parallelLog := filepath.Join(base, "parallel")
	saved := original.Binding()

	continuation, err := supervisorcore.PrepareAdmissionSupervisorContext(t.Context(), supervisorAdmissionSelection(Arguments{savedSupervisor: &saved}, deps.withCapturedEnvironment()), supervisorAdmissionOptions(deps, deps.SupervisorOptions), store, false, "", true)
	settleSupervisorProbeTestClient(t, continuation, err)
	if err != nil || continuation == nil {
		t.Fatalf("continuation admission did not configure private scheduling: client=%v err=%v", continuation, err)
	}
	parallel, err := os.ReadFile(parallelLog)
	if err != nil {
		t.Fatalf("read continuation scheduling log: %v", err)
	}
	if count := bytes.Count(parallel, []byte("parallel\n")); count != 1 {
		t.Fatalf("continuation policy command count = %d, want 1; log=%q", count, parallel)
	}
	snapshot, err := continuation.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("observe continuation supervisor: %v", err)
	}
	configured, err := supervisorcore.OrdinarySchedulingConfiguredGroups(store.Root, continuation.RuntimeBinding(), snapshot.Groups)
	if err != nil || !configured {
		t.Fatalf("continuation did not publish ordinary scheduling receipt: configured=%t err=%v", configured, err)
	}

	warm, err := supervisorcore.PrepareAdmissionSupervisorContext(t.Context(), supervisorAdmissionSelection(Arguments{savedSupervisor: &saved}, deps.withCapturedEnvironment()), supervisorAdmissionOptions(deps, deps.SupervisorOptions), store, false, "", true)
	settleSupervisorProbeTestClient(t, warm, err)
	if err != nil || warm == nil {
		t.Fatalf("warm continuation admission failed: client=%v err=%v", warm, err)
	}
	parallel, err = os.ReadFile(parallelLog)
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(parallel, []byte("parallel\n")); count != 1 {
		t.Fatalf("warm continuation reconfigured scheduling: count=%d log=%q", count, parallel)
	}
}

func TestEnsurePrivateQueuedRunnerRepairElectsOneQueueScan(t *testing.T) {
	store, _, supervisor := newSupervisorProbeTestFixture(t)
	base := filepath.Dir(supervisor.Binding().ConfigPath)
	statusLog := filepath.Join(base, "status")
	if err := os.WriteFile(statusLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, runner, "runner")

	const callers = 24
	start := make(chan struct{})
	errs := make(chan error, callers)
	var calls sync.WaitGroup
	for range callers {
		calls.Add(1)
		go func() {
			defer calls.Done()
			<-start
			errs <- supervisorcore.EnsurePrivateQueuedRunnerRepair(store, supervisor, runner)
		}()
	}
	close(start)
	calls.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent runner repair failed: %v", err)
		}
	}
	status, err := os.ReadFile(statusLog)
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(status, []byte("status\n")); count != 1 {
		t.Fatalf("queue repair status scan count = %d, want 1; log=%q", count, status)
	}
	if err = supervisorcore.EnsurePrivateQueuedRunnerRepair(store, supervisor, runner); err != nil {
		t.Fatalf("warm runner repair failed: %v", err)
	}
	status, err = os.ReadFile(statusLog)
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(status, []byte("status\n")); count != 1 {
		t.Fatalf("warm runner repair repeated queue scan: count=%d log=%q", count, status)
	}
}

func TestRepairPrivateQueueForTaskElectsOneQueueScanAcrossCallers(t *testing.T) {
	store, deps, supervisor, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	if err := supervisorcore.EnsureAdmissionScheduling(store, supervisor, deps.SupervisorOptions); err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(supervisor.Binding().ConfigPath)
	statusLog := filepath.Join(base, "status")
	if err := os.WriteFile(statusLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, runner, "runner")
	brief := []byte("queue repair source")
	request := newHistoricalSupervisorTaskRecord(store.RootID, testsupport.CanonicalTempDir(t), brief)
	profile := appTestExecutionProfile(&request, runner, nil)
	meta := tasklifecycle.NewMeta(request, profile, supervisor.Binding(), runner, task.RunnerOwnershipManaged, "queue-repair-test")
	td, err := store.CreateTask(request.TaskID, &request, brief, &meta)
	owner.ownTask(td)
	if err != nil {
		t.Fatal(err)
	}
	deps.RunnerExecutable = runner
	options := supervisorAdmissionOptions(deps, deps.SupervisorOptions)

	const callers = 24
	start := make(chan struct{})
	errs := make(chan error, callers)
	var calls sync.WaitGroup
	for range callers {
		calls.Add(1)
		go func() {
			defer calls.Done()
			<-start
			errs <- supervisorcore.RepairQueueForTask(t.Context(), store, supervisor.Binding(), &meta, options)
		}()
	}
	close(start)
	calls.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent task runner repair failed: %v", err)
		}
	}
	status, err := os.ReadFile(statusLog)
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(status, []byte("status\n")); count != 1 {
		t.Fatalf("task runner repair status scan count = %d, want 1; log=%q", count, status)
	}
}

func TestRepairPrivateQueueForTaskRefusesUnconnectedWarmBindingBeforeMaintenance(t *testing.T) {
	store, deps, supervisor, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	if err := supervisorcore.EnsureAdmissionScheduling(store, supervisor, deps.SupervisorOptions); err != nil {
		t.Fatal(err)
	}
	warm, err := supervisorcore.ProbePrivateAdmissionSupervisor(t.Context(), store, "", "", deps.SupervisorOptions)
	owner.ownClient(warm, err)
	if closeErr := supervisorcore.ClosePrivateAdmissionTransition(warm, err); closeErr != nil || warm == nil {
		t.Fatalf("fixture is not a ready admission generation: client=%v err=%v", warm != nil, closeErr)
	}
	runner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, runner, "runner")
	brief := []byte("unconnected queue repair source")
	request := newHistoricalSupervisorTaskRecord(store.RootID, testsupport.CanonicalTempDir(t), brief)
	profile := appTestExecutionProfile(&request, runner, nil)
	saved := supervisor.Binding()
	saved.ConfigDigest = task.ComputeSHA256([]byte("unconnected historical configuration"))
	meta := tasklifecycle.NewMeta(request, profile, saved, runner, task.RunnerOwnershipManaged, "queue-repair-test")
	td, err := store.CreateTask(request.TaskID, &request, brief, &meta)
	owner.ownTask(td)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(supervisor.Binding().ConfigPath)
	clearActiveGenerationSupervisorCommands(t, base)
	socketPath := strings.TrimPrefix(supervisor.Binding().Endpoint, "unix:")
	socket, err := os.Lstat(socketPath)
	if err != nil || socket.Mode()&os.ModeSocket == 0 {
		t.Fatalf("fixture endpoint is not a socket: info=%v err=%v", socket, err)
	}
	before := storeRecordDigests(t, store.Root)
	deps.RunnerExecutable = runner
	deps.SupervisorOptions.Observer = func(event pueue.CommandEvent) {
		if event.Stage == "entry" {
			t.Error("unconnected warm binding launched a supervisor command before refusal")
		}
	}
	err = supervisorcore.RepairQueueForTask(t.Context(), store, meta.SupervisorConfig, &meta, supervisorAdmissionOptions(deps, deps.SupervisorOptions))
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) || !errors.Is(err, pueue.ErrBinding) {
		t.Errorf("unconnected warm binding did not require migration: %v", err)
	}
	assertActiveGenerationNoSupervisorCommands(t, base)
	assertSupervisorProbeTestDaemonStartCount(t, base, 1)
	if _, err = os.Lstat(filepath.Join(store.Root, "runner-repair")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unconnected warm binding published runner repair state: %v", err)
	}
	requireUnchangedRecords(t, "unconnected warm binding", store.Root, before)
	after, err := os.Lstat(socketPath)
	if err != nil || !os.SameFile(socket, after) || socket.Mode() != after.Mode() || !socket.ModTime().Equal(after.ModTime()) {
		t.Fatalf("unconnected warm binding changed its socket: before=%v after=%v err=%v", socket, after, err)
	}
}

func newSupervisorProbeTestFixture(t *testing.T) (*taskdir.Store, Dependencies, *pueue.Client) {
	t.Helper()
	store, deps, client, _ := newOwnedSupervisorProbeTestFixture(t)
	return store, deps, client
}

// Direct preparation fixtures receive their actual owner separately. Public
// command dependencies remain free of lifecycle grants and implicit routing.
func newOwnedSupervisorProbeTestFixture(t *testing.T) (*taskdir.Store, Dependencies, *pueue.Client, *componentSupervisorFixture) {
	t.Helper()
	store, deps, owner := newColdSupervisorProbeTestFixture(t)
	// Preserve the owned namespace in failure output before the first Start.
	t.Logf("supervisor probe fixture root=%s", store.Root)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := owner.bindPrivate(ctx, deps.InitialSupervisorExecutable, filepath.Join(filepath.Dir(deps.InitialSupervisorExecutable), "pueued"), deps.SupervisorOptions)
	if err != nil {
		t.Fatalf("bind healthy private supervisor fixture: %v", err)
	}
	owner.listen(t, client.Binding().Endpoint)
	return store, deps, client, owner
}

func newColdSupervisorProbeTestFixture(t *testing.T) (*taskdir.Store, Dependencies, *componentSupervisorFixture) {
	t.Helper()
	owner := newComponentSupervisorFixture(t, componentFiniteDaemon)
	root := owner.root
	store, err := taskdir.InitStore(root)
	owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(owner.base, "bundle")
	clientPath := filepath.Join(bundle, "pueue")
	daemonPath := filepath.Join(bundle, "pueued")
	writeSupervisorProbeExecutables(t, store.RootID, clientPath, daemonPath)
	options := pueue.Options{ObservationTimeout: 10 * time.Second, WaitForPendingClose: true}
	t.Logf("cold supervisor probe fixture root=%s", root)
	deps := Dependencies{ResourceChecker: fixtureResources, InitialSupervisorExecutable: clientPath, SupervisorOptions: options}
	return store, deps, owner
}

func withSupervisorProbeRouting(t *testing.T, deps Dependencies, owner *componentSupervisorFixture) Dependencies {
	t.Helper()
	deps.ownerRouting = &componentPreparationRouting{test: t, owner: owner, client: deps.InitialSupervisorExecutable, options: deps.SupervisorOptions}
	return deps
}

// Journal setup can borrow only this root's publication guard. It cannot Start
// a daemon or claim Wait ownership; actual Starts use the component's setup.
func componentPublicationOptions(t *testing.T, root string, owner *componentSupervisorFixture, options pueue.Options) pueue.Options {
	t.Helper()
	if owner == nil || root != owner.root {
		t.Fatal("publication setup requires the exact component root owner")
	}
	options.DaemonStartGuard = nil
	options.DaemonWaitOwner = nil
	options.PrivateGenerationGuard = owner
	options.WaitForPendingClose = true
	return options
}

// This composes real preparation with the existing component owner.
// Native placement needs separate proof.
type componentPreparationRouting struct {
	test    *testing.T
	owner   *componentSupervisorFixture
	client  string
	options pueue.Options
}

func TestInfrastructureRoutingDependencyPreservesCommandChoice(t *testing.T) {
	route := &componentPreparationRouting{}
	deps := Dependencies{ownerRouting: route, Environment: []string{}}
	deps = deps.withCapturedEnvironmentContext(t.Context()).normalized()
	storeDeps := deps.storeDependencies()
	if deps.infrastructureRouting() != route || storeDeps.infrastructureRouting() != route {
		t.Fatal("command capture, normalization or store narrowing replaced the selected route")
	}
	deps.ownerRouting = nil
	if storeDeps.infrastructureRouting() != route {
		t.Fatal("changing the command copy replaced the held store dependency choice")
	}
	for _, selected := range []supervisorcore.OwnerRouting{deps.infrastructureRouting(), deps.storeDependencies().infrastructureRouting()} {
		if selected != infrastructurehost.NewRouting(deps.invocationSource, deps.RunnerExecutable) {
			t.Fatal("nil routing dependency lost native invocation routing")
		}
	}
}

func (r *componentPreparationRouting) preparation(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext) (installation.ExecutableLookup, pueue.Options, *infrastructure.PreparationObservation, error) {
	if ctx == nil || store == nil || r.owner == nil {
		return installation.ExecutableLookup{}, pueue.Options{}, nil, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return installation.ExecutableLookup{}, pueue.Options{}, nil, err
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return installation.ExecutableLookup{}, pueue.Options{}, nil, err
	}
	if pin.Root() != r.owner.root || pin.RootID() != r.owner.store.RootID || selected.SupervisorExecutable != r.client || selected.OS != runtime.GOOS {
		return installation.ExecutableLookup{}, pueue.Options{}, nil, infrastructure.ErrIdentity
	}
	digest, err := selected.Digest()
	reference := infrastructure.LaunchContextReference{SHA256: digest}
	if err != nil {
		return installation.ExecutableLookup{}, pueue.Options{}, nil, err
	}
	if err = reference.ValidateContext(selected, pin.Root(), pin.RootID()); err != nil {
		return installation.ExecutableLookup{}, pueue.Options{}, nil, err
	}
	observation, err := infrastructure.NewPreparationObservation(r.owner, reference)
	if err != nil {
		return installation.ExecutableLookup{}, pueue.Options{}, nil, err
	}
	options := r.owner.setupOptions(supervisorcore.OptionsForEnvironment(r.options, selected.ControlEnvironment))
	options.DaemonStartGuard = observation
	lookup := installation.ExecutableLookup{InitialExecutable: r.client, Environment: []string{}, AcceptedBootstrapPair: installation.BootstrapPair{Client: r.client, Daemon: filepath.Join(filepath.Dir(r.client), "pueued")}}
	return lookup, options, observation, nil
}

func (r *componentPreparationRouting) EnsurePrivate(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, error) {
	lookup, options, observation, err := r.preparation(ctx, store, selected)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	before := len(r.owner.daemonWaits())
	client, err := supervisorcore.PreparePrivateAdmissionSupervisorContext(ctx, lookup, store, options)
	r.owner.ownClient(client, err)
	r.owner.ownDaemonClients(client, before)
	r.recordReady(observation, client, err)
	return observation.Disposition(), r.closePreparationClient(client, before, err)
}

func (r *componentPreparationRouting) RecoverSaved(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	lookup, options, observation, err := r.preparation(ctx, store, selected)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	saved, err := r.savedBinding(ctx, store, source)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	before := len(r.owner.daemonWaits())
	client, err := supervisorcore.RecoverPrivateClient(ctx, store, saved, options, lookup)
	r.owner.ownClient(client, err)
	r.owner.ownDaemonClients(client, before)
	r.recordReady(observation, client, err)
	return observation.Disposition(), r.closePreparationClient(client, before, err)
}

func (r *componentPreparationRouting) PrepareContinuation(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (disposition infrastructure.Disposition, resultErr error) {
	lookup, control, observation, err := r.preparation(ctx, store, selected)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	firstWait := len(r.owner.daemonWaits())
	var client *pueue.Client
	defer func() {
		r.owner.ownClient(client, resultErr)
		r.owner.ownDaemonClients(client, firstWait)
		r.recordReady(observation, client, resultErr)
		resultErr = r.closePreparationClient(client, firstWait, resultErr)
		disposition = observation.Disposition()
	}()
	original, err := supervisorcore.ReadTaskMetadataSupervisor(ctx, store, source)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	options := supervisorcore.ContinuationOptions{
		Options: control, InitialExecutable: lookup.InitialExecutable,
		Environment: lookup.Environment, OpenStore: openNativeSupervisorStore,
	}
	successor, err := supervisorcore.RebindContinuation(ctx, store.Root, original, options)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	successor, err = supervisorcore.PrepareContinuationSuccessor(ctx, store.Root, successor, options)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	client, resultErr = supervisorcore.RecoverPrivateClient(ctx, store, successor, control, lookup)
	return infrastructure.Disposition{}, resultErr
}

func (r *componentPreparationRouting) MigrateOwn(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
	lookup, options, observation, err := r.preparation(ctx, store, selected)
	if err != nil {
		return infrastructure.Disposition{}, infrastructure.ManualMigrationReport{}, err
	}
	before := len(r.owner.daemonWaits())
	state, migrationErr := inspectSupervisorMigration(ctx, store.Root, lookup, options, store)
	r.owner.ownClient(state.current, migrationErr)
	r.owner.ownDaemonClients(state.current, before)
	result := manualSupervisorMigrationResult(state, migrationErr)
	report, reportErr := infrastructureManualMigrationReport(result)
	// Finite daemon fixtures need their owned endpoint before the canonical
	// readiness probe; endpoint existence alone never records Ready.
	r.establishPreparationEndpoint(observation, state.current, result.err)
	preparationBefore := len(r.owner.daemonWaits())
	var prepared *pueue.Client
	var preparationErr error
	if result.err == nil && state.current != nil {
		prepared, preparationErr = supervisorcore.PreparePrivateAdmissionSupervisorContext(ctx, lookup, store, options)
	}
	r.owner.ownClient(prepared, preparationErr)
	r.owner.ownDaemonClients(prepared, preparationBefore)
	preparationErr = r.closePreparationClient(prepared, preparationBefore, preparationErr)
	if result.err == nil && reportErr == nil && preparationErr == nil && state.current != nil {
		observation.RecordReady()
	}
	return observation.Disposition(), report, errors.Join(r.closePreparationClient(state.current, before, result.err), reportErr, preparationErr)
}

func (r *componentPreparationRouting) closePreparationClient(client *pueue.Client, firstWait int, cause error) error {
	if client != nil && r.owner.lifetime == componentPersistentDaemon && len(r.owner.daemonWaits()) > firstWait {
		// The existing component owner needs this client for supported Shutdown.
		// It closes the client only after the actual registered Wait completes.
		return cause
	}
	return supervisorcore.ClosePrivateAdmissionTransition(client, cause)
}

func (r *componentPreparationRouting) recordReady(observation *infrastructure.PreparationObservation, client *pueue.Client, cause error) {
	if cause != nil || client == nil {
		return
	}
	r.establishPreparationEndpoint(observation, client, cause)
	observation.RecordReady()
}

func (r *componentPreparationRouting) establishPreparationEndpoint(observation *infrastructure.PreparationObservation, client *pueue.Client, cause error) {
	if cause != nil || client == nil {
		return
	}
	if observation.Disposition().Kind == infrastructure.DispositionStarted && r.owner.lifetime == componentFiniteDaemon {
		// The authored finite CLI proves protocol readiness; this separately
		// owned listener supplies the component's subsequent endpoint probe.
		r.owner.listen(r.test, client.Binding().Endpoint)
	}
}

func listenSupervisorProbeTestEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	admissionfixture.ListenSupervisorProbeTestEndpoint(t, endpoint)
}

func stageSupervisorProbeTestPublication(t *testing.T, saved task.SupervisorRef, options pueue.Options, publishedData []byte) (task.SupervisorRef, []byte) {
	t.Helper()
	config := readSupervisorProbeTestFile(t, saved.ConfigPath)
	config = append(config, []byte("\n# interrupted comment-only identity publication\n")...)
	if err := os.WriteFile(saved.ConfigPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	current, err := pueue.Bind(ctx, saved.ClientExecutable, saved.ConfigPath, options)
	settleSupervisorProbeTestClient(t, current, err)
	if err != nil {
		t.Fatalf("resolve comment-only config successor: %v", err)
	}
	observed := current.Binding()
	if observed.ConfigDigest == saved.ConfigDigest || observed.ResolvedConfigSHA256 != saved.ResolvedConfigSHA256 || observed.Endpoint != saved.Endpoint {
		t.Fatalf("comment-only fixture changed endpoint semantics: saved=%+v current=%+v", saved, observed)
	}
	successor := saved
	successor.ConfigDigest = observed.ConfigDigest
	// The pending journal has the same envelope as the production-published
	// identity. Reuse that record, replacing only its observed config binding.
	publication := readSupervisorProbeTestPublication(t, publishedData)
	publication["binding"], err = task.MarshalCanonical(successor)
	if err != nil {
		t.Fatal(err)
	}
	pendingData, err := task.MarshalCanonical(publication)
	if err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(filepath.Dir(saved.ConfigPath), "identity.pending.json")
	if writeErr := os.WriteFile(pendingPath, pendingData, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	root := filepath.Dir(filepath.Dir(saved.ConfigPath))
	if complete, migrationErr := pueue.PrivateSupervisorMigrationComplete(root, saved); migrationErr != nil || complete {
		t.Fatalf("fixture is not a valid unfinished identity publication: complete=%t err=%v", complete, migrationErr)
	}
	return successor, pendingData
}

func readSupervisorProbeTestPublication(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var publication map[string]json.RawMessage
	if err := task.DecodeStrict(data, &publication); err != nil {
		t.Fatal(err)
	}
	return publication
}

func readSupervisorProbeTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertSupervisorProbeTestDaemonMatchesBinding(t *testing.T, path string, want task.SupervisorRef) {
	t.Helper()
	var got historicalPrivateDaemonIdentity
	if err := task.DecodeStrict(readSupervisorProbeTestFile(t, path), &got); err != nil {
		t.Fatal(err)
	}
	if got != historicalPrivateDaemonIdentityFor(want) {
		t.Fatalf("daemon identity = %+v, want binding %+v", got, want)
	}
}

func assertSupervisorProbeTestFile(t *testing.T, path string, want []byte) {
	t.Helper()
	if got := readSupervisorProbeTestFile(t, path); !bytes.Equal(got, want) {
		t.Fatalf("supervisor file changed: path=%s got=%q want=%q", path, got, want)
	}
}

func assertSupervisorProbeTestDaemonStartCount(t *testing.T, base string, want int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(base, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(data, []byte("started\n")); got != want {
		t.Fatalf("private daemon start count = %d, want %d; log=%q", got, want, data)
	}
}

func assertSupervisorProbeTestNoStatus(t *testing.T, base string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(base, "status"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("private admission performed a queue-wide status scan: %q", data)
	}
}

func clearSupervisorProbeTestStatus(t *testing.T, base string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(base, "status"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func clearSupervisorProbeTestGroup(t *testing.T, base string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(base, "group"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertSupervisorProbeTestGroupCount(t *testing.T, base string, want int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(base, "group"))
	if errors.Is(err, os.ErrNotExist) && want == 0 {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(data, []byte("group\n")); got != want {
		t.Fatalf("private admission group command count = %d, want %d; log=%q", got, want, data)
	}
}

func staleSupervisorProbeTestOrdinaryScheduling(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, ".supervisor", "ordinary-scheduling.json")
	data := readSupervisorProbeTestFile(t, path)
	var record map[string]json.RawMessage
	if err := task.DecodeStrict(data, &record); err != nil {
		t.Fatal(err)
	}
	var supervisor task.SupervisorRef
	if err := task.DecodeStrict(record["supervisor"], &supervisor); err != nil {
		t.Fatal(err)
	}
	supervisor.DaemonExecutable = filepath.Join(filepath.Dir(supervisor.DaemonExecutable), "previous-pueued")
	if err := task.ValidateFreshSupervisorRef(supervisor); err != nil {
		t.Fatal(err)
	}
	var err error
	record["supervisor"], err = task.MarshalCanonical(supervisor)
	if err != nil {
		t.Fatal(err)
	}
	data, err = task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertSupervisorProbeTestNoAdmissionAuthority(t *testing.T, store *taskdir.Store) {
	t.Helper()
	if _, _, err := inspection.ReadGroupHead(t.Context(), store); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("supervisor preparation created inspection authority: %v", err)
	}
	assertAppInspectionNoOrdinaryTasks(t, store)
}

func settleSupervisorProbeTestClient(t *testing.T, client *pueue.Client, err error) {
	t.Helper()
	admissionfixture.SettleSupervisorProbeTestClient(t, client, err)
}

// Fixture scripts are source authored in their selected installation, never
// copied or fingerprinted from an installed executable.
func writeSupervisorProbeExecutables(t *testing.T, rootID, clientPath, daemonPath string) {
	t.Helper()
	writeRunnerFixture(t, clientPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status)
    [ -f "$base/ready" ] || exit 1
	[ ! -f "$base/reject-status" ] || exit 97
	printf '%s\n' status >> "$base/status"
    parallel=1
    [ -f "$base/parallel-unlimited" ] && parallel=0
    printf '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":%s},"delegation-inspection-`+rootID+`":{"status":"Running","parallel_tasks":1}}}\n' "$parallel"
    ;;
	group)
	  [ -f "$base/ready" ] || exit 1
	  printf '%s\n' group >> "$base/group"
	  parallel=1
	  [ -f "$base/parallel-unlimited" ] && parallel=0
	  printf '{"default":{"status":"Running","parallel_tasks":%s},"delegation-inspection-`+rootID+`":{"status":"Running","parallel_tasks":1}}\n' "$parallel"
	  ;;
	parallel)
	  printf '%s\n' parallel >> "$base/parallel"
	  : > "$base/parallel-unlimited"
	  ;;
	add)
	  [ "${4-}" = --help ] || exit 64
	  printf '%s\n' add-help >> "$base/staged-capability"
	  printf '%s\n' '--stashed --print-task-id'
	  ;;
	enqueue)
	  [ "${4-}" = --help ] || exit 64
	  printf '%s\n' enqueue-help >> "$base/staged-capability"
	  printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'
	  ;;
  *) exit 64 ;;
esac
`)
	writeSupervisorProbeDaemon(t, daemonPath)
}

// Render original fixture source into its selected installation. The shared
// producer never obtains program bytes from an installed executable.
func writeSupervisorProbeDaemon(t *testing.T, path string) {
	t.Helper()
	writeRunnerFixture(t, path, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
printf '%s\n' started >> "$base/daemon-started"
mkdir -p "$base/run"
printf '%s\n' "$$" > "$base/run/pueue.pid"
: > "$base/ready"
`)
}

// The component owns explicit fixture readers; production source validation
// remains covered in infrastructurehost's saved-binding tests.
func (r *componentPreparationRouting) savedBinding(ctx context.Context, store *taskdir.Store, source infrastructure.SavedBindingSource) (binding task.SupervisorRef, resultErr error) {
	if err := source.Validate(); err != nil {
		return binding, err
	}
	if source.RootID != store.RootID {
		return binding, infrastructure.ErrIdentity
	}
	if source.Kind == infrastructure.SavedBindingTaskMetadata {
		return supervisorcore.ReadTaskMetadataSupervisor(ctx, store, source)
	}
	if source.Kind == infrastructure.SavedBindingWorkspaceRequest {
		operation, err := workspace.LoadOperation(ctx, store, source.OwnerID)
		if err != nil {
			return binding, err
		}
		defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
		binding = operation.Request().Supervisor
	} else {
		td, err := store.OpenTask(source.OwnerID)
		if err != nil {
			return binding, err
		}
		defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
		switch source.Kind {
		case infrastructure.SavedBindingTaskSubmission:
			submission, err := td.ReadSubmission()
			if err != nil {
				return binding, err
			}
			binding = submission.Supervisor
		case infrastructure.SavedBindingTaskStop:
			request, err := td.ReadStopRequest(source.RecordID)
			if err != nil {
				return binding, err
			}
			binding = request.Supervisor
		case infrastructure.SavedBindingTaskMetadata, infrastructure.SavedBindingWorkspaceRequest:
			return binding, infrastructure.ErrIdentity
		default:
			return binding, infrastructure.ErrIdentity
		}
	}
	return binding, source.ValidateBinding(binding)
}
