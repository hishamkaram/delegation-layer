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
	before := snapshotSavedProbeTestState(t, f.stateRoot)
	called := 0
	selector := func(context.Context) (string, string, error) {
		called++
		return "", "", errors.New("unused install unavailable")
	}
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, selector)
	closeSavedProbeTestClient(t, client, err)
	if err != nil || client == nil || called != 0 || client.Binding() != f.saved || client.RuntimeBinding() != f.successor {
		t.Fatalf("healthy exact selection: client=%t calls=%d err=%v", client != nil, called, err)
	}
	assertSavedProbeTestUnchanged(t, f.stateRoot, before)
	assertPrivateDaemonStartCount(t, f.stateRoot, 2)
}

func TestProbeSavedPrivateCompatibleUnavailablePairReadsThroughD(t *testing.T) {
	for _, damage := range []string{"missing client", "missing daemon", "changed client", "changed daemon"} {
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

func damageCompatibleProbePair(t *testing.T, f completedPrivateSuccessorTestFixture, damage string) {
	t.Helper()
	path := f.clientPath
	if damage == "missing daemon" || damage == "changed daemon" {
		path = f.daemonPath
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Restore the original pair before the fixture's canonical shutdown cleanup.
	t.Cleanup(func() {
		if restoreErr := os.WriteFile(path, bytes, 0o700); restoreErr != nil {
			t.Error(restoreErr)
		}
	})
	if damage == "missing client" || damage == "missing daemon" {
		if err = os.Remove(path); err != nil {
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

func TestProbeSavedPrivateCompatibleLeavesExactAPIUnchanged(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	damageCompatibleProbePair(t, f, "missing client")
	dClient, dDaemon := installPrivateSupervisorPair(t, filepath.Join(f.base, "available-D"))
	client, err := ProbeSavedPrivate(t.Context(), f.stateRoot, f.saved, dClient, dDaemon, f.options)
	closeSavedProbeTestClient(t, client, err)
	if client != nil || err != nil {
		t.Fatalf("old exact probe was broadened: client=%t err=%v", client != nil, err)
	}
}
