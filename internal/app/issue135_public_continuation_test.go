package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// This is public component protocol coverage. The explicit setup owner joins
// finite CLI/daemon commands and the Go endpoint; no native placement or
// provider execution is represented by the fake supervisor's empty queue.
type publicContinuationFixture struct {
	owner                     *componentSupervisorFixture
	deps                      Dependencies
	client                    *pueue.Client
	clientC, daemonC, clientD string
	seed                      *taskdir.TaskDir
	seedData                  publicContinuationSeedData
	workErr                   error
}

func TestIssue135PublicContinuationPublishedLegacyPair(t *testing.T) {
	for _, change := range []string{"client bytes", "missing client"} {
		t.Run(change, func(t *testing.T) {
			f := newPublicContinuationFixture(t)
			f.assertNoInventory(t)
			receipt := readSupervisorProbeTestFile(t, filepath.Join(f.owner.root, ".supervisor", "ordinary-scheduling.json"))
			daemonInfo, err := os.Stat(f.daemonC)
			if err != nil {
				t.Fatal(err)
			}
			daemonBytes := readSupervisorProbeTestFile(t, f.daemonC)
			if change == "client bytes" {
				original := readSupervisorProbeTestFile(t, f.clientC)
				replacement := f.clientC + ".replacement"
				writeRunnerFixture(t, replacement, string(original)+"\n# compatible replacement client\n")
				if err = os.Rename(replacement, f.clientC); err != nil {
					t.Fatal(err)
				}
			} else if err = os.Remove(f.clientC); err != nil {
				t.Fatal(err)
			}

			result := runCommand(Arguments{Command: "continue", Root: f.owner.root, TaskID: f.seed.TaskID}, f.deps)
			f.workErr = result.err
			response := publicContinuationResponse(t, result)
			f.assertDaemonUnchanged(t, daemonInfo, daemonBytes)
			if change == "client bytes" {
				f.assertCreated(t, result, response)
			} else {
				f.assertRefused(t, result, response, receipt)
			}
		})
	}
}

func newPublicContinuationFixture(t *testing.T) *publicContinuationFixture {
	t.Helper()
	f := &publicContinuationFixture{owner: newComponentSupervisorFixture(t, componentFiniteDaemon)}
	// Registered before any Start. A failed canonical closure suppresses
	// path removal while independently owned finite resources still close.
	t.Cleanup(func() { f.cleanup(t) })
	f.clientC, f.daemonC = writePublicContinuationPair(t, f.owner.base, "C")
	f.clientD, _ = writePublicContinuationPair(t, f.owner.base, "D")
	f.deps = fixtureContinuationDependencies(t, nil, f.clientD)
	f.deps.Environment = []string{"PATH=/usr/bin:/bin"}
	f.deps.SupervisorOptions = pueue.Options{Environment: f.deps.Environment, ObservationTimeout: 10 * time.Second, WaitForPendingClose: true}
	f.deps.RunnerExecutable = filepath.Join(f.owner.base, "runner")
	writeRunnerFixture(t, f.deps.RunnerExecutable, "#!/bin/sh\nexit 64\n")
	store, err := taskdir.InitStoreWithPredicates(f.owner.root, f.deps.Catalog.Registry())
	f.owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	f.client, err = f.owner.bindPrivate(ctx, f.clientC, f.daemonC, f.deps.SupervisorOptions)
	f.workErr = err
	if err != nil {
		t.Fatal(err)
	}
	f.owner.listen(t, f.client.Binding().Endpoint)
	if err = supervisorcore.EnsureAdmissionSchedulingContext(ctx, f.owner.root, f.client); err != nil {
		t.Fatal(err)
	}
	f.createSeed(t, f.client.Binding())
	return f
}

