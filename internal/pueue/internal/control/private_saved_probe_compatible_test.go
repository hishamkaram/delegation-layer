package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Finite shell commands and canonical records model protocol policy only.
// These fixtures neither launch a native pueued nor prove resident authority.
func TestProbeSavedPrivateCompatibleHealthyExactIgnoresSelector(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	for _, saved := range []task.SupervisorRef{f.saved, f.successor} {
		before := snapshotSavedProbeTestState(t, f.stateRoot)
		called := 0
		selector := func(context.Context) (string, string, error) {
			called++
			return "", "", errors.New("unused install unavailable")
		}
		client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, saved, f.options, selector)
		closeSavedProbeTestClient(t, client, err)
		if err != nil || client == nil || called != 0 || client.Binding() != saved || client.RuntimeBinding() != f.successor {
			t.Fatalf("healthy exact selection: client=%t calls=%d err=%v", client != nil, called, err)
		}
		assertSavedProbeTestUnchanged(t, f.stateRoot, before)
		assertPrivateDaemonStartCount(t, f.stateRoot, 2)
	}
}

func TestProbeSavedPrivateCompatibleUnavailablePairReadsThroughD(t *testing.T) {
	for _, damage := range []string{"missing client", "missing daemon"} {
		t.Run(damage, func(t *testing.T) {
			f := newCompletedPrivateSuccessorTestFixture(t, true)
			clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(f.base, "available-D"))
			damageCompatibleProbePair(t, f, damage)
			before := snapshotSavedProbeTestState(t, f.stateRoot)
			called := 0
			selector := func(ctx context.Context) (string, string, error) {
				called++
				assertCompatibleProbeHoldsSharedLease(t, ctx, f.stateRoot)
				return clientPath, daemonPath, nil
			}
			client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, selector)
			closeSavedProbeTestClient(t, client, err)
			if err != nil || client == nil || called != 1 || client.Binding() != f.saved || client.RuntimeBinding().ClientExecutable != clientPath || client.RuntimeBinding().DaemonExecutable != daemonPath {
				t.Fatalf("compatible read: client=%t calls=%d err=%v", client != nil, called, err)
			}
			if !task.SameSupervisorIdentity(client.RuntimeBinding(), f.successor) {
				t.Fatal("D changed the active config or endpoint")
			}
			assertSavedProbeTestUnchanged(t, f.stateRoot, before)
			assertPrivateDaemonStartCount(t, f.stateRoot, 2)
		})
	}
}

func TestProbeSavedPrivateTaskObservationSeparatesCompatibilitySnapshotsFromSharedRows(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(f.base, "available-D"))
	damageCompatibleProbePair(t, f, "missing client")
	damageCompatibleProbePair(t, f, "missing daemon")
	const callers = 16
	identities := privateTaskObservationTestIdentities(callers)
	writePrivateTaskObservationTestStatus(t, f.stateRoot, identities)
	controlPath := filepath.Join(f.stateRoot, privateSupervisorDirectory, "compatible-observation-controls")
	options := f.options
	options.Environment = append(os.Environ(), "PRIVATE_CONTROL_MARKER="+controlPath)
	selected := 0
	selector := func(context.Context) (string, string, error) {
		selected++
		return clientPath, daemonPath, nil
	}
	clients := make([]*Client, 0, callers)
	for index := range callers {
		client, err := ProbeSavedPrivateTaskObservation(t.Context(), f.stateRoot, f.saved, options, selector)
		t.Cleanup(func() { closeSavedProbeTestClient(t, client, err) })
		if err != nil || client == nil {
			t.Fatalf("compatible observation %d: client=%t err=%v", index, client != nil, err)
		}
		if client.Binding() != f.saved || client.RuntimeBinding().ClientExecutable != clientPath || client.RuntimeBinding().DaemonExecutable != daemonPath || !task.SameSupervisorIdentity(client.RuntimeBinding(), f.successor) {
			t.Fatal("compatible observation changed the saved identity or selected runtime")
		}
		if got := privateTaskObservationStatusCount(t, controlPath); got != index+1 || selected != index+1 {
			t.Fatalf("compatible observation %d: snapshots=%d selections=%d, want %d each", index, got, selected, index+1)
		}
		clients = append(clients, client)
	}

	assertCompatibleProbeSharedRows(t, clients, identities, controlPath)
	current, found, err := loadPrivateSupervisorBinding(f.stateRoot)
	if err != nil || !found || current != f.successor {
		t.Fatalf("observation changed the canonical generation: found=%t err=%v", found, err)
	}
	assertPrivateDaemonStartCount(t, f.stateRoot, 2)
}

func assertCompatibleProbeSharedRows(t *testing.T, clients []*Client, identities []Identity, controlPath string) {
	t.Helper()
	// ReconcileStatus owns row sharing. Hold every caller immediately after it
	// reads the entry flight ID, so compatibility reads cannot imply overlap.
	callers := len(clients)
	scope := newTestCallScope(t, clients[0].options)
	entered := make(chan struct{}, callers)
	released, release := scope.barrier()
	calls := make([]*testCall[privateTaskObservationResult], 0, callers)
	for index, client := range clients {
		client.options.taskObservationEntryObserver = func() {
			entered <- struct{}{}
			<-released
		}
		ctx := scope.context()
		calls = append(calls, startTestCall(scope, func() privateTaskObservationResult {
			observation, err := client.ReconcileStatus(ctx, identities[index])
			return privateTaskObservationResult{observation: observation, err: err}
		}))
	}
	for _, call := range calls {
		awaitEntry(scope, entered, call, "compatible row caller did not enter the shared observation")
	}
	if got := privateTaskObservationStatusCount(t, controlPath); got != callers {
		t.Fatalf("row entry started a snapshot before release: got %d, want %d", got, callers)
	}
	release()
	results := make(chan privateTaskObservationResult, callers)
	for _, call := range calls {
		results <- awaitCall(scope, call, "compatible shared row observation")
	}
	close(results)
	assertPrivateTaskObservationResults(t, results)
	if got := privateTaskObservationStatusCount(t, controlPath); got != callers+1 {
		t.Fatalf("compatibility probes plus overlapping rows started %d snapshots, want %d", got, callers+1)
	}
}

