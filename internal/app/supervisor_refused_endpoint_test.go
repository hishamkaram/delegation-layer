package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// refusedEndpointAppFixture is a production-published private root whose
// fixture daemon exited naturally, leaving its recorded PID absent and a
// refused socket inode at the exact endpoint.
type refusedEndpointAppFixture struct {
	root       string
	store      *taskdir.Store
	deps       Dependencies
	published  task.SupervisorRef
	base       string
	socketPath string
	socket     os.FileInfo
}

func newRefusedEndpointAppFixture(t *testing.T) refusedEndpointAppFixture {
	t.Helper()
	store, deps, original, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	fixture := refusePrivateAppEndpoint(t, store.Root, original.Binding())
	fixture.store, fixture.deps = store, deps
	return fixture
}

// refusePrivateAppEndpoint replaces the published private generation's
// endpoint with a refused socket inode and removes its readiness marker.
func refusePrivateAppEndpoint(t *testing.T, root string, published task.SupervisorRef) refusedEndpointAppFixture {
	t.Helper()
	fixture := refusedEndpointAppFixture{
		root:       root,
		published:  published,
		base:       filepath.Dir(published.ConfigPath),
		socketPath: strings.TrimPrefix(published.Endpoint, "unix:"),
	}
	// The fixture listener keeps its unlinked inode until cleanup; the path
	// now names only a refused socket left behind by the stopped daemon.
	for _, path := range []string{fixture.socketPath, filepath.Join(fixture.base, "ready")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("unix", fixture.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener type = %T, want *net.UnixListener", listener)
	}
	unixListener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	if fixture.socket, err = os.Lstat(fixture.socketPath); err != nil {
		t.Fatal(err)
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	connection, dialErr := dialer.DialContext(t.Context(), "unix", fixture.socketPath)
	if connection != nil {
		if closeErr := connection.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Fatalf("closed fixture socket is not a proven refusal: %v", dialErr)
	}
	return fixture
}

func (fixture refusedEndpointAppFixture) assertSocketPreserved(t *testing.T) {
	t.Helper()
	info, err := os.Lstat(fixture.socketPath)
	if err != nil || !os.SameFile(info, fixture.socket) {
		t.Fatalf("refused endpoint inode changed: info=%v err=%v", info, err)
	}
}

func (fixture refusedEndpointAppFixture) assertRepaired(t *testing.T, starts int) {
	t.Helper()
	info, err := os.Lstat(fixture.socketPath)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSocket == 0 {
			t.Fatalf("repair replaced the supervisor endpoint with a non-socket: %v", info)
		}
		dialer := net.Dialer{Timeout: 10 * time.Second}
		connection, dialErr := dialer.DialContext(t.Context(), "unix", fixture.socketPath)
		if dialErr != nil {
			t.Fatalf("replacement supervisor socket is not listening: %v", dialErr)
		}
		if closeErr := connection.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	case !errors.Is(err, os.ErrNotExist):
		t.Fatalf("inspect repaired supervisor endpoint: %v", err)
	}
	assertSupervisorProbeTestDaemonStartCount(t, fixture.base, starts)
	if active, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root); err != nil || !found || active != fixture.published {
		t.Fatalf("repair changed the published generation: %+v found=%t err=%v", active, found, err)
	}
	assertSupervisorProbeTestDaemonMatchesBinding(t, filepath.Join(fixture.base, "daemon.identity.json"), fixture.published)
}

func (fixture refusedEndpointAppFixture) removeRecordedPID(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(fixture.base, "run", "pueue.pid")); err != nil {
		t.Fatal(err)
	}
}