func (f *publicContinuationFixture) createSeed(t *testing.T, saved task.SupervisorRef) {
	t.Helper()
	ids, err := f.owner.store.TaskIDs(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatalf("seed inventory=%v err=%v", ids, err)
	}
	workspace := filepath.Join(f.owner.base, "workspace")
	if err = os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	brief := []byte("continue this fixture session")
	request, err := admission.BuildRequest(f.owner.store.RootID, strings.Repeat("a", 32), "fixture:test", workspace, task.TaskConfig{Permission: "read-only", Budget: "1m0s"}, brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := f.deps.Catalog.Candidate(request, f.deps.Environment)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := admission.ProfileForAdmission(candidate, request, false)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture ReadyCandidate has no launch scope. Its factory explicitly
	// records the same isolated scope that owns this component invocation.
	profile.Plan.Environment = slices.Clone(f.deps.Environment)
	meta := tasklifecycle.NewMeta(request, profile, saved, f.deps.RunnerExecutable, task.RunnerOwnershipManaged, "public-continuation-component")
	f.seed, err = f.owner.store.CreateTask(request.TaskID, &request, brief, &meta)
	f.owner.ownTask(f.seed)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := sealFixtureEvidence(f.seed, meta.Predicate, "answer\n", func() error {
		return f.seed.RecordProviderIdentity(task.SessionIdentity{Provider: request.Provider, ConversationID: "public-component-session"})
	})
	if err != nil || outcome == nil {
		t.Fatalf("seed outcome=%+v err=%v", outcome, err)
	}
	// Sole seed-data permission mint. The factory performed no submission,
	// native launch or provider turn; start/started bytes are captured DATA.
	f.seedData, err = capturePublicContinuationSeed(f.seed, outcome.Payload.Basename)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != task.VerdictRejected {
		t.Fatalf("fixture interpreter outcome=%+v", outcome)
	}
}

func publicContinuationResponse(t *testing.T, result commandResult) Response {
	t.Helper()
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "continue", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	t.Logf("public exit=%d response=%s error=%v", result.code, output.Bytes(), result.err)
	return response
}

func (f *publicContinuationFixture) assertCreated(t *testing.T, result commandResult, response Response) {
	t.Helper()
	if result.code != 0 || result.err != nil || response.TaskRecord != "created" || response.TaskID == "" || response.TaskID == f.seed.TaskID || response.Failure != nil {
		t.Fatalf("compatible client did not reach public creation: code=%d response=%+v err=%v", result.code, response, result.err)
	}
	td, err := f.owner.store.OpenTask(response.TaskID)
	f.owner.ownTask(td)
	if err != nil {
		t.Fatal(err)
	}
	_, meta, err := td.PreparedRecords()
	if err != nil || meta.SupervisorConfig != f.client.Binding() {
		t.Fatalf("successor durable binding=%+v err=%v", meta, err)
	}
	receipt, err := td.ReadSupervisorReceipt()
	if err != nil || receipt.ClientExecutable != f.clientC || receipt.DaemonExecutable != f.daemonC || receipt.ClientSHA256 == f.client.Binding().ClientSHA256 || receipt.DaemonSHA256 != f.client.Binding().DaemonSHA256 {
		t.Fatalf("public runtime receipt did not retain same-daemon Cprime: %+v err=%v", receipt, err)
	}
	if got := readSupervisorProbeTestFile(t, filepath.Join(filepath.Dir(f.client.Binding().ConfigPath), "submissions")); string(got) != "add\n" {
		t.Fatalf("submissions=%q", got)
	}
}

func (f *publicContinuationFixture) assertRefused(t *testing.T, result commandResult, response Response, receipt []byte) {
	t.Helper()
	if result.code != 1 || !errors.Is(result.err, pueue.ErrPrivateSupervisorSubmissionIncomplete) || response.TaskRecord != "not_created" || response.TaskID != "" || response.Failure == nil || response.Failure.Code != "continuation_unavailable" || response.Failure.Stage != "resolve-continuation-supervisor" || response.Failure.NextAction != "stop_and_report" {
		t.Fatalf("missing client lost early refusal mapping: code=%d response=%+v err=%v", result.code, response, result.err)
	}
	if after := readSupervisorProbeTestFile(t, filepath.Join(f.owner.root, ".supervisor", "ordinary-scheduling.json")); !bytes.Equal(receipt, after) {
		t.Fatal("early refusal changed scheduling receipt")
	}
	ids, err := f.owner.store.TaskIDs(t.Context())
	if err != nil || len(ids) != 1 || ids[0] != f.seed.TaskID {
		t.Fatalf("early refusal created successor: ids=%v err=%v", ids, err)
	}
	if _, err = os.Lstat(filepath.Join(filepath.Dir(f.client.Binding().ConfigPath), "submissions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("early refusal submitted work: %v", err)
	}
}

func (f *publicContinuationFixture) assertDaemonUnchanged(t *testing.T, before os.FileInfo, data []byte) {
	t.Helper()
	after, err := os.Stat(f.daemonC)
	if err != nil || !os.SameFile(before, after) || !bytes.Equal(data, readSupervisorProbeTestFile(t, f.daemonC)) {
		t.Fatalf("published daemon changed: %v", err)
	}
	assertSupervisorProbeTestDaemonStartCount(t, filepath.Dir(f.client.Binding().ConfigPath), 1)
}

func (f *publicContinuationFixture) assertNoInventory(t *testing.T) {
	t.Helper()
	_, _, found, err := supervisorcore.ResolveStateSupervisorPair(filepath.Join(f.owner.root, ".supervisor"))
	if err != nil || found {
		t.Fatalf("legacy fixture has current inventory: found=%t err=%v", found, err)
	}
}

// Data permission gates removal only. Independently owned finite children,
// their original Waits, endpoint loops and handles are always settled first.
func (f *publicContinuationFixture) cleanup(t *testing.T) {
	t.Helper()
	f.owner.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), componentFixtureCleanupTimeout)
		defer cancel()
		f.owner.closeErr = f.closeOwned(ctx)
	})
	if f.owner.closeErr != nil {
		t.Errorf("retained public continuation namespace %s: %v", f.owner.base, f.owner.closeErr)
	}
}

