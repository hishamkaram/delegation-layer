package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/guardian"
	guardianfixture "github.com/hishamkaram/delegation-layer/internal/guardian/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// Only fixture observations live here. Workspace journals, execution and the
// production guardian retain their existing mutation and lifecycle authority.
type nativeBaselineTask struct {
	lineage      nativeLineage
	resource     *workspace.Resource
	registration workspace.Registration
	turn         nativeTurn
	td           *taskdir.TaskDir
	profile      PreparedProfile
	starts       atomic.Int32
}

const baselineOddPath = "odd\tline\nü.txt"

func newNativeBaselineTask(t *testing.T) *nativeBaselineTask {
	t.Helper()
	lineage := newNativeLineage(t)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	template, _ := newAppTaskInStore(t, lineage.store, strings.Repeat("c", 32), false, nil)
	t.Cleanup(func() { closeNativeWorktreeTestFile(t, template.Close()) })
	request, metadata, err := template.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	req, meta := *request, *metadata
	meta.SupervisorConfig.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	meta.SupervisorConfig.ClientSHA256, meta.SupervisorConfig.DaemonSHA256 = "", ""
	req.TaskID, meta.TaskID = resource.Request().TaskID, resource.Request().TaskID
	req.CanonicalCwd = registration.CanonicalCwd
	admission := workspace.Subject{Kind: workspace.SubjectAdmission, ID: req.TaskID, SHA256: req.BriefSHA256}
	reservation, err := resource.Reserve(lineage.scope.ctx, admission)
	if err != nil {
		t.Fatal(err)
	}
	req.Workspace = &task.WorkspaceBinding{ID: resource.Request().WorkspaceID, Reservation: reservation.Sequence, RequestSHA256: admission.SHA256, BudgetNanos: int64(time.Minute)}
	subject, err := workspacescore.TaskSubject(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordCaptureObligationWithBudget(lineage.scope.ctx, reservation, subject, int64(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// tee consumes the finite saved brief, writes turn files and returns the
	// same nonempty answer to the fixture predicate. No paid provider is used.
	meta.ProviderExecutable, err = filepath.EvalSymlinks("/usr/bin/tee")
	if err != nil {
		t.Fatal(err)
	}
	td, err := lineage.store.CreateTask(req.TaskID, &req, []byte("brief"), &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNativeWorktreeTestFile(t, td.Close()) })
	prepareBaselineBudgetReceipt(t, td, meta.SupervisorConfig)
	return &nativeBaselineTask{
		lineage: lineage, resource: resource, registration: registration, td: td,
		turn: nativeTurn{request: req, reservation: reservation, subject: subject},
		profile: PreparedProfile{
			Plan:            execution.Plan{Executable: meta.ProviderExecutable, Arguments: []string{"turn.txt", baselineOddPath}, Directory: req.CanonicalCwd, Predicate: meta.Predicate},
			ObservedVersion: meta.ProviderVersion, Effective: meta.EffectiveConfig,
		},
	}
}

// A numeric receipt lets the real budgetStopper use the task stop journal.
// PrepareAppBudgetRecords also creates a start guard, so it cannot precede
// the runner under test. Only submission records are prepared here.
func prepareBaselineBudgetReceipt(t *testing.T, td *taskdir.TaskDir, supervisor task.SupervisorRef) {
	t.Helper()
	permit, err := td.PrepareSubmission(supervisor)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(permit.Consume(), permit.Release()); err != nil {
		t.Fatal(err)
	}
	submission, err := td.ReadSubmission()
	if err != nil {
		t.Fatal(err)
	}
	ref := submission.Supervisor
	if err = td.RecordSupervisorReceipt(task.SupervisorReceipt{
		SchemaVersion: task.SchemaVersion, RootID: submission.RootID, TaskID: submission.TaskID,
		SpecSHA256: submission.SpecSHA256, MetaSHA256: submission.MetaSHA256, NumericTaskID: 7, Label: submission.Label,
		ConfigPath: ref.ConfigPath, ConfigDigest: ref.ConfigDigest, Endpoint: ref.Endpoint, ObservedVersion: ref.ObservedVersion,
		ClientExecutable: ref.ClientExecutable, ClientSHA256: ref.ClientSHA256, ResolvedConfigSHA256: ref.ResolvedConfigSHA256,
		ExecutableIdentity: ref.ExecutableIdentity, DaemonExecutable: ref.DaemonExecutable, DaemonSHA256: ref.DaemonSHA256,
		ResolutionOS: ref.ResolutionOS, ResolutionHome: ref.ResolutionHome, ResolutionDataLocal: ref.ResolutionDataLocal,
		ResolutionConfig: ref.ResolutionConfig, ResolutionRuntime: ref.ResolutionRuntime, ResolutionUsername: ref.ResolutionUsername, ResolutionCwd: ref.ResolutionCwd,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *nativeBaselineTask) dependencies() Dependencies {
	return Dependencies{
		ResourceChecker: fixtureResources,
		Environment:     os.Environ(),
		PrepareProfile:  func(task.TaskRecord) (PreparedProfile, error) { return f.profile, nil },
		ExecutionHooks: execution.Hooks{Start: func(command *exec.Cmd) error {
			f.starts.Add(1)
			return command.Start()
		}},
	}
}

func (f *nativeBaselineTask) run(child *openedLifetimeChild, deps Dependencies) commandResult {
	return runProvider(newResponse("dispatch"), f.lineage.store, f.td, &f.turn.request, f.profile, nil, child, deps)
}

func (f *nativeBaselineTask) baselineTemp(t *testing.T, parent string) string {
	t.Helper()
	path := filepath.Join(parent, "baseline-tmp")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", path)
	return path
}

func assertBaselineScratchEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("baseline scratch retained invocation files: entries=%v err=%v", entries, err)
	}
}

type baselineScratchInventory struct {
	entries map[string]os.FileInfo
	err     error
}

func readBaselineScratchInventory(parent string) baselineScratchInventory {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return baselineScratchInventory{err: err}
	}
	inventory := baselineScratchInventory{entries: make(map[string]os.FileInfo, len(entries))}
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			return baselineScratchInventory{err: infoErr}
		}
		inventory.entries[entry.Name()] = info
	}
	return inventory
}