func TestStatusReportsRefusedEndpointUnreachableWithoutMutation(t *testing.T) {
	fixture := newRefusedEndpointAppFixture(t)
	result := supervisorStatus(fixture.store.Root, fixture.deps, newSupervisorTestResponse())
	control := result.response.SupervisorControl
	if result.code != 1 || control.LegacyReachability != "unreachable" || control.ActiveWork != "unknown" || control.NextAction != "run_supervisor_migrate" {
		t.Fatalf("refused status = code %d %+v", result.code, control)
	}
	if !errors.Is(result.err, pueue.ErrPrivateEndpointRefused) || result.err.Error() != "private supervisor is unreachable" {
		t.Fatalf("status did not retain its bounded typed cause: %v", result.err)
	}
	fixture.assertSocketPreserved(t)
	assertSupervisorProbeTestDaemonStartCount(t, fixture.base, 1)

	// The same durable state remains repairable by the admission owner.
	repaired, err := preparePrivateAdmissionSupervisor(fixture.deps, fixture.store, fixture.deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, repaired, err)
	if err != nil {
		t.Fatalf("admission after status did not repair the refused endpoint: %v", err)
	}
	fixture.assertRepaired(t, 2)
}

func TestLegacyStatusReportsRefusedEndpointUnreachable(t *testing.T) {
	refusal := errors.Join(pueue.ErrBinding, pueue.ErrPrivateEndpointRefused)
	result, done := legacySupervisorStateResult(newSupervisorTestResponse(), inspection.GroupRequestRecord{}, false, true, false, refusal)
	control := result.response.SupervisorControl
	if !done || result.code != 1 || control.LegacyReachability != "unreachable" || control.ActiveWork != "unknown" || control.NextAction != "stop_and_report" {
		t.Fatalf("legacy refused status = done %t code %d %+v", done, result.code, control)
	}
	if !errors.Is(result.err, pueue.ErrPrivateEndpointRefused) || result.err.Error() != "private supervisor is unreachable" {
		t.Fatalf("legacy status did not retain its bounded typed cause: %v", result.err)
	}
	result, _ = legacySupervisorStateResult(newSupervisorTestResponse(), inspection.GroupRequestRecord{}, false, true, false, pueue.ErrBinding)
	if result.response.SupervisorControl.LegacyReachability != "incompatible" || errors.Is(result.err, pueue.ErrBinding) {
		t.Fatalf("unrelated verification failure changed presentation: %+v %v", result.response.SupervisorControl, result.err)
	}
}

func TestAdmissionClosesRefusedEndpointOfAbsentRecordedDaemon(t *testing.T) {
	fixture := newRefusedEndpointAppFixture(t)
	repaired, err := preparePrivateAdmissionSupervisor(fixture.deps, fixture.store, fixture.deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, repaired, err)
	if err != nil || repaired == nil {
		t.Fatalf("admission did not repair the refused endpoint: client=%v err=%v", repaired, err)
	}
	if repaired.Binding() != fixture.published {
		t.Fatalf("admission returned a different generation: %+v", repaired.Binding())
	}
	fixture.assertRepaired(t, 2)
	assertSupervisorProbeTestNoAdmissionAuthority(t, fixture.store)
}

func TestDispatchReportsRefusedEndpointOwnerReason(t *testing.T) {
	fixture := newRefusedEndpointAppFixture(t)
	fixture.removeRecordedPID(t)
	supervisor, err := preparePrivateAdmissionSupervisor(fixture.deps, fixture.store, fixture.deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, supervisor, err)
	if err == nil {
		t.Fatal("admission started a daemon for an unknown endpoint owner")
	}
	staged := admissioncore.WithStage("start-supervisor", err)
	failure := dispatchFailure(staged, "not_created")
	if failure.Code != "supervisor_setup_failed" || failure.ReasonCode != "endpoint_owner_unknown" || failure.NextAction != "stop_and_report" {
		t.Fatalf("dispatch failure = %+v", failure)
	}
	message := commandPresentationError("dispatch", commandResult{err: staged, response: Response{TaskRecord: "not_created"}}).Error()
	if !strings.Contains(message, supervisorFailureDetail("endpoint_owner_unknown")) || strings.Contains(message, fixture.base) {
		t.Fatalf("dispatch message is not bounded: %q", message)
	}
	fixture.assertSocketPreserved(t)
	assertSupervisorProbeTestDaemonStartCount(t, fixture.base, 1)
}