func (f *publicContinuationFixture) closeOwned(ctx context.Context) error {
	workErr := f.closeWork(ctx)
	finiteErr := f.closeFinite(ctx)
	if err := errors.Join(workErr, finiteErr); err != nil {
		return err
	}
	if err := f.owner.checkDirectory(); err != nil {
		return err
	}
	if err := f.owner.directory.Close(); err != nil {
		return err
	}
	return os.RemoveAll(f.owner.base)
}

func (f *publicContinuationFixture) closeWork(ctx context.Context) (resultErr error) {
	if f.owner.store == nil {
		return nil
	}
	ids, err := f.owner.store.TaskIDs(ctx)
	resultErr = errors.Join(resultErr, err)
	for _, id := range ids {
		if f.seed != nil && id == f.seed.TaskID {
			resultErr = errors.Join(resultErr, f.seedData.check(f.seed))
			continue
		}
		resultErr = errors.Join(resultErr, f.closePublicTask(ctx, id))
	}
	inspections, err := f.owner.store.InspectionOperationIDs(ctx)
	resultErr = errors.Join(resultErr, err)
	if len(inspections) != 0 {
		resultErr = errors.Join(resultErr, fmt.Errorf("unexpected inspection work retained: %v", inspections))
	}
	workspaces, err := f.owner.store.WorkspaceOperationIDs(ctx)
	resultErr = errors.Join(resultErr, err)
	if len(workspaces) != 0 {
		resultErr = errors.Join(resultErr, fmt.Errorf("unexpected workspace work retained: %v", workspaces))
	}
	return errors.Join(resultErr, ctx.Err())
}

