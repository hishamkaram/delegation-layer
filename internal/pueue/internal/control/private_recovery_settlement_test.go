package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type privateRecoverySettlementFixture struct {
	stateRoot        string
	record           privateRecoveryBindingRecord
	clientExecutable string
	daemonExecutable string
	gate             *stopIdentityLeaseTestGate
	options          Options
}

func newPrivateRecoverySettlementFixture(t *testing.T) privateRecoverySettlementFixture {
	t.Helper()
	stateRoot, bindings := newPrivateSuccessorTestState(t)
	seedPrivateSuccessorTestHistory(t, stateRoot, bindings, [][2]string{{"A", "B"}}, "B")
	seedPrivateSuccessorTestBlocker(t, stateRoot, bindings, "recovery alias")
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	makePrivateTestDirectory(t, filepath.Join(base, "data"))
	makePrivateTestDirectory(t, filepath.Join(base, "run"))
	record, found, err := readPrivateRecoveryBinding(stateRoot)
	if err != nil || !found {
		t.Fatalf("saved private recovery fixture: found=%t err=%v", found, err)
	}
	clientExecutable := filepath.Join(filepath.Dir(stateRoot), "pueue-replacement")
	daemonExecutable := filepath.Join(filepath.Dir(stateRoot), "pueued-replacement")
	writeExecutable(t, clientExecutable, bindingProbePendingTestScript+privateClientFixture)
	writeExecutable(t, daemonExecutable, privateDaemonFixture)
	gateName := "private-recovery-version"
	return privateRecoverySettlementFixture{
		stateRoot: stateRoot, record: record, clientExecutable: clientExecutable, daemonExecutable: daemonExecutable,
		gate:    newStopIdentityLeaseTestGate(t, base, gateName),
		options: Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, Environment: append(os.Environ(), "BINDING_PROBE_GATE="+filepath.Join(base, gateName)), ObservationTimeout: 10 * time.Second},
	}
}

func TestPrivateRecoveryResidentBindFailureJoinsVersionProbe(t *testing.T) {
	fixture := newPrivateRecoverySettlementFixture(t)
	fixture.options.WaitForPendingClose = true
	pendingClose := make(chan struct{})
	fixture.options.pendingCloseObserver = sync.OnceFunc(func() { close(pendingClose) })
	var completed atomic.Bool
	fixture.options.Observer = func(event CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "--version") {
			completed.Store(true)
		}
	}
	watchdog, cancelWatchdog := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelWatchdog()
	probeContext, cancelProbe := context.WithCancel(watchdog)
	call := &bindingProbePendingTestCall{done: make(chan struct{})}
	completedBeforeReturn := false
	t.Cleanup(func() {
		fixture.gate.release()
		cancelProbe()
		<-call.done
		for _, pending := range pendingHandles(call.err) {
			if !awaitPendingNaturally(pending, 10*time.Second) {
				t.Error("released version probe did not finish naturally")
			}
		}
		if closeErr := call.client.CloseAfterPending(call.err); closeErr != nil {
			t.Error(closeErr)
		}
	})
	go func() {
		call.client, _, _, call.err = bindPrivateRecoveryClient(probeContext, fixture.stateRoot, fixture.record, fixture.record.Supervisor, fixture.clientExecutable, fixture.daemonExecutable, fixture.options)
		completedBeforeReturn = completed.Load()
		close(call.done)
	}()
	fixture.gate.wait(t, watchdog, call.done)
	cancelProbe()
	waitPrivateBindingPendingClose(t, watchdog, pendingClose, call)
	select {
	case <-call.done:
		t.Fatalf("resident recovery returned while version probe was held: %v", call.err)
	default:
	}
	fixture.gate.release()
	select {
	case <-call.done:
	case <-watchdog.Done():
		t.Fatalf("resident recovery did not settle after release: %v", watchdog.Err())
	}
	if !completedBeforeReturn {
		t.Fatal("resident recovery discarded its Bind client before the version probe completed")
	}
	if call.client != nil || !errors.Is(call.err, ErrInFlight) {
		t.Fatalf("binding failure changed: client=%v err=%v", call.client, call.err)
	}
	pending := pendingFrom(call.err)
	if pending == nil {
		t.Fatal("binding failure lost the original version probe")
	}
	result := completeBindingProbePendingTest(t, fixture.gate, pending)
	if string(result.Stdout) != "pueue 99.7.3\n" {
		t.Fatalf("version probe completion=%q", result.Stdout)
	}
}

func TestPrivateRecoveryDefaultBindFailureKeepsFiniteObservation(t *testing.T) {
	fixture := newPrivateRecoverySettlementFixture(t)
	client, probeErr := detachBindingProbePendingTest(t, fixture.gate, func(ctx context.Context) (*Client, error) {
		owner, _, _, err := bindPrivateRecoveryClient(ctx, fixture.stateRoot, fixture.record, fixture.record.Supervisor, fixture.clientExecutable, fixture.daemonExecutable, fixture.options)
		return owner, err
	})
	if client != nil {
		t.Fatal("failed private recovery returned a usable client")
	}
	pending := requireBindingProbePendingTestHandle(t, probeErr, "--version")
	result := completeBindingProbePendingTest(t, fixture.gate, pending)
	if string(result.Stdout) != "pueue 99.7.3\n" {
		t.Fatalf("detached version probe completion=%q", result.Stdout)
	}
}
