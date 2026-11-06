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
	owner                              *componentSupervisorFixture
	deps                               Dependencies
	client                             *pueue.Client
	clientC, daemonC, clientD, daemonD string
	seed                               *taskdir.TaskDir
	seedData                           publicContinuationSeedData
	workErr                            error
}

// The fixture owns completion custody and the release-only projection. The
// optional reader is observed after every original completion, never as a
// replacement for Done or as a source of command diagnostics.
type publicContinuationCompletion struct {
	done         <-chan struct{}
	releaseFault func() error
}

func TestIssue135PublicContinuationPublishedLegacyPair(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"client bytes", "missing client", "missing clients"} {
		t.Run(change, func(t *testing.T) {
			f := newPublicContinuationFixture(t)
			f.assertNoInventory(t)
			receipt := readSupervisorProbeTestFile(t, filepath.Join(f.owner.root, ".supervisor", "ordinary-scheduling.json"))
			daemonInfo, err := os.Stat(f.daemonC)
			if err != nil {
				t.Fatal(err)
			}
			expectedRuntime := f.changePublishedClient(t, change)
			result := runCommand(Arguments{Command: "continue", Root: f.owner.root, TaskID: f.seed.TaskID}, f.deps)
			f.workErr = result.err
			response := publicContinuationResponse(t, result)
			f.assertDaemonUnchanged(t, daemonInfo)
			if change == "missing clients" {
				f.assertRefused(t, result, response, receipt)
				return
			}
			f.assertCreated(t, result, response, expectedRuntime)
		})
	}
}

func (f *publicContinuationFixture) changePublishedClient(t *testing.T, change string) task.SupervisorRef {
	t.Helper()
	if change == "client bytes" {
		replacement := f.clientC + ".replacement"
		writeRunnerFixture(t, replacement, publicContinuationClient+"\n# compatible replacement client\n")
		if err := os.Rename(replacement, f.clientC); err != nil {
			t.Fatal(err)
		}
		return f.expectedRuntime(f.clientC, f.daemonC)
	}
	if err := os.Remove(f.clientC); err != nil {
		t.Fatal(err)
	}
	if change == "missing clients" {
		if err := os.Remove(f.clientD); err != nil {
			t.Fatal(err)
		}
	}
	return f.expectedRuntime(f.clientD, f.daemonD)
}

// Expected transport comes from this fixture's selected installed paths and
// canonical config, independently of the scheduling receipt under test.
func (f *publicContinuationFixture) expectedRuntime(client, daemon string) task.SupervisorRef {
	binding := f.client.Binding()
	binding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	binding.ClientExecutable, binding.DaemonExecutable = client, daemon
	binding.ClientSHA256, binding.DaemonSHA256 = "", ""
	return binding
}