func TestMigrateRefusedEndpointWithoutRecordedPIDStops(t *testing.T) {
	store, deps, original, owner := newOwnedSupervisorProbeTestFixture(t)
	fixture := refusePrivateAppEndpoint(t, store.Root, original.Binding())
	fixture.store, fixture.deps = store, withSupervisorProbeRouting(t, deps, owner)
	fixture.removeRecordedPID(t)
	fixture.deps.SupervisorOptions.PrivateMigrationAuthorizer = authorizePrivateMigrationQueue
	result := supervisorMigrate(fixture.store.Root, fixture.deps, newSupervisorTestResponse())
	control := result.response.SupervisorControl
	if result.code != 1 || control.ReasonCode != "endpoint_owner_unknown" || control.NextAction != "stop_and_report" {
		t.Fatalf("migrate result = code %d %+v %v", result.code, control, result.err)
	}
	fixture.assertSocketPreserved(t)
	assertSupervisorProbeTestDaemonStartCount(t, fixture.base, 1)
}

func TestRefusedEndpointReentryAfterUnlinkStartsOnce(t *testing.T) {
	fixture := newRefusedEndpointAppFixture(t)
	// A crash after the canonical removal leaves only the proven absence.
	if err := os.Remove(fixture.socketPath); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		supervisor, err := preparePrivateAdmissionSupervisor(fixture.deps, fixture.store, fixture.deps.SupervisorOptions)
		settleSupervisorProbeTestClient(t, supervisor, err)
		if err != nil {
			t.Fatalf("admission re-entry failed: %v", err)
		}
	}
	fixture.assertRepaired(t, 2)
}

func TestConcurrentAdmissionsRepairRefusedEndpointOnce(t *testing.T) {
	fixture := newRefusedEndpointAppFixture(t)
	for _, client := range prepareSupervisorProbeAdmissions(t, fixture.store, fixture.deps, 8) {
		if client == nil {
			t.Fatal("concurrent admission returned a nil supervisor")
		}
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
	}
	fixture.assertRepaired(t, 2)
}

// newRefusedEndpointContinueFixture is the public continue scenario of
// outputUpgradeFixture with its committed predecessor bound to a private
// supervisor whose daemon exited naturally, leaving a refused endpoint.
func newRefusedEndpointContinueFixture(t *testing.T) (*outputUpgradeFixture, refusedEndpointAppFixture) {
	t.Helper()
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	testCase := outputUpgradeControlCases(t)[0]
	parent := mustCCRContinuationTempDir(t, userHome, "delegation-refused-continue-")
	owner := newComponentSupervisorFixtureIn(t, componentFiniteDaemon, parent)
	fixture := &outputUpgradeFixture{
		testCase:  testCase,
		root:      owner.root,
		workspace: testsupport.CanonicalTempDir(t),
		runner:    filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run"),
	}
	catalog := testCase.catalog(t)
	store, err := taskdir.InitStoreWithPredicates(fixture.root, catalog.Registry())
	if err != nil {
		t.Fatal(err)
	}
	rootID := store.RootID
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	// The supervisor captures its command environment when bound, before the
	// provider-only HOME and PATH snapshot is selected.
	var options pueue.Options
	var clientPath string
	fixture.supervisor, options, clientPath = bindRefusedContinuePair(t, owner, rootID)
	options.ControlOwnerExecutable = canonicalAppTestExecutable(t)
	writeRunnerFixture(t, fixture.runner, "#!/bin/sh\nexit 0\n")
	environment := installOutputUpgradeProvider(t, testCase.executable())
	fixture.deps = Dependencies{
		ResourceChecker:             fixtureResources,
		Catalog:                     catalog,
		SupervisorOptions:           options,
		InitialSupervisorExecutable: clientPath,
		RunnerExecutable:            fixture.runner,
		Environment:                 environment,
	}
	fixture.deps = withSupervisorProbeRouting(t, fixture.deps, owner)
	fixture.createPredecessor(t)
	return fixture, refusePrivateAppEndpoint(t, fixture.root, fixture.supervisor.Binding())
}

