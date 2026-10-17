package control

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestBindingVersionProbePendingIsNotBindingDrift(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "normal", "pueue "+FixtureVersion)
	writeExecutable(t, fake.executable, bindingProbePendingTestScript+fakeSupervisorScript)
	base := filepath.Dir(fake.configPath)
	gate := newStopIdentityLeaseTestGate(t, base, "binding-version")
	options := Options{
		Environment:        append(fake.environment, "BINDING_PROBE_GATE="+filepath.Join(base, "binding-version")),
		ObservationTimeout: 10 * time.Second,
	}
	assertBindingProbePendingTest(t, gate, "pueue "+FixtureVersion+"\n", func(ctx context.Context) (*Client, error) {
		return Bind(ctx, fake.executable, fake.configPath, options)
	})
}

func TestSavedPrivateVersionProbePendingIsNotBindingDrift(t *testing.T) {
	stateRoot, bindings := newPrivateSuccessorTestState(t)
	seedPrivateSuccessorTestHistory(t, stateRoot, bindings, [][2]string{{"A", "B"}}, "B")
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	makePrivateTestDirectory(t, filepath.Join(base, "data"))
	makePrivateTestDirectory(t, filepath.Join(base, "run"))

	// The unchanged published pair bypasses version discovery. A replacement
	// pair must take the Bind path while retaining the exact saved endpoint.
	// The canonical publication fixture has absent old package executables;
	// no daemon bootstrap or publication is needed to reach the version probe.
	replacement := filepath.Join(filepath.Dir(stateRoot), "pueue-replacement")
	daemon := filepath.Join(filepath.Dir(stateRoot), "pueued-replacement")
	writeExecutable(t, replacement, bindingProbePendingTestScript+privateClientFixture)
	writeExecutable(t, daemon, privateDaemonFixture)
	gate := newStopIdentityLeaseTestGate(t, base, "binding-version")
	options := Options{
		Environment:        append(os.Environ(), "BINDING_PROBE_GATE="+filepath.Join(base, "binding-version")),
		ObservationTimeout: 10 * time.Second,
	}
	assertBindingProbePendingTest(t, gate, "pueue 99.7.3\n", func(ctx context.Context) (*Client, error) {
		return RecoverPrivate(ctx, stateRoot, bindings["B"], replacement, daemon, options)
	})
}

func TestBindSavedReturnsOwnerForPendingVersionProbe(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "normal", "pueue "+FixtureVersion)
	writeExecutable(t, fake.executable, bindingProbePendingTestScript+fakeSupervisorScript)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	client, err := Bind(ctx, fake.executable, fake.configPath, Options{Environment: fake.environment, ObservationTimeout: 10 * time.Second})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	saved := client.Binding()
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}

	base := filepath.Dir(fake.configPath)
	gate := newStopIdentityLeaseTestGate(t, base, "saved-binding-version")
	options := Options{
		Environment:        append(fake.environment, "BINDING_PROBE_GATE="+filepath.Join(base, "saved-binding-version")),
		ObservationTimeout: 10 * time.Second,
	}
	owner, probeErr := detachBindingProbePendingTest(t, gate, func(ctx context.Context) (*Client, error) {
		return BindSaved(ctx, saved, options)
	})
	if owner == nil {
		t.Fatal("BindSaved discarded the owner of its pending version probe")
	}
	pending := requireBindingProbePendingTestHandle(t, probeErr, "--version")
	completeBindingProbePendingTest(t, gate, pending)
}

func TestBindSavedForMigrationWithClientReturnsOwnerForPendingVersionProbe(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "normal", "pueue "+FixtureVersion)
	writeExecutable(t, fake.executable, bindingProbePendingTestScript+fakeSupervisorScript)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	client, err := Bind(ctx, fake.executable, fake.configPath, Options{Environment: fake.environment, ObservationTimeout: 10 * time.Second})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	saved := client.Binding()
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}

	base := filepath.Dir(fake.configPath)
	gate := newStopIdentityLeaseTestGate(t, base, "saved-migration-binding-version")
	options := Options{
		Environment:        append(fake.environment, "BINDING_PROBE_GATE="+filepath.Join(base, "saved-migration-binding-version")),
		ObservationTimeout: 10 * time.Second,
	}
	owner, probeErr := detachBindingProbePendingTest(t, gate, func(ctx context.Context) (*Client, error) {
		return BindSavedForMigrationWithClient(ctx, saved, saved.ClientExecutable, options)
	})
	if owner == nil {
		t.Fatal("BindSavedForMigrationWithClient discarded the owner of its pending version probe")
	}
	pending := requireBindingProbePendingTestHandle(t, probeErr, "--version")
	completeBindingProbePendingTest(t, gate, pending)
}