func (f *publicContinuationFixture) closeFinite(ctx context.Context) (resultErr error) {
	f.owner.mu.Lock()
	f.owner.closing = true
	f.owner.mu.Unlock()
	// Every partial setup client was retained before checking Bind's error.
	// CloseAfterPending synchronously settles the complete public work graph.
	for _, client := range f.owner.clients {
		resultErr = errors.Join(resultErr, client.CloseAfterPending(f.workErr))
	}
	completions := slices.Clone(f.owner.pending)
	completions = append(completions, publicContinuationCommandCompletions(f.workErr)...)
	for _, wait := range f.owner.daemonWaits() {
		completions = append(completions, wait.done)
	}
	for _, done := range completions {
		select {
		case <-done:
		case <-ctx.Done():
			resultErr = errors.Join(resultErr, ctx.Err())
		}
	}
	for _, endpoint := range f.owner.endpoints {
		if endpoint.listener == nil {
			continue
		}
		err := endpoint.listener.Close()
		if !errors.Is(err, net.ErrClosed) {
			resultErr = errors.Join(resultErr, err)
		}
		select {
		case <-endpoint.done:
		case <-ctx.Done():
			resultErr = errors.Join(resultErr, ctx.Err())
		}
	}
	// Historical result errors do not retain borrowers. Actual unfinished
	// children do, even after the observer's finite cleanup context expires.
	for _, done := range completions {
		select {
		case <-done:
		default:
			return errors.Join(resultErr, errors.New("finite child completion unresolved; borrowers retained"))
		}
	}
	for _, td := range f.owner.tasks {
		resultErr = errors.Join(resultErr, td.Close())
	}
	if f.owner.store != nil {
		resultErr = errors.Join(resultErr, f.owner.store.Close())
	}
	if f.owner.rootDirectory != nil {
		err := f.owner.rootDirectory.Close()
		resultErr = errors.Join(resultErr, err)
		if err == nil {
			f.owner.rootDirectory = nil
		}
	}
	return resultErr
}

// Read completion ownership from every branch, including retained bootstrap
// owners. A historical observation error alone is never an unfinished child.
func publicContinuationCommandCompletions(err error) []<-chan struct{} {
	if err == nil {
		return nil
	}
	var done []<-chan struct{}
	if owner, ok := err.(interface{ Done() <-chan struct{} }); ok {
		done = append(done, owner.Done())
	}
	if inFlight, ok := err.(*pueue.InFlightError); ok && inFlight.Pending != nil {
		done = append(done, inFlight.Pending.Done())
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			done = append(done, publicContinuationCommandCompletions(child)...)
		}
	} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		done = append(done, publicContinuationCommandCompletions(wrapped.Unwrap())...)
	}
	return done
}

func (f *publicContinuationFixture) closePublicTask(ctx context.Context, id string) (resultErr error) {
	td, err := f.owner.store.OpenTask(id)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	stop, err := td.PrepareStop(strings.Repeat("b", 32), "user", time.Time{})
	if err != nil {
		return err
	}
	if err = errors.Join(stop.Consume(), stop.Release()); err != nil {
		return err
	}
	proven, err := td.ProveUnlaunchedTaskContext(ctx)
	if err != nil || !proven {
		return errors.Join(err, fmt.Errorf("task %s lacks canonical never-launched proof", id))
	}
	return checkRootOrdinaryTaskClosure(ctx, f.owner.store, id)
}

func writePublicContinuationPair(t *testing.T, base, name string) (string, string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	client, daemon := filepath.Join(dir, "pueue"), filepath.Join(dir, "pueued")
	writeRunnerFixture(t, client, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
parallel=1
[ -f "$base/parallel-unlimited" ] && parallel=0
groups='"default":{"status":"Running","parallel_tasks":'$parallel'}}'
case "${3-}" in
 --version) printf '%s\n' 'pueue 4.0.4' ;;
 status) [ -f "$base/ready" ]; printf '{"tasks":{},"groups":{%s}\n' "$groups" ;;
 group) printf '{%s\n' "$groups" ;;
 parallel) : > "$base/parallel-unlimited" ;;
 add) if [ "${4-}" = --help ]; then printf '%s\n' '--stashed --print-task-id'; else printf '%s\n' add >> "$base/submissions"; printf '%s\n' 7; fi ;;
 enqueue) [ "${4-}" != --help ] || printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...' ;;
 *) exit 64 ;;
esac
`)
	writeRunnerFixture(t, daemon, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
/bin/mkdir -p "$base/run"
printf '%s\n' "$$" > "$base/run/pueue.pid"
printf '%s\n' started >> "$base/daemon-started"
: > "$base/ready"
# distinct daemon installation `+name+`\n`)
	return client, daemon
}