// Execution binds its verified command before start-entry and preflight. Its
// portable materialization is already owned then; the baseline must add one
// distinct directory and preserve that exact verifier resource until Close.
func baselineInvocationScratch(before, during baselineScratchInventory) (string, error) {
	if err := errors.Join(before.err, during.err); err != nil {
		return "", err
	}
	if len(before.entries) > 1 || len(during.entries) != len(before.entries)+1 {
		return "", fmt.Errorf("want one new baseline directory: before=%v during=%v", before.entries, during.entries)
	}
	for name, prior := range before.entries {
		current, present := during.entries[name]
		if !prior.IsDir() || !strings.HasPrefix(name, "delegation-layer-verified-") || !present || !os.SameFile(prior, current) {
			return "", fmt.Errorf("preflight did not preserve its bound verifier directory %q", name)
		}
	}
	for name, current := range during.entries {
		if _, inherited := before.entries[name]; inherited {
			continue
		}
		if !current.IsDir() || !strings.HasPrefix(name, "delegate-turn-baseline-") {
			return "", fmt.Errorf("unexpected preflight scratch entry %q", name)
		}
		return name, nil
	}
	return "", errors.New("acknowledged Git command has no invocation scratch")
}

func assertBaselineInvocationScratch(t *testing.T, parent string, before <-chan baselineScratchInventory) string {
	t.Helper()
	var initial baselineScratchInventory
	select {
	case initial = <-before:
	default:
		t.Fatal("Git acknowledged entry before the execution start-entry observation")
	}
	name, err := baselineInvocationScratch(initial, readBaselineScratchInventory(parent))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scratch owners while Git is blocked: bound_verifier=%d baseline=%s", len(initial.entries), name)
	return filepath.Join(parent, name)
}

