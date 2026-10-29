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
// The stop begins under the ordinary observation bound and must classify the
// endpoint first. With the exact recorded process live, the canonical wait
// then stays pending until its own deadline. After that process exits and is
// joined, cleanup closes the endpoint, and a repeated cleanup also succeeds.
func assertNativeTeardownWaitsForRecordedProcess(t *testing.T, refused bool) {
	t.Helper()
	fixture, process, finish := newRecordedTeardownProcess(t)
	if !refused {
		removePrivateTestPath(t, fixture.socketPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
	refusal, err := beginNativePrivateSupervisorStop(ctx, fixture.client)
	cancel()
	if err != nil || errors.Is(refusal, ErrPrivateEndpointRefused) != refused {
		t.Fatalf("stop begin = refusal %v err %v, want refusal %t", refusal, err, refused)
	}
	// The finite deadline is the behavior proof that the wait stays pending.
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = awaitNativePrivateSupervisorStop(ctx, fixture.client, process, refusal)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPrivateEndpointRefused) != refused {
		t.Fatalf("cleanup with the recorded process live = %v, want a pending wait that keeps refusal %t", err, refused)
	}
	assertNativeTeardownWaitIsPending(t, err, refusal)
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

// assertNativeTeardownWaitIsPending requires the wait's own error to be a
// plain deadline, not a binding fault. A refusal carries ErrBinding by design,
// so a refused endpoint must be the exact second cause beside that wait error.
func assertNativeTeardownWaitIsPending(t *testing.T, err, refusal error) {
	t.Helper()
	waitErr := err
	if refusal != nil {
		var joined interface{ Unwrap() []error }
		if !errors.As(err, &joined) || len(joined.Unwrap()) != 2 || !errors.Is(joined.Unwrap()[1], refusal) {
			t.Fatalf("cleanup with the recorded process live = %v, want the wait error joined with refusal %v", err, refusal)
		}
		waitErr = joined.Unwrap()[0]
	}
	if !errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(waitErr, ErrBinding) {
		t.Fatalf("pending wait = %v, want its deadline without a binding fault", waitErr)
	}
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