func TestBindingVersionProbeCompletedFailureRemainsBinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		exit    bool
	}{
		{name: "nonzero exit", version: "pueue " + FixtureVersion, exit: true},
		{name: "blank version", version: " \t "},
		{name: "control character", version: "pueue\t" + FixtureVersion},
		{name: "invalid UTF-8", version: "pueue \xff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeSupervisorPaths(t, "normal", tc.version)
			writeExecutable(t, fake.executable, bindingProbePendingTestScript+fakeSupervisorScript)
			environment := fake.environment
			if tc.exit {
				environment = append(environment, "BINDING_PROBE_EXIT=7")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			client, bindErr := Bind(ctx, fake.executable, fake.configPath, Options{Environment: environment, ObservationTimeout: 10 * time.Second})
			supervisorLeaseTestSettleClient(t, client, bindErr)
			if !errors.Is(bindErr, ErrBinding) || errors.Is(bindErr, ErrInFlight) || pendingFrom(bindErr) != nil {
				t.Fatalf("completed incompatible version lost binding failure: %v", bindErr)
			}
			if tc.exit {
				var exitErr *exec.ExitError
				if !errors.As(bindErr, &exitErr) || exitErr.ExitCode() != 7 {
					t.Fatalf("completed version command lost its exit failure: %v", bindErr)
				}
			}
		})
	}
}

func TestBindingReadyProbePendingIsNotBindingDrift(t *testing.T) {
	fake := newBindingReadyProbeTestClient(t)
	base := filepath.Dir(fake.configPath)
	gate := newStopIdentityLeaseTestGate(t, base, "binding-status")
	fake.client.options.Environment = append(fake.environment, "BINDING_PROBE_STATUS_GATE="+filepath.Join(base, "binding-status"))
	_, probeErr := detachBindingProbePendingTest(t, gate, func(ctx context.Context) (*Client, error) {
		return fake.client, fake.client.Ready(ctx)
	})
	pending := requireBindingProbePendingTestHandle(t, probeErr, "status", "--json")
	if errors.Is(probeErr, ErrBinding) {
		t.Errorf("unfinished status was classified as binding drift: %v", probeErr)
	}
	result := completeBindingProbePendingTest(t, gate, pending)
	if readyErr := fake.client.validateReadyResult(result); readyErr != nil {
		t.Fatalf("retained status did not complete with a healthy queue: %v", readyErr)
	}
	if pendingFrom(probeErr) != pending {
		t.Fatal("Ready error no longer exposes the original pending command")
	}
}

func TestBindingReadyProbeCompletedFailureRemainsBinding(t *testing.T) {
	for _, nonzero := range []bool{true, false} {
		name := "malformed status"
		if nonzero {
			name = "nonzero exit"
		}
		t.Run(name, func(t *testing.T) {
			fake := newBindingReadyProbeTestClient(t)
			if nonzero {
				fake.client.options.Environment = append(fake.environment, "BINDING_PROBE_STATUS_EXIT=7")
			} else if writeErr := os.WriteFile(fake.statusPath, []byte(`{"tasks":`), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			readyErr := fake.client.Ready(ctx)
			supervisorLeaseTestSettleClient(t, fake.client, readyErr)
			if !errors.Is(readyErr, ErrBinding) || errors.Is(readyErr, ErrInFlight) || pendingFrom(readyErr) != nil {
				t.Fatalf("completed status failure lost binding classification: %v", readyErr)
			}
		})
	}
}

func TestBindingProbeErrorPreservesGenuineFailureWithPending(t *testing.T) {
	pending := newPending(nil)
	inFlight := &InFlightError{Pending: pending}
	bindingFailure := errors.Join(ErrBinding, errors.New("fixture configuration digest changed"))
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "binding failure first", err: errors.Join(bindingFailure, inFlight)},
		{name: "pending command first", err: errors.Join(inFlight, bindingFailure)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classified := bindingProbeError(tc.err)
			if !errors.Is(classified, ErrBinding) || !errors.Is(classified, bindingFailure) {
				t.Fatalf("pending command erased an independently established binding failure: %v", classified)
			}
			if !errors.Is(classified, ErrInFlight) || !errors.Is(classified, inFlight) || pendingFrom(classified) != pending {
				t.Fatalf("genuine binding failure lost the original pending command: %v", classified)
			}
			handles := pendingHandles(classified)
			if len(handles) != 1 || handles[0] != pending {
				t.Fatalf("mixed probe failure changed pending ownership: %v", classified)
			}
		})
	}
}

func newBindingReadyProbeTestClient(t *testing.T) *fakeSupervisor {
	t.Helper()
	fake := newFakeSupervisorPaths(t, "normal", "pueue "+FixtureVersion)
	writeExecutable(t, fake.executable, bindingProbePendingTestScript+fakeSupervisorScript)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, bindErr := Bind(ctx, fake.executable, fake.configPath, Options{Environment: fake.environment, ObservationTimeout: 10 * time.Second})
	supervisorLeaseTestSettleClient(t, client, bindErr)
	if bindErr != nil {
		t.Fatal(bindErr)
	}
	fake.client = client
	return fake
}