// bindRefusedContinuePair binds a live private generation of the finite pair
// at root; callers then replace its endpoint with a refused socket.
func bindRefusedContinuePair(t *testing.T, owner *componentSupervisorFixture, rootID string) (*pueue.Client, pueue.Options, string) {
	t.Helper()
	store, err := openNativeSupervisorStore(t.Context(), owner.root)
	owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	clientPath, daemonPath := writeRefusedContinueSupervisorPair(t, rootID)
	options := pueue.Options{Environment: pueue.DefaultEnvironment(), ObservationTimeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := owner.bindPrivate(ctx, clientPath, daemonPath, options)
	settleSupervisorProbeTestClient(t, client, err)
	if err != nil {
		t.Fatalf("bind private refused-endpoint pair fixture: %v", err)
	}
	owner.listen(t, client.Binding().Endpoint)
	return client, options, clientPath
}

// writeRefusedContinueSupervisorPair is a finite private pair that answers
// readiness only after its daemon wrote the ready marker and accepts the
// ordinary stashed submission without running a provider. A status-fault
// marker makes the next status write the marker's bytes as its response and
// fail once, so any later recovery attempt would visibly repair the endpoint. The daemon names
// mkdir absolutely because a continuation starts it with the provider-only
// environment recorded by its predecessor.
func writeRefusedContinueSupervisorPair(t *testing.T, rootID string) (clientPath, daemonPath string) {
	t.Helper()
	bundle := testsupport.CanonicalTempDir(t)
	clientPath, daemonPath = filepath.Join(bundle, "pueue"), filepath.Join(bundle, "pueued")
	writeRunnerFixture(t, clientPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
parallel=1
[ -f "$base/parallel-unlimited" ] && parallel=0
groups='"default":{"status":"Running","parallel_tasks":'$parallel'},"delegation-inspection-`+rootID+`":{"status":"Running","parallel_tasks":1}'
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status)
    if [ -f "$base/status-fault" ]; then IFS= read -r fault < "$base/status-fault" || :; /bin/rm "$base/status-fault"; printf '%s' "$fault"; exit 1; fi
    [ -f "$base/ready" ] || exit 1
    printf '{"tasks":{},"groups":{%s}}\n' "$groups"
    ;;
  group) [ -f "$base/ready" ] || exit 1; printf '{%s}\n' "$groups" ;;
  parallel) [ -f "$base/ready" ] || exit 1; : > "$base/parallel-unlimited" ;;
  add)
    if [ "${4-}" = --help ]; then printf '%s\n' '--stashed --print-task-id'; exit 0; fi
    [ -f "$base/ready" ] || exit 1
    printf '%s\n' add >> "$base/submissions"
    printf '%s\n' 7
    ;;
  enqueue)
    if [ "${4-}" = --help ]; then printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'; exit 0; fi
    [ -f "$base/ready" ] || exit 1
    ;;
  *) exit 64 ;;
esac
`)
	writeRunnerFixture(t, daemonPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
printf '%s\n' started >> "$base/daemon-started"
/bin/mkdir -p "$base/run"
printf '%s\n' "$$" > "$base/run/pueue.pid"
: > "$base/ready"
`)
	return clientPath, daemonPath
}

func TestContinueRepairsRefusedEndpointOfAbsentRecordedDaemon(t *testing.T) {
	t.Parallel()
	fixture, refused := newRefusedEndpointContinueFixture(t)
	before := snapshotOutputUpgradeTask(t, fixture.root, outputUpgradePredecessorID)
	result := continueTask(Arguments{Root: fixture.root, TaskID: outputUpgradePredecessorID}, fixture.deps)
	if result.code != 0 || result.err != nil || result.response.TaskRecord != "created" || result.response.TaskID == "" || result.response.TaskID == outputUpgradePredecessorID {
		t.Fatalf("continue did not create a successor: code=%d err=%v task_record=%q task_id=%q", result.code, result.err, result.response.TaskRecord, result.response.TaskID)
	}
	refused.assertRepaired(t, 2)
	if submissions := readSupervisorProbeTestFile(t, filepath.Join(refused.base, "submissions")); string(submissions) != "add\n" {
		t.Fatalf("successor submissions = %q, want one stashed add", submissions)
	}
	if after := snapshotOutputUpgradeTask(t, fixture.root, outputUpgradePredecessorID); !equalOutputUpgradeSnapshots(before, after) {
		t.Error("predecessor bytes changed while creating its successor")
	}
}