// Verify the exact no-actor seed contract before any finite supervisor fixture
// is launched: rejected publication is eligible with its observed session,
// recorded launch scope and released runner lease. It is not native closure.
func TestIssue135PublicContinuationSeedContract(t *testing.T) {
	f := &publicContinuationFixture{owner: newComponentSupervisorFixture(t, componentFiniteDaemon)}
	t.Cleanup(func() { f.cleanup(t) })
	f.deps = fixtureContinuationDependencies(t, nil, "/unused/pueue")
	f.deps.Environment = []string{"PATH=/usr/bin:/bin"}
	f.deps.RunnerExecutable = filepath.Join(f.owner.base, "runner")
	store, err := taskdir.InitStoreWithPredicates(f.owner.root, f.deps.Catalog.Registry())
	f.owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	digest := task.ComputeSHA256([]byte("no-actor supervisor fixture"))
	saved := supervisorfixture.ContinuationSupervisorRef(f.owner.root, digest, "unix:"+filepath.Join(f.owner.base, "socket"))
	f.createSeed(t, saved)
	request, meta, err := f.seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	if err = tasklifecycle.ValidateContinuationEnvironment(meta); err != nil {
		t.Fatal(err)
	}
	prior, err := tasklifecycle.ContinuationPrior(f.seed, request)
	if err != nil || prior.Provider != "fixture:test" || prior.ConversationID != "public-component-session" || prior.PredecessorTaskID != f.seed.TaskID {
		t.Fatalf("observed session=%+v err=%v", prior, err)
	}
	if err = tasklifecycle.PredecessorTerminatedContext(t.Context(), f.owner.root, f.seed, request, tasklifecycle.SupervisorOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = f.seedData.check(f.seed); err != nil {
		t.Fatal(err)
	}
	if waits := f.owner.daemonWaits(); len(waits) != 0 {
		t.Fatalf("seed contract launched daemon: %v", waits)
	}
}

func TestIssue135PublicContinuationDataFaultStillClosesOwnedHandles(t *testing.T) {
	// Entirely no-actor data: t.TempDir owns its eventual removal after the
	// assertion. The real cleanup path must retain it at the fault boundary.
	base := t.TempDir()
	directory, err := os.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	owner := &componentSupervisorFixture{base: base, root: filepath.Join(base, "state"), directory: directory, lifetime: componentFiniteDaemon}
	f := &publicContinuationFixture{owner: owner}
	t.Cleanup(func() {
		if err := errors.Join(f.closeFinite(context.WithoutCancel(t.Context())), directory.Close()); err != nil {
			t.Error(err)
		}
	})
	f.deps = fixtureContinuationDependencies(t, nil, "/unused/pueue")
	f.deps.Environment = []string{"PATH=/usr/bin:/bin"}
	f.deps.RunnerExecutable = filepath.Join(base, "runner")
	store, err := taskdir.InitStoreWithPredicates(owner.root, f.deps.Catalog.Registry())
	owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	digest := task.ComputeSHA256([]byte("no-actor supervisor fixture"))
	f.createSeed(t, supervisorfixture.ContinuationSupervisorRef(owner.root, digest, "unix:"+filepath.Join(base, "socket")))
	if err = os.WriteFile(filepath.Join(f.seed.Dir, "unexpected-native-authority"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = f.closeOwned(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed seed removal error=%v", err)
	}
	if _, err = os.Stat(base); err != nil {
		t.Fatalf("data fault removed namespace: %v", err)
	}
	if _, err = store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("data permission blocked independent Store close: %v", err)
	}
}

func TestIssue135PublicContinuationUnfinishedWaitRetainsBorrowers(t *testing.T) {
	base := t.TempDir()
	directory, err := os.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	owner := &componentSupervisorFixture{base: base, root: filepath.Join(base, "state"), directory: directory, lifetime: componentFiniteDaemon}
	f := &publicContinuationFixture{owner: owner}
	// This completion port is synthetic, with no native PID or placement claim.
	done := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(done)
		}
		if err := errors.Join(f.closeFinite(context.WithoutCancel(t.Context())), directory.Close()); err != nil {
			t.Error(err)
		}
	})
	store, err := taskdir.InitStore(owner.root)
	owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.RegisterDaemonWait(owner.root, 123, done); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = f.closeFinite(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unfinished Wait error=%v", err)
	}
	if _, err = store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("unfinished original Wait lost Store borrower: %v", err)
	}
	close(done)
	released = true
	if err = f.closeFinite(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("completed original Wait retained Store: %v", err)
	}
}
