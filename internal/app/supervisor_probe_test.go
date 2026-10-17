package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestPreparePrivateAdmissionSupervisorRecoversPendingPublication(t *testing.T) {
	store, deps, original := newSupervisorProbeTestFixture(t)
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
	store, deps, original := newSupervisorProbeTestFixture(t)
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
	store, deps, _ := newSupervisorProbeTestFixture(t)
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
	store, deps, original := newSupervisorProbeTestFixture(t)
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
	store, deps, original := newSupervisorProbeTestFixture(t)
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

	continuation, err := prepareAdmissionSupervisor(Arguments{savedSupervisor: &saved}, deps, store, commonprovider.ProfileCandidate{}, "", deps.SupervisorOptions, true)
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

	warm, err := prepareAdmissionSupervisor(Arguments{savedSupervisor: &saved}, deps, store, commonprovider.ProfileCandidate{}, "", deps.SupervisorOptions, true)
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
	store, deps, supervisor := newSupervisorProbeTestFixture(t)
	base := filepath.Dir(supervisor.Binding().ConfigPath)
	statusLog := filepath.Join(base, "status")
	if err := os.WriteFile(statusLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, runner, "runner")
	meta := &task.MetaRecord{SupervisorConfig: supervisor.Binding()}

	const callers = 24
	start := make(chan struct{})
	errs := make(chan error, callers)
	var calls sync.WaitGroup
	for range callers {
		calls.Add(1)
		go func() {
			defer calls.Done()
			<-start
			errs <- repairPrivateQueueForTask(
				t.Context(), store, supervisor.Binding(), meta,
				Dependencies{
					InitialSupervisorExecutable: deps.InitialSupervisorExecutable,
					SupervisorOptions:           deps.SupervisorOptions,
					RunnerExecutable:            runner,
				},
			)
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

func newSupervisorProbeTestFixture(t *testing.T) (*taskdir.Store, Dependencies, *pueue.Client) {
	t.Helper()
	root := filepath.Join(testsupport.CanonicalTempDir(t), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	bundle := testsupport.CanonicalTempDir(t)
	clientPath := filepath.Join(bundle, "pueue")
	daemonPath := filepath.Join(bundle, "pueued")
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
    printf '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":%s},"delegation-inspection-`+store.RootID+`":{"status":"Running","parallel_tasks":1}}}\n' "$parallel"
    ;;
	group)
	  [ -f "$base/ready" ] || exit 1
	  printf '%s\n' group >> "$base/group"
	  parallel=1
	  [ -f "$base/parallel-unlimited" ] && parallel=0
	  printf '{"default":{"status":"Running","parallel_tasks":%s},"delegation-inspection-`+store.RootID+`":{"status":"Running","parallel_tasks":1}}\n' "$parallel"
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
	writeRunnerFixture(t, daemonPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
printf '%s\n' started >> "$base/daemon-started"
mkdir -p "$base/run"
printf '%s\n' "$$" > "$base/run/pueue.pid"
: > "$base/ready"
`)
	options := pueue.Options{ObservationTimeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := pueue.BindPrivate(ctx, clientPath, daemonPath, store.Root, options)
	settleSupervisorProbeTestClient(t, client, err)
	if err != nil {
		t.Fatalf("bind healthy private supervisor fixture: %v", err)
	}
	listenSupervisorProbeTestEndpoint(t, client.Binding().Endpoint)
	return store, Dependencies{InitialSupervisorExecutable: clientPath, SupervisorOptions: options}, client
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
	var got struct {
		Executable           string `json:"executable"`
		SHA256               string `json:"sha256"`
		ConfigPath           string `json:"config_path,omitempty"`
		ConfigDigest         string `json:"config_digest,omitempty"`
		ResolvedConfigSHA256 string `json:"resolved_config_sha256,omitempty"`
		Endpoint             string `json:"endpoint,omitempty"`
	}
	if err := task.DecodeStrict(readSupervisorProbeTestFile(t, path), &got); err != nil {
		t.Fatal(err)
	}
	if got.Executable != want.DaemonExecutable || got.SHA256 != want.DaemonSHA256 ||
		got.ConfigPath != want.ConfigPath || got.ConfigDigest != want.ConfigDigest ||
		got.ResolvedConfigSHA256 != want.ResolvedConfigSHA256 || got.Endpoint != want.Endpoint {
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
	var supervisor map[string]json.RawMessage
	if err := task.DecodeStrict(record["supervisor"], &supervisor); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := json.Unmarshal(supervisor["daemon_sha256"], &current); err != nil {
		t.Fatal(err)
	}
	replacement := strings.Repeat("0", 64)
	if current == replacement {
		replacement = strings.Repeat("1", 64)
	}
	digest, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	supervisor["daemon_sha256"] = digest
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
