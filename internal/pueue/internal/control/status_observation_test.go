package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

type privateTaskObservationResult struct {
	observation Observation
	err         error
}

func (r privateTaskObservationResult) pendingCommand() *Pending { return r.observation.Pending }

func TestReconcileStatusSharesOnePrivateSnapshotAcrossOverlappingClients(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	bound := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := bound.Close(); err != nil {
			t.Error(err)
		}
	})

	const callers = 16
	identities := privateTaskObservationTestIdentities(callers)
	writePrivateTaskObservationTestStatus(t, fixture.stateRoot, identities)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	controlPath := filepath.Join(base, "task-observation-controls")
	startedPath := filepath.Join(base, "task-observation-started")
	releasePath := filepath.Join(base, "task-observation-release")
	entered := make(chan struct{}, callers)
	environment := append(os.Environ(),
		"PRIVATE_CONTROL_MARKER="+controlPath,
		"PRIVATE_STATUS_STARTED="+startedPath,
		"PRIVATE_STATUS_RELEASE="+releasePath,
	)

	clients := privateTaskObservationClients(t, bound, identities, environment, entered)

	start := make(chan struct{})
	results := make(chan privateTaskObservationResult, callers)
	workers := startPrivateTaskObservationCallers(clients, identities, start, results)
	close(start)
	waitForPrivateTaskObservationEntries(t, entered, callers)
	waitForPrivateFixtureMarker(t, startedPath)
	releasePrivateStatus(t, releasePath)
	workers.Wait()
	close(results)
	assertPrivateTaskObservationResults(t, results)
	if got := privateTaskObservationStatusCount(t, controlPath); got != 1 {
		t.Fatalf("overlapping status calls started %d queue snapshots, want 1", got)
	}

	clients[0].options.taskObservationEntryObserver = nil
	observation, err := clients[0].ReconcileStatus(context.Background(), identities[0])
	if err != nil || !observation.Matched {
		t.Fatalf("later status call failed: observation=%+v err=%v", observation, err)
	}
	if got := privateTaskObservationStatusCount(t, controlPath); got != 2 {
		t.Fatalf("later status call reused a stale flight: status calls=%d, want 2", got)
	}
}

func privateTaskObservationClients(t *testing.T, bound *Client, identities []Identity, environment []string, entered chan<- struct{}) []*Client {
	t.Helper()
	clients := make([]*Client, len(identities))
	for index := range clients {
		options := bound.options
		options.Environment = environment
		options.taskObservationEntryObserver = func() { entered <- struct{}{} }
		client, err := NewClient(bound.Binding(), options)
		if err != nil {
			t.Fatal(err)
		}
		clients[index] = client
	}
	return clients
}

func startPrivateTaskObservationCallers(clients []*Client, identities []Identity, start <-chan struct{}, results chan<- privateTaskObservationResult) *sync.WaitGroup {
	var workers sync.WaitGroup
	workers.Add(len(clients))
	for index, client := range clients {
		go func() {
			defer workers.Done()
			<-start
			observation, err := client.ReconcileStatus(context.Background(), identities[index])
			results <- privateTaskObservationResult{observation: observation, err: err}
		}()
	}
	return &workers
}

func waitForPrivateTaskObservationEntries(t *testing.T, entered <-chan struct{}, count int) {
	t.Helper()
	for range count {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("private status caller did not enter the shared observation")
		}
	}
}

func assertPrivateTaskObservationResults(t *testing.T, results <-chan privateTaskObservationResult) {
	t.Helper()
	for result := range results {
		if result.err != nil || !result.observation.Matched || result.observation.State != StateRunning {
			t.Fatalf("shared private status observation failed: observation=%+v err=%v", result.observation, result.err)
		}
	}
}

func TestReconcileStatusReplacesMalformedAndWrongGenerationSnapshots(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	bound := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := bound.Close(); err != nil {
			t.Error(err)
		}
	})
	identities := privateTaskObservationTestIdentities(1)
	writePrivateTaskObservationTestStatus(t, fixture.stateRoot, identities)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	controlPath := filepath.Join(base, "task-observation-repair-controls")
	options := bound.options
	options.Environment = append(os.Environ(), "PRIVATE_CONTROL_MARKER="+controlPath)
	client, err := NewClient(bound.Binding(), options)
	if err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(privateTaskObservationPath(fixture.stateRoot), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if observation, reconcileErr := client.ReconcileStatus(context.Background(), identities[0]); reconcileErr != nil || !observation.Matched {
		t.Fatalf("malformed derived snapshot did not self-heal: observation=%+v err=%v", observation, reconcileErr)
	}

	wrongBinding := bound.RuntimeBinding()
	wrongBinding.ConfigDigest = task.ComputeSHA256([]byte("other config generation"))
	label := identities[0].Label()
	if err = publishPrivateTaskObservation(fixture.stateRoot, wrongBinding, QueueSnapshot{Jobs: []Job{{ID: 7, Label: &label, State: StateRunning}}}); err != nil {
		t.Fatal(err)
	}
	if observation, reconcileErr := client.ReconcileStatus(context.Background(), identities[0]); reconcileErr != nil || !observation.Matched {
		t.Fatalf("wrong-generation snapshot did not self-heal: observation=%+v err=%v", observation, reconcileErr)
	}
	if got := privateTaskObservationStatusCount(t, controlPath); got != 2 {
		t.Fatalf("snapshot repair started %d status commands, want 2", got)
	}
}