func TestContinueRefusedEndpointWithUnprovenOwnerCreatesNoSuccessor(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		reason string
		owner  func(*testing.T, refusedEndpointAppFixture)
	}{
		{name: "missing recorded PID", reason: "endpoint_owner_unknown", owner: func(t *testing.T, refused refusedEndpointAppFixture) { refused.removeRecordedPID(t) }},
		{name: "live recorded PID", reason: "endpoint_refused_live", owner: func(t *testing.T, refused refusedEndpointAppFixture) {
			// The test process is a present process that owns no endpoint.
			if err := os.WriteFile(filepath.Join(refused.base, "run", "pueue.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, refused := newRefusedEndpointContinueFixture(t)
			testCase.owner(t, refused)
			before := snapshotOutputUpgradeTask(t, fixture.root, outputUpgradePredecessorID)
			result := continueTask(Arguments{Root: fixture.root, TaskID: outputUpgradePredecessorID}, fixture.deps)
			var recovery *pueue.PrivateRecoveryError
			if result.code == 0 || result.response.TaskRecord == "created" || !errors.As(result.err, &recovery) || recovery.ReasonCode != testCase.reason || !errors.Is(result.err, pueue.ErrPrivateEndpointRefused) {
				t.Fatalf("continue result = code %d response=%+v err=%v, want %s", result.code, result.response, result.err, testCase.reason)
			}
			failure := dispatchFailure(result.err, result.response.TaskRecord)
			wantFailure := DispatchFailureResponse{Code: "supervisor_setup_failed", ReasonCode: testCase.reason, Stage: "start-supervisor", NextAction: "stop_and_report"}
			if failure != wantFailure {
				t.Fatalf("continuation lost its supervisor setup failure: %+v", failure)
			}
			tasks, err := os.ReadDir(filepath.Join(fixture.root, "tasks"))
			if err != nil || len(tasks) != 1 {
				t.Fatalf("continue created a successor task directory: entries=%d err=%v", len(tasks), err)
			}
			if _, err = os.Lstat(filepath.Join(refused.base, "submissions")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused continuation submitted work: %v", err)
			}
			refused.assertSocketPreserved(t)
			assertSupervisorProbeTestDaemonStartCount(t, refused.base, 1)
			if after := snapshotOutputUpgradeTask(t, fixture.root, outputUpgradePredecessorID); !equalOutputUpgradeSnapshots(before, after) {
				t.Error("predecessor bytes changed after a refused continuation")
			}
		})
	}
}

// refusedPairFixture is a store-backed private root bound to the finite pair,
// with its recorded daemon absent and a refused socket at the endpoint.
type refusedPairFixture struct {
	refusedEndpointAppFixture
	options    pueue.Options
	clientPath string
	owner      *componentSupervisorFixture
	source     infrastructure.SavedBindingSource
}

func newRefusedPairFixture(t *testing.T) refusedPairFixture {
	t.Helper()
	owner := newComponentSupervisorFixture(t, componentFiniteDaemon)
	root := owner.root
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	rootID := store.RootID
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	client, options, clientPath := bindRefusedContinuePair(t, owner, rootID)
	brief := []byte("refused supervisor control recovery")
	request := newHistoricalSupervisorTaskRecord(rootID, testsupport.CanonicalTempDir(t), brief)
	runner := filepath.Join(owner.base, "delegate-run")
	writeRunnerFixture(t, runner, "#!/bin/sh\nexit 0\n")
	profile := appTestExecutionProfile(&request, runner, nil)
	meta := tasklifecycle.NewMeta(request, profile, client.Binding(), runner, task.RunnerOwnershipCustom, "refused-supervisor-test")
	td, err := owner.store.CreateTask(request.TaskID, &request, brief, &meta)
	owner.ownTask(td)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := infrastructure.SavedBindingDigest(meta.SupervisorConfig)
	if err != nil {
		t.Fatal(err)
	}
	refused := refusePrivateAppEndpoint(t, root, client.Binding())
	refused.store = owner.store
	refused.deps = withSupervisorProbeRouting(t, Dependencies{ResourceChecker: fixtureResources, InitialSupervisorExecutable: clientPath, SupervisorOptions: options}, owner)
	return refusedPairFixture{
		refusedEndpointAppFixture: refused, options: owner.setupOptions(options), clientPath: clientPath, owner: owner,
		source: infrastructure.SavedBindingSource{RootID: rootID, Kind: infrastructure.SavedBindingTaskMetadata, OwnerID: request.TaskID, BindingSHA256: digest},
	}
}

func TestControlClientRepairsCleanRefusedEndpoint(t *testing.T) {
	fixture := newRefusedPairFixture(t)
	client, err := fixture.deps.supervisorControlClientOpener()(t.Context(), fixture.root, fixture.published, fixture.source, fixture.deps.SupervisorOptions, fixture.deps.supervisorLookup())
	fixture.owner.ownClient(client, err)
	if err != nil {
		t.Fatalf("control client did not repair the clean refusal: %v", err)
	}
	fixture.assertRepaired(t, 2)
}

// TestSupervisorClientsNeverRecoverObservationFault gives both supervisor
// clients a refused endpoint whose status command fails once with a malformed
// or truncated response. Neither is a clean refusal, so neither client
// reconciles, recovers, starts, or touches the socket.
func TestSupervisorClientsNeverRecoverObservationFault(t *testing.T) {
	for _, output := range []string{"x", `{"tasks"`} {
		t.Run(strconv.Quote(output), func(t *testing.T) {
			testSupervisorClientsNeverRecoverObservationFault(t, output)
		})
	}
}

func testSupervisorClientsNeverRecoverObservationFault(t *testing.T, output string) {
	t.Helper()
	for name, open := range map[string]func(refusedPairFixture) (*pueue.Client, error){
		"control client": func(fixture refusedPairFixture) (*pueue.Client, error) {
			client, err := fixture.deps.supervisorControlClientOpener()(t.Context(), fixture.root, fixture.published, fixture.source, fixture.deps.SupervisorOptions, fixture.deps.supervisorLookup())
			fixture.owner.ownClient(client, err)
			return client, err
		},
		"recovering client": func(fixture refusedPairFixture) (*pueue.Client, error) {
			before := len(fixture.owner.daemonWaits())
			client, err := supervisorcore.RecoverPrivateClient(t.Context(), fixture.store, fixture.published, fixture.options, supervisorcore.ExecutableLookup{InitialExecutable: fixture.clientPath})
			fixture.owner.ownClient(client, err)
			fixture.owner.ownDaemonClients(client, before)
			return client, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRefusedPairFixture(t)
			if err := os.WriteFile(filepath.Join(fixture.base, "status-fault"), []byte(output), 0o600); err != nil {
				t.Fatal(err)
			}
			client, err := open(fixture)
			if client != nil || err == nil || !errors.Is(err, pueue.ErrBinding) || errors.Is(err, pueue.ErrPrivateEndpointRefused) {
				t.Fatalf("observation fault = client %v err %v, want an operational binding failure", client, err)
			}
			if _, err = os.Lstat(filepath.Join(fixture.base, "status-fault")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the observation fault was not consumed: %v", err)
			}
			fixture.assertSocketPreserved(t)
			assertSupervisorProbeTestDaemonStartCount(t, fixture.base, 1)
		})
	}
}
