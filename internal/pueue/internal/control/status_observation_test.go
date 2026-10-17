package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type privateTaskObservationResult struct {
	observation Observation
	err         error
}

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
	wrongBinding.ClientSHA256 = task.ComputeSHA256([]byte("other-client-generation"))
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

func TestReconcileStatusRetainsObservationOwnerUntilPendingCommandCompletes(t *testing.T) {
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
	controlPath := filepath.Join(base, "task-observation-pending-controls")
	startedPath := filepath.Join(base, "task-observation-pending-started")
	releasePath := filepath.Join(base, "task-observation-pending-release")
	environment := append(os.Environ(),
		"PRIVATE_CONTROL_MARKER="+controlPath,
		"PRIVATE_STATUS_STARTED="+startedPath,
		"PRIVATE_STATUS_RELEASE="+releasePath,
	)

	firstOptions := bound.options
	firstOptions.Environment = environment
	firstOptions.ObservationTimeout = 20 * time.Millisecond
	first := newPrivateTaskObservationClient(t, bound, firstOptions)
	firstPending := pendingPrivateTaskObservation(t, first, identity)
	waitForPrivateFixtureMarker(t, startedPath)

	secondEntered := make(chan struct{}, 1)
	secondOptions := bound.options
	secondOptions.Environment = environment
	secondOptions.ObservationTimeout = 5 * time.Second
	secondOptions.taskObservationEntryObserver = func() { secondEntered <- struct{}{} }
	second := newPrivateTaskObservationClient(t, bound, secondOptions)
	secondResult := make(chan error, 1)
	go func() {
		secondResult <- reconcilePrivateTaskObservationForTest(second, identity)
	}()
	waitForPrivateTaskObservationEntries(t, secondEntered, 1)
	select {
	case resultErr := <-secondResult:
		t.Fatalf("second caller passed an in-flight observation owner: %v", resultErr)
	case <-time.After(50 * time.Millisecond):
	}
	if got := privateTaskObservationStatusCount(t, controlPath); got != 1 {
		t.Fatalf("second snapshot started while the first command was in flight: got %d status commands", got)
	}

	releasePrivateStatus(t, releasePath)
	if !awaitPendingNaturally(firstPending, 10*time.Second) {
		t.Fatal("first status command did not complete after release")
	}
	if err := awaitPrivateTaskObservationResult(t, secondResult); err != nil {
		t.Fatalf("second status observation failed after owner completion: %v", err)
	}
	if got := privateTaskObservationStatusCount(t, controlPath); got != 2 {
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

func pendingPrivateTaskObservation(t *testing.T, client *Client, identity Identity) *Pending {
	t.Helper()
	observation, err := client.ReconcileStatus(context.Background(), identity)
	if !errors.Is(err, ErrInFlight) || observation.Pending == nil {
		t.Fatalf("short observation did not retain its command owner: observation=%+v err=%v", observation, err)
	}
	return observation.Pending
}

func reconcilePrivateTaskObservationForTest(client *Client, identity Identity) error {
	observation, err := client.ReconcileStatus(context.Background(), identity)
	if err == nil && (!observation.Matched || observation.State != StateRunning) {
		return ErrUnknown
	}
	return err
}

func awaitPrivateTaskObservationResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("second status observation did not finish")
		return nil
	}
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