func TestBaselineInvocationScratchKeepsExactOwners(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"delegate-turn-baseline-1", "delegate-turn-baseline-2", "delegation-layer-verified-1", "delegation-layer-verified-2", "unexpected"} {
		if err := os.Mkdir(filepath.Join(parent, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	observed := readBaselineScratchInventory(parent)
	if observed.err != nil {
		t.Fatal(observed.err)
	}
	baseline := observed.entries["delegate-turn-baseline-1"]
	verifier := observed.entries["delegation-layer-verified-1"]
	before := baselineScratchInventory{entries: map[string]os.FileInfo{"delegation-layer-verified-1": verifier}}
	valid := baselineScratchInventory{entries: map[string]os.FileInfo{"delegation-layer-verified-1": verifier, "delegate-turn-baseline-1": baseline}}
	if name, err := baselineInvocationScratch(before, valid); err != nil || name != "delegate-turn-baseline-1" {
		t.Fatalf("valid preflight owners: baseline=%q err=%v", name, err)
	}
	cases := map[string]baselineScratchInventory{
		"missing baseline": before,
		"second baseline": {entries: map[string]os.FileInfo{
			"delegation-layer-verified-1": verifier, "delegate-turn-baseline-1": baseline, "delegate-turn-baseline-2": observed.entries["delegate-turn-baseline-2"],
		}},
		"unexpected sibling": {entries: map[string]os.FileInfo{
			"delegation-layer-verified-1": verifier, "unexpected": observed.entries["unexpected"],
		}},
		"replaced verifier": {entries: map[string]os.FileInfo{
			"delegation-layer-verified-1": observed.entries["delegation-layer-verified-2"], "delegate-turn-baseline-1": baseline,
		}},
		"missing verifier": {entries: map[string]os.FileInfo{"delegate-turn-baseline-1": baseline}},
	}
	for name, during := range cases {
		t.Run(name, func(t *testing.T) {
			if directory, err := baselineInvocationScratch(before, during); err == nil {
				t.Fatalf("accepted invalid scratch ownership: baseline=%q", directory)
			}
		})
	}
}

// Configure only records the exact exec.Cmd that execution later waits. All
// native behavior is forwarded to the separately opened production handle.
type baselineCommandWitness struct {
	*lifetime.Scope
	mu       sync.Mutex
	commands []*exec.Cmd
}

func (w *baselineCommandWitness) Configure(command *exec.Cmd) error {
	w.mu.Lock()
	w.commands = append(w.commands, command)
	w.mu.Unlock()
	return w.Scope.Configure(command)
}

type baselineGuardian struct {
	child       *openedLifetimeChild
	witness     *baselineCommandWitness
	owner       *nativeFixtureOwner
	workerInput *os.File
	done        chan struct{}
	err         error // published by closing done
}

// Compose the production state loader, guardian and worker client. The finite
// cat worker stands in for the app caller; guardian owns its one Start/Wait.
// The guardian binds this test process, so it is the only process owner until
// it joins: lineage queries are refused for that whole window.
func (f *nativeBaselineTask) guardian(t *testing.T, budget time.Duration) *baselineGuardian {
	t.Helper()
	state, err := loadLifetimeGuardianRunState(f.lineage.store, runnerArguments{TaskID: f.td.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNativeWorktreeTestFile(t, state.closeOwner()) })
	deadline := time.Now().Add(budget)
	parent, connection := net.Pipe()
	input, writer := nativeWorktreeTestPipe(t)
	worker := exec.Command("/bin/cat")
	worker.Stdin = input
	ready := make(chan struct{})
	f.lineage.owner.live.Store(true)
	owner, err := guardian.New(guardian.Options{
		RootID: f.lineage.store.RootID, TaskID: f.td.TaskID, OwnerKind: state.ownerKind, Deadline: deadline,
		Records: state.records, AcquireOwner: state.acquireOwner, ReconcileOwned: state.reconcileOwned,
		Connection: parent, Worker: worker,
		StartWorker: func(command *exec.Cmd) error {
			startErr := command.Start()
			if startErr == nil {
				close(ready)
			}
			return startErr
		},
	})
	if err != nil {
		f.lineage.owner.live.Store(false)
		t.Fatal(err)
	}
	fixture := &baselineGuardian{owner: f.lineage.owner, workerInput: writer, done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(f.lineage.scope.ctx, time.Minute)
	go func() { fixture.err = owner.Run(ctx); close(fixture.done) }()
	t.Cleanup(func() {
		closeNativeWorktreeTestFile(t, writer.Close())
		if fixture.child != nil {
			closeNativeWorktreeTestFile(t, fixture.child.Close())
		} else if fixture.witness != nil {
			closeNativeWorktreeTestFile(t, guardian.ReleaseScopeUncanceled(context.Background(), fixture.witness))
		}
		guardianfixture.CloseConnection(t, connection)
		guardianfixture.CloseConnection(t, parent)
		cancel()
		<-fixture.done
		fixture.owner.live.Store(false)
	})
	select {
	case <-ready:
	case <-fixture.done:
		t.Fatalf("guardian setup: %v", fixture.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	fixture.witness = openBaselineCommandWitness(t, f.td)
	client, err := guardian.NewWorkerClient(connection, fixture.witness, f.td.TaskID, deadline, guardian.NewLaunchGateFactory(f.lineage.scope.runner))
	if err != nil {
		t.Fatal(err)
	}
	fixture.child = &openedLifetimeChild{control: client, deadline: deadline}
	return fixture
}

func openBaselineCommandWitness(t *testing.T, td *taskdir.TaskDir) *baselineCommandWitness {
	t.Helper()
	registration, intent, err := readLifetimeChildRecords(td)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lifetime.NewPlan(lifetime.Kind(intent.ScopeKind), lifetime.Assurance(intent.Assurance), intent.ScopePlan)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := lifetime.NewRef(lifetime.Kind(registration.ScopeKind), lifetime.Assurance(registration.Assurance), registration.ScopeID)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := lifetime.OpenRegistered(plan, ref)
	if err != nil {
		t.Fatal(err)
	}
	return &baselineCommandWitness{Scope: scope}
}

func (g *baselineGuardian) finish(t *testing.T) error {
	t.Helper()
	closeNativeWorktreeTestFile(t, g.workerInput.Close())
	select {
	case <-g.done:
		g.owner.live.Store(false)
		return g.err
	case <-time.After(15 * time.Second):
		t.Fatal("native guardian did not finalize and join its worker")
		return nil
	}
}

// A nondeadline guardian joins its finite cat worker's ordinary exit; no
// fixture process claimed the worker's process owner.
func assertBaselineWorkerExited(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	wait, err := td.ReadLifetimeWorkerWait()
	if err != nil || wait.Result != task.LifetimeWorkerWaitSuccess || wait.ExitCode == nil || *wait.ExitCode != 0 {
		t.Fatalf("native guardian did not join an ordinary worker exit: %+v, %v", wait, err)
	}
}

func (f *nativeBaselineTask) assertRefused(t *testing.T, owner *baselineGuardian, result commandResult) {
	t.Helper()
	if result.response.Outcome == nil || result.response.Publication != task.PublicationRejected.String() {
		t.Fatalf("baseline failure launched or failed to reject: starts=%d result=%+v err=%v", f.starts.Load(), result.response, result.err)
	}
	f.assertNeverLaunched(t, owner)
}

func (f *nativeBaselineTask) assertNeverLaunched(t *testing.T, owner *baselineGuardian) {
	t.Helper()
	inspection, err := f.td.Inspect()
	if err != nil || inspection.StartedExists || f.starts.Load() != 0 {
		t.Fatalf("refused launch has a provider start: inspection=%+v starts=%d err=%v", inspection, f.starts.Load(), err)
	}
	f.assertNoBaselineJournal(t)
	replay := f.run(owner.child, f.dependencies())
	if !errors.Is(replay.err, task.ErrAlreadyStarted) || replay.code == 0 || f.starts.Load() != 0 {
		t.Fatalf("baseline refusal replay: starts=%d code=%d err=%v", f.starts.Load(), replay.code, replay.err)
	}
}

func (f *nativeBaselineTask) baselineRef() string {
	return workspacescore.ManagedBaselineRef(f.lineage.store.RootID, f.resource.Request().WorkspaceID, f.turn.reservation.Sequence)
}

func (f *nativeBaselineTask) assertNoBaselineJournal(t *testing.T) {
	t.Helper()
	if _, err := f.resource.TurnBaseline(f.lineage.scope.ctx, f.turn.reservation.Sequence); !workspace.OperationAbsent(err) {
		t.Fatalf("refused launch published a baseline: %v", err)
	}
}

// The ref query is an independent native launch, so it runs only after the
// guardian joins. The fixture guardian captures nothing after the runner
// returns, so the deferred absence still describes the refused launch.
func (f *nativeBaselineTask) assertNoBaselineRef(t *testing.T) {
	t.Helper()
	if got := f.lineage.gitIn(t, f.lineage.main, "for-each-ref", "--format=%(refname)", f.baselineRef()); got != "" {
		t.Fatalf("refused launch pinned a baseline: %s", got)
	}
}

func TestManagedTurnBaselineExpiredRunnerRefusesProvider(t *testing.T) {
	fixture := newNativeBaselineTask(t)
	scratch := fixture.baselineTemp(t, fixture.lineage.root)
	owner := fixture.guardian(t, time.Minute)
	// Inject the already elapsed runner deadline at its fixed input boundary;
	// the live guardian remains available to acknowledge the canonical stop.
	expired := *owner.child
	expired.deadline = time.Now().Add(-time.Second)
	result := fixture.run(&expired, fixture.dependencies())
	fixture.assertRefused(t, owner, result)
	assertBaselineScratchEmpty(t, scratch)
	owner.witness.mu.Lock()
	commands := len(owner.witness.commands)
	owner.witness.mu.Unlock()
	if commands != 0 {
		t.Fatalf("expired runner entered %d native commands", commands)
	}
	if err := owner.finish(t); err != nil {
		t.Fatalf("expired runner guardian finalization: %v", err)
	}
	fixture.assertNoBaselineRef(t)
	assertBaselineStop(t, fixture, expired.deadline)
	assertBaselineStopAcknowledged(t, fixture.td)
}

func TestManagedTurnBaselineScratchInsideCheckoutRefusesProvider(t *testing.T) {
	fixture := newNativeBaselineTask(t)
	scratch := fixture.baselineTemp(t, fixture.registration.CheckoutPath)
	owner := fixture.guardian(t, time.Minute)
	result := fixture.run(owner.child, fixture.dependencies())
	fixture.assertRefused(t, owner, result)
	assertBaselineScratchEmpty(t, scratch)
	diagnostic, err := os.ReadFile(filepath.Join(fixture.td.Dir, "raw", "stderr"))
	if err != nil || !strings.Contains(string(diagnostic), workspace.ErrInvalidGitContext.Error()) {
		t.Fatalf("runner lost the explicit scratch refusal: %q, %v", diagnostic, err)
	}
	if err = owner.finish(t); err != nil {
		t.Fatalf("scratch refusal guardian completion: %v", err)
	}
	assertBaselineWorkerExited(t, fixture.td)
	fixture.assertNoBaselineRef(t)
	// The runner persists the preflight diagnostic in its seal; the writer's
	// typed cause must also survive cleanup, rather than become os.Remove's error.
	// Its lineage Git is an independent launch, so it runs after the guardian.
	if err = fixture.lineage.observeBaseline(fixture.turn.request, os.Environ()); !errors.Is(err, workspace.ErrInvalidGitContext) {
		t.Fatalf("overlapping scratch lost its typed cause: %v", err)
	}
	assertBaselineScratchEmpty(t, scratch)
}

func assertBaselineStop(t *testing.T, fixture *nativeBaselineTask, deadline time.Time) {
	t.Helper()
	request, err := fixture.td.ReadStopRequest("budget")
	if err != nil || request.Cause != "budget" || request.Deadline != deadline.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("canonical task deadline stop = %+v, %v", request, err)
	}
	termination, err := fixture.td.ReadLifetimeTermination()
	if err != nil || !termination.ScopeEmpty || termination.Cause != task.LifetimeTerminationBudget {
		t.Fatalf("native termination is not independently empty: %+v, %v", termination, err)
	}
	wait, err := fixture.td.ReadLifetimeWorkerWait()
	if err != nil || wait.ChildPID <= 0 || wait.WorkerSHA256 == "" {
		t.Fatalf("native guardian did not persist exact worker Wait: %+v, %v", wait, err)
	}
	t.Logf("native budget proof: request=%+v termination=%+v worker_wait=%+v", request, termination, wait)
}

func assertBaselineStopAcknowledged(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	reply, err := td.ReadStopReply("budget")
	if err != nil || !reply.Acknowledged || reply.Action != "terminate" {
		t.Fatalf("native task stop was not acknowledged: %+v, %v", reply, err)
	}
}

func TestManagedTurnBaselineDeadlineDuringGitStopsAndJoins(t *testing.T) {
	fixture := newNativeBaselineTask(t)
	scratch := fixture.baselineTemp(t, fixture.lineage.root)
	shim := installBaselineGitShim(t, fixture.lineage.root, fixture.baselineRef())
	ready, hold := installBaselineGitBarrier(t, shim)
	owner := fixture.guardian(t, 10*time.Second)
	before := make(chan baselineScratchInventory, 1)
	deps := fixture.dependencies()
	deps.ExecutionHooks.Event = func(event string) {
		if event == "start-entry" {
			before <- readBaselineScratchInventory(scratch)
		}
	}
	done := make(chan commandResult, 1)
	go func() { done <- fixture.run(owner.child, deps) }()
	// Cleanup releases the barrier on every assertion failure and joins the
	// runner before any fixture resource or task directory can be closed.
	joined := false
	t.Cleanup(func() {
		if _, err := io.WriteString(hold, "release\n"); err != nil {
			t.Error(err)
		}
		if !joined {
			<-done
		}
	})
	if early, finished := awaitBaselineGitEntry(t, ready, done); finished {
		joined = true
		t.Fatalf("runner ended before baseline Git acknowledged entry: code=%d err=%v", early.code, early.err)
	}
	fixture.assertNoBaselineJournal(t)
	if err := shim.heldRefAbsent(); err != nil {
		t.Fatal(err)
	}
	invocationScratch := assertBaselineInvocationScratch(t, scratch, before)
	var result commandResult
	select {
	case result = <-done:
		joined = true
	case <-time.After(30 * time.Second):
		t.Fatal("task deadline did not join baseline Git within the independent watchdog")
	}
	assertBaselineDeadlineDisposition(t, result)
	fixture.assertNeverLaunched(t, owner)
	t.Logf("deadline guardian joined: %v", owner.finish(t))
	fixture.assertNoBaselineRef(t)
	assertBaselineStop(t, fixture, owner.child.deadline)
	if reply, replyErr := fixture.td.ReadStopReply("budget"); task.RecordAbsent(replyErr) {
		t.Log("stop reply absent; native budget termination is independently durable")
	} else if replyErr != nil || !reply.Acknowledged || reply.Action != "terminate" {
		t.Fatalf("invalid budget stop reply: %+v, %v", reply, replyErr)
	}
	assertBaselineGitWait(t, owner.witness, invocationScratch)
	assertBaselineScratchEmpty(t, scratch)
}

// The guardian and execution share one deadline. Either a successful stop
// handshake seals rejection, or guardian closure leaves a control error and
// unknown publication. Neither replaces the native stop and exact Wait proof.
func assertBaselineDeadlineDisposition(t *testing.T, result commandResult) {
	t.Helper()
	if result.err == nil {
		if result.response.Outcome == nil || result.response.Publication != task.PublicationRejected.String() {
			t.Fatalf("deadline did not reject the unstarted provider: %+v", result.response)
		}
	} else if result.code == 0 {
		t.Fatalf("deadline control failure returned success: %v", result.err)
	}
	t.Logf("deadline runner result: code=%d publication=%s err=%v", result.code, result.response.Publication, result.err)
}

func awaitBaselineGitEntry(t *testing.T, ready *os.File, done <-chan commandResult) (commandResult, bool) {
	t.Helper()
	entered := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		var token [5]byte
		_, err := io.ReadFull(ready, token[:])
		if err == nil && string(token[:]) != "ready" {
			err = errors.New("invalid baseline Git acknowledgement")
		}
		entered <- err
	}()
	t.Cleanup(func() {
		closeNativeWorktreeTestFile(t, ready.Close())
		<-exited
	})
	select {
	case err := <-entered:
		if err != nil {
			t.Fatal(err)
		}
	case result := <-done:
		return result, true
	case <-time.After(15 * time.Second):
		t.Fatal("native baseline Git did not acknowledge entry")
	}
	return commandResult{}, false
}

func assertBaselineGitWait(t *testing.T, witness *baselineCommandWitness, invocationScratch string) {
	t.Helper()
	witness.mu.Lock()
	defer witness.mu.Unlock()
	matched := 0
	for _, command := range witness.commands {
		if !strings.Contains(strings.Join(command.Args, " "), "add --sparse -A") {
			continue
		}
		matched++
		if command.ProcessState == nil {
			t.Fatal("baseline Git has no joined exec.Cmd.Wait result")
		}
		status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() {
			t.Fatalf("blocked baseline Git did not terminate natively: %v", command.ProcessState)
		}
		indexes := 0
		for _, variable := range command.Env {
			if index, present := strings.CutPrefix(variable, "GIT_INDEX_FILE="); present {
				indexes++
				if filepath.Dir(index) != invocationScratch {
					t.Fatalf("joined Git used scratch %q, want the acknowledged invocation %q", index, invocationScratch)
				}
			}
		}
		if indexes != 1 {
			t.Fatalf("joined Git has %d private index bindings, want exactly one", indexes)
		}
		t.Logf("baseline Git exact joined Wait: pid=%d state=%s", command.ProcessState.Pid(), command.ProcessState)
	}
	if matched != 1 {
		t.Fatalf("joined %d baseline add commands, want exactly one", matched)
	}
}