func TestPrivateTaskObservationReadinessIsProvisionedByLifecycleOwner(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	bound := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := bound.Close(); err != nil {
			t.Error(err)
		}
	})
	lockPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTaskObservationLockName)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if ready, err := PrivateTaskObservationReady(fixture.stateRoot); err != nil || ready {
		t.Fatalf("missing observation lock reported ready: ready=%t err=%v", ready, err)
	}
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return EnsurePrivateTaskObservationWithBootstrapLock(fixture.stateRoot)
	}); err != nil {
		t.Fatal(err)
	}
	if ready, err := PrivateTaskObservationReady(fixture.stateRoot); err != nil || !ready {
		t.Fatalf("lifecycle owner did not provision observation lock: ready=%t err=%v", ready, err)
	}
}

// The first caller is canceled at the exact start of its native status
// command, so its observation timeout stays an ordinary setup bound. The
// pre-start variant holds the observation lock first: waiting before the
// command starts must not cancel anything.
func TestReconcileStatusRetainsObservationOwnerUntilPendingCommandCompletes(t *testing.T) {
	for _, preStartLock := range []bool{false, true} {
		name := "command start"
		if preStartLock {
			name = "pre-start lock wait"
		}
		t.Run(name, func(t *testing.T) { runPendingPrivateTaskObservationCase(t, preStartLock) })
	}
}

// pendingStatusScenario is one bound private supervisor whose fake status
// command blocks until the test releases it. scope owns both callers, their
// contexts and the release marker.
type pendingStatusScenario struct {
	bound       *Client
	stateRoot   string
	controlPath string
	startedPath string
	releasePath string
	environment []string
	identity    Identity
	scope       *testCallScope
}

// privateStatusStart records the first caller's shared-observation entry and
// the exact start of its native status snapshot command.
type privateStatusStart struct {
	entered chan struct{}
	started atomic.Bool
}

func runPendingPrivateTaskObservationCase(t *testing.T, preStartLock bool) {
	fixture := newPrivateSupervisorTestFixture(t)
	bound := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := bound.Close(); err != nil {
			t.Error(err)
		}
	})
	identity := privateTaskObservationTestIdentities(1)[0]
	writePrivateTaskObservationTestStatus(t, fixture.stateRoot, []Identity{identity})
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	scenario := pendingStatusScenario{
		bound: bound, stateRoot: fixture.stateRoot, identity: identity,
		controlPath: filepath.Join(base, "task-observation-pending-controls"),
		startedPath: filepath.Join(base, "task-observation-pending-started"),
		releasePath: filepath.Join(base, "task-observation-pending-release"),
		scope:       newTestCallScope(t, bound.options),
	}
	scenario.scope.onRelease(func() { releasePrivateStatusOnCleanup(t, scenario.releasePath) })
	scenario.environment = append(os.Environ(),
		"PRIVATE_CONTROL_MARKER="+scenario.controlPath,
		"PRIVATE_STATUS_STARTED="+scenario.startedPath,
		"PRIVATE_STATUS_RELEASE="+scenario.releasePath,
	)
	firstContext, cancelFirst := scenario.scope.cancelableContext()
	firstOptions := bound.options
	firstOptions.Environment = scenario.environment
	firstStart := cancelAtStatusStart(&firstOptions, cancelFirst)
	first := newPrivateTaskObservationClient(t, bound, firstOptions)
	held := holdPrivateTaskObservationLock(t, base, preStartLock)
	firstCall := scenario.start(first, firstContext)
	if held != nil {
		awaitEntry(scenario.scope, firstStart.entered, firstCall, "first caller did not enter the shared observation")
		if firstStart.started.Load() {
			t.Fatal("first status command started while the observation lock was still held")
		}
		if err := closeBootstrapLock(held); err != nil {
			t.Fatal(err)
		}
	}
	firstPending := pendingPrivateTaskObservation(t, scenario.scope, firstCall, firstStart)
	waitForPrivateFixtureMarker(t, scenario.startedPath)
	scenario.assertSecondCallerWaits(t, firstPending)
}

// cancelAtStatusStart installs observers that only record facts: the caller's
// entry, and the started event of its status snapshot command, which cancels
// the caller's context.
func cancelAtStatusStart(options *Options, cancel context.CancelFunc) *privateStatusStart {
	start := &privateStatusStart{entered: make(chan struct{}, 1)}
	options.taskObservationEntryObserver = func() {
		select {
		case start.entered <- struct{}{}:
		default:
		}
	}
	options.Observer = func(event CommandEvent) {
		if event.Stage == "started" && slices.Contains(event.Argv, "status") && slices.Contains(event.Argv, "--json") {
			start.started.Store(true)
			cancel()
		}
	}
	return start
}