type bindingProbePendingTestCall struct {
	done   chan struct{}
	client *Client
	err    error
}

func assertBindingProbePendingTest(t *testing.T, gate *stopIdentityLeaseTestGate, wantOutput string, probe func(context.Context) (*Client, error)) {
	t.Helper()
	_, probeErr := detachBindingProbePendingTest(t, gate, probe)
	pending := requireBindingProbePendingTestHandle(t, probeErr, "--version")
	if errors.Is(probeErr, ErrBinding) {
		t.Errorf("unfinished --version was classified as binding drift: %v", probeErr)
	}
	result := completeBindingProbePendingTest(t, gate, pending)
	if string(result.Stdout) != wantOutput {
		t.Fatalf("retained version command changed its result: got=%q want=%q", result.Stdout, wantOutput)
	}
	if pendingFrom(probeErr) != pending {
		t.Fatal("probe error no longer exposes the original pending command")
	}
}

func completeBindingProbePendingTest(t *testing.T, gate *stopIdentityLeaseTestGate, pending *Pending) CommandResult {
	t.Helper()
	gate.release()
	if !awaitPendingNaturally(pending, 10*time.Second) {
		t.Fatal("released command did not finish")
	}
	result, done := pending.Result()
	if !done || !result.Started || result.PID <= 0 || result.ExitCode != 0 || result.Err != nil {
		t.Fatalf("retained command lost natural completion: done=%t result=%+v", done, result)
	}
	return result
}

func detachBindingProbePendingTest(t *testing.T, gate *stopIdentityLeaseTestGate, probe func(context.Context) (*Client, error)) (*Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	probeContext, cancelProbe := context.WithCancel(ctx)
	call := &bindingProbePendingTestCall{done: make(chan struct{})}
	t.Cleanup(func() {
		// Release the finite child even after a failed classification assertion,
		// then let its sole Wait owner publish the retained result.
		gate.release()
		cancelProbe()
		<-call.done
		for _, pending := range pendingHandles(call.err) {
			if !awaitPendingNaturally(pending, 10*time.Second) {
				t.Error("released command did not complete naturally")
			}
		}
		if closeErr := call.client.CloseAfterPending(call.err); closeErr != nil {
			t.Error(closeErr)
		}
	})
	go func() {
		call.client, call.err = probe(probeContext)
		close(call.done)
	}()
	select {
	case <-gate.ready:
		if gate.err != nil {
			t.Fatalf("command fixture checkpoint: %v", gate.err)
		}
	case <-call.done:
		t.Fatalf("probe returned before its command reached the gate: %v", call.err)
	case <-ctx.Done():
		t.Fatalf("command did not reach its gate: %v", ctx.Err())
	}
	cancelProbe()
	select {
	case <-call.done:
	case <-ctx.Done():
		t.Fatalf("probe did not detach from its gated command: %v", ctx.Err())
	}
	return call.client, call.err
}

func requireBindingProbePendingTestHandle(t *testing.T, probeErr error, command ...string) *Pending {
	t.Helper()
	var inFlight *InFlightError
	if !errors.Is(probeErr, ErrInFlight) || !errors.As(probeErr, &inFlight) || inFlight == nil || inFlight.Pending == nil {
		t.Fatalf("unfinished command lost its in-flight handle: %v", probeErr)
	}
	pending := inFlight.Pending
	handles := pendingHandles(probeErr)
	if len(handles) != 1 || handles[0] != pending || pendingFrom(probeErr) != pending {
		t.Fatalf("probe wrappers replaced or duplicated the pending command: %v", probeErr)
	}
	args := pending.Args()
	if len(args) < len(command) || !slices.Equal(args[len(args)-len(command):], command) {
		t.Fatalf("pending command=%v want suffix=%v", args, command)
	}
	// An unfinished observation has no authoritative CommandResult yet. In
	// particular, its zero Started field cannot establish a failed Start.
	if _, done := pending.Result(); done {
		t.Fatal("command completed before its gate was released")
	}
	return pending
}

const bindingProbePendingTestScript = `#!/bin/sh
set -eu
probe_gate=
probe_exit=0
case "${3-}" in
  --version) probe_gate=${BINDING_PROBE_GATE:-}; probe_exit=${BINDING_PROBE_EXIT:-0} ;;
  status) probe_gate=${BINDING_PROBE_STATUS_GATE:-}; probe_exit=${BINDING_PROBE_STATUS_EXIT:-0} ;;
esac
if [ "$probe_gate" != "" ]; then
  printf x > "$probe_gate-seen"
  IFS= read -r release < "$probe_gate-release"
fi
if [ "$probe_exit" != 0 ]; then exit "$probe_exit"; fi
`