func newPublicContinuationFixture(t *testing.T) *publicContinuationFixture {
	t.Helper()
	f := &publicContinuationFixture{owner: newComponentSupervisorFixture(t, componentFiniteDaemon)}
	// Registered before any Start. A failed canonical closure suppresses
	// path removal while independently owned finite resources still close.
	t.Cleanup(func() { f.cleanup(t) })
	f.clientC, f.daemonC = writePublicContinuationPair(t, f.owner.base, "C")
	f.clientD, f.daemonD = writePublicContinuationPair(t, f.owner.base, "D")
	f.deps = fixtureContinuationDependencies(t, nil, f.clientD)
	f.deps.Environment = []string{"PATH=/usr/bin:/bin"}
	f.deps.SupervisorOptions = pueue.Options{Environment: f.deps.Environment, ObservationTimeout: 10 * time.Second, WaitForPendingClose: true}
	f.deps.RunnerExecutable = canonicalAppTestExecutable(t)
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

func (f *publicContinuationFixture) assertCreated(t *testing.T, result commandResult, response Response, expectedRuntime task.SupervisorRef) {
	t.Helper()
	if result.code != 0 || result.err != nil || response.TaskRecord != "created" || response.TaskID == "" || response.TaskID == f.seed.TaskID || response.Failure != nil {
		t.Fatalf("compatible client did not reach public creation: code=%d response=%+v err=%v", result.code, response, result.err)
	}
	f.assertSuccessorBinding(t, response.TaskID)
	f.assertRuntimeScheduling(t, expectedRuntime)
	if got := readSupervisorProbeTestFile(t, filepath.Join(filepath.Dir(f.client.Binding().ConfigPath), "submissions")); string(got) != "add\n" {
		t.Fatalf("submissions=%q", got)
	}
}

func (f *publicContinuationFixture) assertSuccessorBinding(t *testing.T, id string) {
	t.Helper()
	td, err := f.owner.store.OpenTask(id)
	f.owner.ownTask(td)
	if err != nil {
		t.Fatal(err)
	}
	_, meta, err := td.PreparedRecords()
	if err != nil || meta.SupervisorConfig != f.client.Binding() {
		t.Fatalf("successor durable binding=%+v err=%v", meta, err)
	}
	receipt, err := td.ReadSupervisorReceipt()
	if err != nil || receipt.SupervisorRef() != f.client.Binding() || receipt.NumericTaskID != 7 {
		t.Fatalf("task receipt changed its logical published binding: %+v err=%v", receipt, err)
	}
}

func (f *publicContinuationFixture) assertRuntimeScheduling(t *testing.T, runtime task.SupervisorRef) {
	t.Helper()
	// Task metadata and its numeric receipt retain logical C. Scheduling
	// owns the runtime Cprime identity and compares the full canonical binding.
	if runtime.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || runtime.ClientSHA256 != "" || runtime.DaemonSHA256 != "" || !task.SameSupervisorIdentity(runtime, f.client.Binding()) {
		t.Fatalf("runtime lost installed-path transport identity: %+v", runtime)
	}
	recorded, err := supervisorcore.OrdinarySchedulingRecorded(f.owner.root, runtime)
	if err != nil || !recorded {
		t.Fatalf("scheduling receipt did not record same-daemon Cprime: recorded=%t err=%v", recorded, err)
	}
}

func (f *publicContinuationFixture) assertRefused(t *testing.T, result commandResult, response Response, receipt []byte) {
	t.Helper()
	// Failed selected-installation lookup is ErrConfiguration; classifyCode
	// assigns exit 2 before considering its joined ErrBinding cause.
	if result.code != 2 || !errors.Is(result.err, pueue.ErrConfiguration) || !errors.Is(result.err, pueue.ErrBinding) || !errors.Is(result.err, os.ErrNotExist) || response.TaskRecord != "not_created" || response.Failure == nil || response.Failure.Code != "supervisor_setup_failed" || response.Failure.Stage != "prepare-supervisor-executables" || response.Failure.NextAction != "stop_and_report" {
		t.Fatalf("unavailable selected installations lost early refusal mapping: code=%d response=%+v err=%v", result.code, response, result.err)
	}
	f.assertNoContinuationWork(t, response.TaskID)
	if after := readSupervisorProbeTestFile(t, filepath.Join(f.owner.root, ".supervisor", "ordinary-scheduling.json")); !bytes.Equal(receipt, after) {
		t.Fatal("early refusal changed scheduling receipt")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(f.client.Binding().ConfigPath), "submissions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("early refusal submitted work: %v", err)
	}
}

func (f *publicContinuationFixture) assertNoContinuationWork(t *testing.T, candidateID string) {
	t.Helper()
	ids, err := f.owner.store.TaskIDs(t.Context())
	if err != nil || len(ids) != 1 || ids[0] != f.seed.TaskID {
		t.Fatalf("early refusal created successor: ids=%v err=%v", ids, err)
	}
	// A generated candidate ID is not durable creation. The authoritative
	// directory census above must still contain only the original seed.
	if candidateID != "" {
		if err = task.ValidateTaskID(candidateID); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Lstat(filepath.Join(f.owner.root, "tasks", candidateID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused candidate has a durable directory: %v", err)
		}
	}
	helpers, err := f.owner.store.InspectionOperationIDs(t.Context())
	if err != nil || len(helpers) != 0 {
		t.Fatalf("early refusal created inspection helpers: ids=%v err=%v", helpers, err)
	}
}

func (f *publicContinuationFixture) assertDaemonUnchanged(t *testing.T, before os.FileInfo) {
	t.Helper()
	after, err := os.Stat(f.daemonC)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("published daemon changed: %v", err)
	}
	published, found, err := pueue.ReadPrivateSupervisorBinding(f.owner.root)
	if err != nil || !found || published != f.client.Binding() {
		t.Fatalf("public continuation replaced published generation: found=%t binding=%+v err=%v", found, published, err)
	}
	assertSupervisorProbeTestDaemonStartCount(t, filepath.Dir(f.client.Binding().ConfigPath), 1)
}

func (f *publicContinuationFixture) assertNoInventory(t *testing.T) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(f.owner.root, ".supervisor", "pueue-pair.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy fixture has executable inventory: %v", err)
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

func (f *publicContinuationFixture) closeOwned(ctx context.Context, extra ...publicContinuationCompletion) error {
	workErr := f.closeWork(ctx)
	finiteErr := f.closeFinite(ctx, extra...)
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

func (f *publicContinuationFixture) closeFinite(ctx context.Context, extra ...publicContinuationCompletion) (resultErr error) {
	f.owner.mu.Lock()
	f.owner.closing = true
	f.owner.mu.Unlock()
	// Every partial setup client was retained before checking Bind's error.
	// CloseAfterPending synchronously settles the complete public work graph.
	for _, client := range f.owner.clients {
		resultErr = errors.Join(resultErr, client.CloseAfterPending(f.workErr))
	}
	completions := slices.Clone(extra)
	for _, pending := range f.owner.pending {
		completions = append(completions, publicContinuationCompletion{done: pending})
	}
	completions = append(completions, publicContinuationCommandCompletions(f.workErr)...)
	for _, wait := range f.owner.daemonWaits() {
		completions = append(completions, publicContinuationCompletion{done: wait.done})
	}
	for _, completion := range completions {
		select {
		case <-completion.done:
		case <-ctx.Done():
			resultErr = errors.Join(resultErr, ctx.Err())
		}
	}
	resultErr = errors.Join(resultErr, f.closeEndpoints(ctx))
	// Historical result errors do not retain borrowers. Actual unfinished
	// children do, even after the observer's finite cleanup context expires.
	for _, completion := range completions {
		select {
		case <-completion.done:
		default:
			return errors.Join(resultErr, errors.New("finite child completion unresolved; borrowers retained"))
		}
	}
	// Completion retires usable custody; release failure remains authoritative
	// for path removal even when independently safe borrowers can now close.
	for _, completion := range completions {
		if completion.releaseFault != nil {
			resultErr = errors.Join(resultErr, completion.releaseFault())
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

func (f *publicContinuationFixture) closeEndpoints(ctx context.Context) (resultErr error) {
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
	return resultErr
}

// Adapt actual producer types; BootstrapCleanupError.Err includes command
// diagnostics, so only its Unwrap release projection belongs in this port.
func publicContinuationCommandCompletions(err error) []publicContinuationCompletion {
	completions := collectPublicContinuationCommandCompletions(err)
	// errors.As also sees owners through wrappers. Keep one completion and
	// release observation per authoritative owner when walking all join arms.
	seen := make(map[<-chan struct{}]bool)
	var unique []publicContinuationCompletion
	for _, completion := range completions {
		if seen[completion.done] {
			continue
		}
		seen[completion.done] = true
		unique = append(unique, completion)
	}
	return unique
}

func collectPublicContinuationCommandCompletions(err error) []publicContinuationCompletion {
	if err == nil {
		return nil
	}
	var completions []publicContinuationCompletion
	var owner *pueue.BootstrapCleanupError
	if errors.As(err, &owner) {
		completions = append(completions, publicContinuationCompletion{done: owner.Done(), releaseFault: func() error {
			releaseErr := owner.Unwrap()
			if task.AllErrorCauses(releaseErr, func(cause error) bool { return errors.Is(cause, pueue.ErrInFlight) }) {
				return nil
			}
			return releaseErr
		}})
	}
	var inFlight *pueue.InFlightError
	if errors.As(err, &inFlight) && inFlight.Pending != nil {
		completions = append(completions, publicContinuationCompletion{done: inFlight.Pending.Done()})
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			completions = append(completions, collectPublicContinuationCommandCompletions(child)...)
		}
	} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		completions = append(completions, collectPublicContinuationCommandCompletions(wrapped.Unwrap())...)
	}
	return completions
}

func (f *publicContinuationFixture) closePublicTask(ctx context.Context, id string) (resultErr error) {
	td, err := f.owner.store.OpenTask(id)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	stop, err := td.PrepareStopContext(ctx, strings.Repeat("b", 32), "user", time.Time{})
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
	return ctx.Err()
}

func writePublicContinuationPair(t *testing.T, base, name string) (string, string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	client, daemon := filepath.Join(dir, "pueue"), filepath.Join(dir, "pueued")
	writeRunnerFixture(t, client, publicContinuationClient)
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
		if closeErr := errors.Join(f.closeFinite(context.WithoutCancel(t.Context())), directory.Close()); closeErr != nil {
			t.Error(closeErr)
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
		if closeErr := errors.Join(f.closeFinite(context.WithoutCancel(t.Context())), directory.Close()); closeErr != nil {
			t.Error(closeErr)
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

func TestIssue135PublicContinuationReleaseFaultGatesRemoval(t *testing.T) {
	for _, fault := range []bool{false, true} {
		t.Run(fmt.Sprintf("release fault=%t", fault), func(t *testing.T) {
			f := newPublicContinuationReleaseFixture(t)
			releaseErr := errors.New("bootstrap native release fault")
			if !fault {
				releaseErr = nil
			}
			// Synthetic completed owner ports prove only the fixture removal policy.
			// No bootstrap internals, native processes or native release are fabricated.
			done := make(chan struct{})
			close(done)
			if err := f.owner.RegisterDaemonWait(f.owner.root, 123, done); err != nil {
				t.Fatal(err)
			}
			completion := publicContinuationCompletion{done: done, releaseFault: func() error { return releaseErr }}
			cleanupErr := f.closeOwned(t.Context(), completion)
			f.assertReleaseRemoval(t, releaseErr, cleanupErr)
		})
	}
}

func newPublicContinuationReleaseFixture(t *testing.T) *publicContinuationFixture {
	t.Helper()
	base := t.TempDir()
	directory, err := os.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	owner := &componentSupervisorFixture{base: base, root: filepath.Join(base, "state"), directory: directory, lifetime: componentFiniteDaemon}
	f := &publicContinuationFixture{owner: owner, workErr: errors.New("historical command diagnostic")}
	t.Cleanup(func() {
		closeErr := f.closeFinite(context.WithoutCancel(t.Context()))
		// The successful removal path already closed this exact handle.
		if _, statErr := directory.Stat(); !errors.Is(statErr, os.ErrClosed) {
			closeErr = errors.Join(closeErr, statErr, directory.Close())
		}
		if closeErr != nil {
			t.Error(closeErr)
		}
	})
	store, err := taskdir.InitStore(owner.root)
	owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *publicContinuationFixture) assertReleaseRemoval(t *testing.T, releaseErr, cleanupErr error) {
	t.Helper()
	_, pathErr := os.Lstat(f.owner.base)
	if releaseErr != nil {
		if pathErr != nil {
			t.Fatalf("bootstrap release fault removed namespace: %v", pathErr)
		}
		if !errors.Is(cleanupErr, releaseErr) {
			t.Fatalf("bootstrap release fault lost at removal gate: %v", cleanupErr)
		}
	} else if cleanupErr != nil || !errors.Is(pathErr, os.ErrNotExist) {
		t.Fatalf("command diagnostic prevented clean removal: cleanup=%v path=%v", cleanupErr, pathErr)
	}
	if _, err := f.owner.store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("completed independent Store retained: %v", err)
	}
}

const publicContinuationClient = `#!/bin/sh
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
`