func damageCompatibleProbePair(t *testing.T, f completedPrivateSuccessorTestFixture, damage string) {
	t.Helper()
	path := f.clientPath
	if damage == "missing daemon" || damage == "changed daemon" {
		path = f.daemonPath
	}
	source := privateClientFixture
	if damage == "missing daemon" || damage == "changed daemon" {
		source = privateDaemonFixture
	}
	// Cleanup restores original fixture source without reading the installed file.
	t.Cleanup(func() { writeExecutable(t, path, source) })
	if damage == "missing client" || damage == "missing daemon" {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	} else {
		writeExecutable(t, path, "#!/bin/sh\nexit 91\n")
	}
}

func assertCompatibleProbeHoldsSharedLease(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	exclusiveContext, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	lock, err := acquireBootstrapLock(exclusiveContext, filepath.Join(root, privateSupervisorDirectory, "bootstrap.lock"))
	if closeErr := closeBootstrapLock(lock); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("selector escaped shared observation: %v", err)
	}
}

func TestProbeSavedPrivateCompatibleStoppedDoesNotStart(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "compatible"}[unavailable], func(t *testing.T) {
			f := newCompletedPrivateSuccessorTestFixture(t, false)
			clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(f.base, "available-D"))
			if unavailable {
				damageCompatibleProbePair(t, f, "missing daemon")
			}
			before := snapshotSavedProbeTestState(t, f.stateRoot)
			called := 0
			client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) { called++; return clientPath, daemonPath, nil })
			closeSavedProbeTestClient(t, client, err)
			if client != nil || err != nil || called != map[bool]int{false: 0, true: 1}[unavailable] {
				t.Fatalf("stopped probe: client=%t calls=%d err=%v", client != nil, called, err)
			}
			assertSavedProbeTestUnchanged(t, f.stateRoot, before)
			assertPrivateDaemonStartCount(t, f.stateRoot, 1)
		})
	}
}

func TestProbeSavedPrivateCompatibleMissingSelectedPairIsOperational(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, false)
	removePrivateTestPath(t, filepath.Join(f.base, "completed"))
	before := snapshotSavedProbeTestState(t, f.stateRoot)
	called := 0
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) {
		called++
		return f.clientPath, f.daemonPath, nil
	})
	closeSavedProbeTestClient(t, client, err)
	if client != nil || called != 1 || !errors.Is(err, ErrBinding) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing selected transport became clean not-ready: client=%t calls=%d err=%v", client != nil, called, err)
	}
	assertSavedProbeTestUnchanged(t, f.stateRoot, before)
	assertPrivateDaemonStartCount(t, f.stateRoot, 1)
}

func TestProbeSavedPrivateCompatibleUnrelatedSavedDoesNotSelectD(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	damageCompatibleProbePair(t, f, "missing client")
	saved := f.saved
	saved.ConfigDigest = task.ComputeSHA256([]byte("unrelated saved config"))
	called := 0
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, saved, f.options, func(context.Context) (string, string, error) { called++; return "", "", nil })
	closeSavedProbeTestClient(t, client, err)
	if client != nil || err != nil || called != 0 {
		t.Fatalf("unrelated saved selected compatible transport: calls=%d err=%v", called, err)
	}
	assertPrivateDaemonStartCount(t, f.stateRoot, 2)
}

func TestProbeSavedPrivateCompatibleSelectorFaultGraphIsHard(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	damageCompatibleProbePair(t, f, "missing client")
	selectionFault, closeFault := errors.New("available pair read fault"), errors.New("resolver close fault")
	before := snapshotSavedProbeTestState(t, f.stateRoot)
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) { return "", "", errors.Join(selectionFault, closeFault) })
	closeSavedProbeTestClient(t, client, err)
	if client != nil || !errors.Is(err, selectionFault) || !errors.Is(err, closeFault) {
		t.Fatalf("selector/Close graph lost: %v", err)
	}
	assertSavedProbeTestUnchanged(t, f.stateRoot, before)
}

func TestProbeSavedPrivateTaskObservationUnrelatedCompleteExactStaysNotReady(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	saved := f.saved
	saved.ConfigDigest = task.ComputeSHA256([]byte("unrelated saved config"))
	for _, probe := range []struct {
		name string
		run  func(context.Context, string, task.SupervisorRef, Options, CompatiblePairSelector) (*Client, error)
	}{
		{name: "general", run: ProbeSavedPrivateCompatible},
		{name: "task observation", run: ProbeSavedPrivateTaskObservation},
	} {
		t.Run(probe.name, func(t *testing.T) {
			before := snapshotSavedProbeTestState(t, f.stateRoot)
			called := 0
			client, err := probe.run(t.Context(), f.stateRoot, saved, f.options, func(context.Context) (string, string, error) {
				called++
				return "", "", errors.New("ready exact pair must not select another installation")
			})
			closeSavedProbeTestClient(t, client, err)
			if client != nil || err != nil || called != 0 {
				t.Fatalf("unrelated complete generation did not remain not ready: client=%t calls=%d err=%v", client != nil, called, err)
			}
			assertSavedProbeTestUnchanged(t, f.stateRoot, before)
			assertPrivateDaemonStartCount(t, f.stateRoot, 2)
		})
	}
}