func (s pendingStatusScenario) start(client *Client, ctx context.Context) *testCall[privateTaskObservationResult] {
	return startTestCall(s.scope, func() privateTaskObservationResult {
		observation, err := client.ReconcileStatus(ctx, s.identity)
		return privateTaskObservationResult{observation: observation, err: err}
	})
}

// holdPrivateTaskObservationLock takes the published observation lock before
// the first caller starts, when requested.
func holdPrivateTaskObservationLock(t *testing.T, base string, hold bool) *bootstrapLock {
	t.Helper()
	if !hold {
		return nil
	}
	lock, err := acquireExistingSupervisorLock(t.Context(), filepath.Join(base, privateTaskObservationLockName), unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := closeBootstrapLock(lock); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return lock
}

// assertSecondCallerWaits requires a later caller to enter, start no status
// command and find the observation lock still owned by the first caller's
// pending command; after release both observations complete naturally.
func (s pendingStatusScenario) assertSecondCallerWaits(t *testing.T, firstPending *Pending) {
	t.Helper()
	secondEntered := make(chan struct{}, 1)
	secondOptions := s.bound.options
	secondOptions.Environment = s.environment
	secondOptions.taskObservationEntryObserver = func() { secondEntered <- struct{}{} }
	second := newPrivateTaskObservationClient(t, s.bound, secondOptions)
	secondCall := s.start(second, s.scope.context())
	awaitEntry(s.scope, secondEntered, secondCall, "second caller did not enter the shared observation")
	if got := privateTaskObservationStatusCount(t, s.controlPath); got != 1 {
		t.Fatalf("second snapshot started while the first command was in flight: got %d status commands", got)
	}
	// A nonblocking exclusive acquisition skips its callback when the lock is
	// busy; running it would prove the first caller lost its lease.
	acquired := false
	probeErr := withPublishedPrivateLock(t.Context(), s.stateRoot, privateTaskObservationLockName, unix.LOCK_EX|unix.LOCK_NB, func() error {
		acquired = true
		return nil
	})
	if probeErr != nil || acquired {
		t.Fatalf("pending first observation did not retain the observation lock: acquired=%t err=%v", acquired, probeErr)
	}
	releasePrivateStatus(t, s.releasePath)
	if !awaitPendingNaturally(firstPending, time.Until(s.scope.cutoff)) {
		t.Fatal("first status command did not complete after release")
	}
	result := awaitCall(s.scope, secondCall, "second status observation")
	if result.err != nil || !result.observation.Matched || result.observation.State != StateRunning {
		t.Fatalf("second status observation failed after owner completion: observation=%+v err=%v", result.observation, result.err)
	}
	if got := privateTaskObservationStatusCount(t, s.controlPath); got != 2 {
		t.Fatalf("post-pending observation started %d status commands, want 2", got)
	}
}

func newPrivateTaskObservationClient(t *testing.T, bound *Client, options Options) *Client {
	t.Helper()
	client, err := NewClient(bound.Binding(), options)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// pendingPrivateTaskObservation awaits the first caller, which its own
// context canceled at the exact status command start, and requires the
// in-flight command it still owns.
func pendingPrivateTaskObservation(t *testing.T, scope *testCallScope, call *testCall[privateTaskObservationResult], start *privateStatusStart) *Pending {
	t.Helper()
	result := awaitCall(scope, call, "first status observation")
	if !start.started.Load() || !errors.Is(result.err, ErrInFlight) || result.observation.Pending == nil {
		t.Fatalf("canceled observation did not retain its started command owner: started=%t observation=%+v err=%v", start.started.Load(), result.observation, result.err)
	}
	return result.observation.Pending
}

func privateTaskObservationTestIdentities(count int) []Identity {
	identities := make([]Identity, count)
	for index := range identities {
		identities[index] = Identity{
			RootID:     strings.Repeat("a", 32),
			TaskID:     fmt.Sprintf("%032x", index+1),
			SpecSHA256: strings.Repeat("b", 64),
			MetaSHA256: strings.Repeat("c", 64),
		}
	}
	return identities
}

func writePrivateTaskObservationTestStatus(t *testing.T, stateRoot string, identities []Identity) {
	t.Helper()
	jobs := make(map[string]any, len(identities))
	for index, identity := range identities {
		id := int64(index + 1)
		jobs[strconv.FormatInt(id, 10)] = statusTestJob(id, identity.Label(), statusStreamTestRunningState())
	}
	data := statusTestPayload(t, jobs, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": uint64(0)}})
	if err := os.WriteFile(filepath.Join(stateRoot, privateSupervisorDirectory, "test-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func privateTaskObservationStatusCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == "status" {
			count++
		}
	}
	return count
}
