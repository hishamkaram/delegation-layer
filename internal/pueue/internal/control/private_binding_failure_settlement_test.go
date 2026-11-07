package control

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type privateBindFailureOperation func(context.Context, privateRecoverySettlementFixture) (*Client, error)

func privateBindFailureSettlementCases() []struct {
	name string
	bind privateBindFailureOperation
} {
	return []struct {
		name string
		bind privateBindFailureOperation
	}{
		{
			name: "transition",
			bind: func(ctx context.Context, fixture privateRecoverySettlementFixture) (*Client, error) {
				client, _, err := bindPrivateTransitionClient(ctx, fixture.clientExecutable, fixture.daemonExecutable, fixture.record.ConfigPath, fixture.options, false)
				return client, err
			},
		},
		{
			name: "expected binding",
			bind: func(ctx context.Context, fixture privateRecoverySettlementFixture) (*Client, error) {
				return nil, verifyExpectedPrivateBinding(ctx, fixture.clientExecutable, fixture.daemonExecutable, fixture.record.ConfigPath, fixture.options, &fixture.record.Supervisor)
			},
		},
		{
			name: "migration preparation",
			bind: func(ctx context.Context, fixture privateRecoverySettlementFixture) (*Client, error) {
				stateRoot, private := privateStateRoot(fixture.record.Supervisor.ConfigPath)
				if !private {
					return nil, errors.New("settlement fixture requires a private state root")
				}
				return preparePrivateWithLock(ctx, fixture.clientExecutable, fixture.daemonExecutable, stateRoot, fixture.options, true)
			},
		},
	}
}

func TestPrivateBindingResidentFailuresJoinVersionProbe(t *testing.T) {
	for _, testCase := range privateBindFailureSettlementCases() {
		t.Run(testCase.name, func(t *testing.T) {
			assertPrivateResidentBindFailureSettlement(t, newPrivateRecoverySettlementFixture(t), testCase.bind)
		})
	}
}

func assertPrivateResidentBindFailureSettlement(t *testing.T, fixture privateRecoverySettlementFixture, bind privateBindFailureOperation) {
	t.Helper()
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
		call.client, call.err = bind(probeContext, fixture)
		completedBeforeReturn = completed.Load()
		close(call.done)
	}()
	fixture.gate.wait(t, watchdog, call.done)
	cancelProbe()
	waitPrivateBindingPendingClose(t, watchdog, pendingClose, call)
	select {
	case <-call.done:
		t.Fatalf("resident binding returned while its version probe was held: %v", call.err)
	default:
	}
	fixture.gate.release()
	select {
	case <-call.done:
	case <-watchdog.Done():
		t.Fatalf("resident binding did not settle after release: %v", watchdog.Err())
	}
	if !completedBeforeReturn {
		t.Fatal("resident binding discarded its Bind client before the version probe completed")
	}
	requirePrivateBindFailure(t, call.client, call.err)
	pending := pendingFrom(call.err)
	if pending == nil || len(pendingHandles(call.err)) != 1 {
		t.Fatalf("binding failure lost or duplicated its original version probe: %v", call.err)
	}
	result := completeBindingProbePendingTest(t, fixture.gate, pending)
	if string(result.Stdout) != "pueue 99.7.3\n" {
		t.Fatalf("version probe completion=%q", result.Stdout)
	}
}

func waitPrivateBindingPendingClose(t *testing.T, ctx context.Context, pendingClose <-chan struct{}, call *bindingProbePendingTestCall) {
	t.Helper()
	select {
	case <-pendingClose:
	case <-call.done:
		t.Fatalf("resident binding returned before retaining its pending probe: %v", call.err)
	case <-ctx.Done():
		t.Fatalf("version probe did not reach resident close: %v", ctx.Err())
	}
}

func TestPrivateBindingDefaultFailuresKeepFiniteObservation(t *testing.T) {
	for _, testCase := range privateBindFailureSettlementCases() {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newPrivateRecoverySettlementFixture(t)
			client, probeErr := detachBindingProbePendingTest(t, fixture.gate, func(ctx context.Context) (*Client, error) {
				return testCase.bind(ctx, fixture)
			})
			requirePrivateBindFailure(t, client, probeErr)
			pending := requireBindingProbePendingTestHandle(t, probeErr, "--version")
			result := completeBindingProbePendingTest(t, fixture.gate, pending)
			if string(result.Stdout) != "pueue 99.7.3\n" {
				// The same retained command must finish after its finite observer returns.
				t.Fatalf("detached version probe completion=%q", result.Stdout)
			}
		})
	}
}

func requirePrivateBindFailure(t *testing.T, client *Client, err error) {
	t.Helper()
	if client != nil || !errors.Is(err, ErrInFlight) || errors.Is(err, ErrBinding) {
		t.Fatalf("pending binding failure changed: client=%v err=%v", client, err)
	}
}
