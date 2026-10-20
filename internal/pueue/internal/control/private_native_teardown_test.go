package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// newRecordedTeardownProcess records a live blocking process as the daemon of
// a bound fixture root whose endpoint is refused. It returns the process
// identity captured from that record, then removes the record as a graceful
// daemon would, and returns the owned EOF/join finisher.
func newRecordedTeardownProcess(t *testing.T) (refusedPrivateEndpointFixture, privateProcessIdentity, func() error) {
	t.Helper()
	fixture := newRefusedPrivateEndpointFixture(t)
	process, _, finish := startBlockingProcess(t)
	pidPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(process.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, present, err := fixture.client.privateDaemonProcessIdentity()
	if err != nil || !present || identity.PID != process.Process.Pid {
		t.Fatalf("blocking process was not recorded: identity=%+v present=%t err=%v", identity, present, err)
	}
	removePrivateTestPID(t, fixture.stateRoot)
	return fixture, identity, finish
}

// TestStopNativePrivateSupervisorWaitsForRecordedProcess proves the native
// fixture cleanup never claims closure from the endpoint alone, for an absent
// and for a refused endpoint.
func TestStopNativePrivateSupervisorWaitsForRecordedProcess(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprintf("refused %t", refused), func(t *testing.T) {
			assertNativeTeardownWaitsForRecordedProcess(t, refused)
		})
	}
}

// assertNativeTeardownWaitsForRecordedProcess runs one endpoint scenario.
// With the exact recorded process live, the canonical wait stays pending
// until its deadline. After that process exits and is joined, cleanup closes
// the endpoint, and a repeated cleanup also succeeds.
func assertNativeTeardownWaitsForRecordedProcess(t *testing.T, refused bool) {
	t.Helper()
	fixture, process, finish := newRecordedTeardownProcess(t)
	if !refused {
		removePrivateTestPath(t, fixture.socketPath)
	}
	// The finite deadline is the behavior proof that the wait stays pending.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := stopNativePrivateSupervisor(ctx, fixture.client, process)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPrivateEndpointRefused) != refused {
		t.Fatalf("cleanup with the recorded process live = %v, want a pending wait that keeps refusal %t", err, refused)
	}
	if refused {
		fixture.assertSocketPreserved(t)
	}

	if err = finish(); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		ctx, cancel = context.WithTimeout(context.Background(), privateTestObservationTimeout)
		err = stopNativePrivateSupervisor(ctx, fixture.client, process)
		cancel()
		if err != nil {
			t.Fatalf("cleanup attempt %d after the recorded process exited: %v", attempt, err)
		}
	}
	assertPrivateTestPathMissing(t, fixture.socketPath)
}

// TestStopNativePrivateSupervisorReportsUnknownProcess keeps an incomplete or
// unobservable process identity as a cleanup error, even when the endpoint is
// absent.
func TestStopNativePrivateSupervisorReportsUnknownProcess(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	removePrivateTestPath(t, fixture.socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
	defer cancel()
	if err := stopNativePrivateSupervisor(ctx, fixture.client, privateProcessIdentity{}); !errors.Is(err, ErrBinding) {
		t.Fatalf("cleanup with an incomplete process identity = %v, want a binding error", err)
	}

	errUnobservable := errors.New("process table unavailable")
	fixture.client.options.processObserver = func(int) (privateProcessObservation, error) {
		return privateProcessObservation{}, errUnobservable
	}
	process := privateProcessIdentity{PID: os.Getpid(), StartToken: "recorded"}
	if err := stopNativePrivateSupervisor(ctx, fixture.client, process); !errors.Is(err, ErrBinding) || !errors.Is(err, errUnobservable) {
		t.Fatalf("cleanup with an unobservable process = %v, want the observation error", err)
	}
}
