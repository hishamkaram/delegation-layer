package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// refusedPrivateEndpointFixture is a real BindPrivate root whose fixture
// daemon exited naturally, leaving its recorded PID absent, plus a refused
// socket inode at the exact private endpoint.
type refusedPrivateEndpointFixture struct {
	privateSupervisorTestFixture
	client     *Client
	socketPath string
	socket     os.FileInfo
}

func newRefusedPrivateEndpointFixture(t *testing.T) refusedPrivateEndpointFixture {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	socketPath := strings.TrimPrefix(client.runtimeBinding.Endpoint, "unix:")
	removePrivateTestPath(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready"))
	return refusedPrivateEndpointFixture{
		privateSupervisorTestFixture: fixture,
		client:                       client,
		socketPath:                   socketPath,
		socket:                       listenRefusedPrivateTestSocket(t, socketPath),
	}
}

func listenRefusedPrivateTestSocket(t *testing.T, path string) os.FileInfo {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener type = %T, want *net.UnixListener", listener)
	}
	unixListener.SetUnlinkOnClose(false)
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			t.Error(removeErr)
		}
	})
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func (fixture refusedPrivateEndpointFixture) assertSocketPreserved(t *testing.T) {
	t.Helper()
	info, err := os.Lstat(fixture.socketPath)
	if err != nil || !os.SameFile(info, fixture.socket) {
		t.Fatalf("refused endpoint inode changed: info=%v err=%v", info, err)
	}
}

func (fixture refusedPrivateEndpointFixture) assertSocketRemoved(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(fixture.socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused stale endpoint remained: %v", err)
	}
}

func (fixture refusedPrivateEndpointFixture) bind(t *testing.T) (*Client, error) {
	t.Helper()
	client, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
	if err == nil {
		t.Cleanup(func() {
			if closeErr := client.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	return client, err
}

func assertRefusedEndpointReason(t *testing.T, err error, reason string) {
	t.Helper()
	var recovery *PrivateRecoveryError
	if !errors.As(err, &recovery) || recovery.ReasonCode != reason {
		t.Fatalf("refused endpoint reason = %v, want %s", err, reason)
	}
	if !errors.Is(err, ErrPrivateEndpointRefused) || !errors.Is(err, ErrBinding) {
		t.Fatalf("refused endpoint lost its original identity: %v", err)
	}
	if errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("ordinary refusal was classified as migration-required: %v", err)
	}
}

func TestRefusedEndpointWithoutRecordedPIDFailsClosed(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	removePrivateTestPID(t, fixture.stateRoot)
	identityPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "daemon.identity.json")
	identity, present := snapshotPrivateTestFile(t, identityPath)

	_, err := fixture.bind(t)
	assertRefusedEndpointReason(t, err, privateEndpointOwnerUnknownReason)
	ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
	defer cancel()
	err = WithBootstrapLockContext(ctx, fixture.stateRoot, func(lockedContext context.Context) error {
		prepared, prepareErr := PreparePrivateWithLock(lockedContext, fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
		if prepareErr != nil {
			return prepareErr
		}
		startErr := prepared.StartPrivate(lockedContext, fixture.stateRoot)
		return errors.Join(startErr, prepared.CloseAfterPending(startErr))
	})
	assertRefusedEndpointReason(t, err, privateEndpointOwnerUnknownReason)
	fixture.assertSocketPreserved(t)
	assertPrivateTestFileUnchanged(t, identityPath, identity, present)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestRefusedEndpointWithPresentRecordedPIDFailsClosed(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	process, _, finishProcess := startBlockingProcess(t)
	pidPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(process.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.bind(t)
	if finishErr := finishProcess(); finishErr != nil {
		t.Fatal(finishErr)
	}
	assertRefusedEndpointReason(t, err, privateEndpointRefusedLiveReason)
	fixture.assertSocketPreserved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestRefusedEndpointClosureRejectsNonSocketAndOutsidePath(t *testing.T) {
	t.Run("regular file", testRefusedEndpointRejectsRegularFile)
	t.Run("symlink", testRefusedEndpointRejectsSymlink)
	t.Run("outside runtime directory", testRefusedEndpointRejectsOutsidePath)
}

func testRefusedEndpointRejectsRegularFile(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	removePrivateTestPath(t, fixture.socketPath)
	if err := os.WriteFile(fixture.socketPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(fixture.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	unavailable, refused, probeErr := privateEndpointProbe(context.Background(), fixture.socketPath)
	_, err = fixture.bind(t)
	assertNativeNonSocketBindFailure(t, unavailable, refused, probeErr, err)
	// A stale typed refusal cannot make the canonical closure remove a file.
	_, exitErr := exitedPrivateTestCommand(t)
	err = fixture.client.closeRefusedPrivateEndpoint(context.Background(), PrivateEndpointObservation{state: PrivateEndpointRefused, cause: exitErr})
	if !errors.Is(err, ErrBinding) || !errors.Is(err, ErrPrivateEndpointRefused) {
		t.Fatalf("closure accepted a non-socket endpoint: %v", err)
	}
	if after, statErr := os.Lstat(fixture.socketPath); statErr != nil || !os.SameFile(before, after) {
		t.Fatalf("non-socket endpoint changed: %v", statErr)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

// assertNativeNonSocketBindFailure checks BindPrivate against the canonical
// probe's native fact for a regular file at the endpoint.
func assertNativeNonSocketBindFailure(t *testing.T, unavailable, refused bool, probeErr, bindErr error) {
	t.Helper()
	switch {
	case !unavailable && refused && probeErr == nil:
		// Linux connect(2) reports ECONNREFUSED for a non-socket inode, so the
		// canonical closure sees the refusal and must still keep the file.
		if !errors.Is(bindErr, ErrBinding) || !errors.Is(bindErr, ErrPrivateEndpointRefused) {
			t.Fatalf("refused non-socket endpoint lost its identity: %v", bindErr)
		}
	case !unavailable && !refused && errors.Is(probeErr, syscall.ENOTSOCK):
		// macOS connect(2) reports ENOTSOCK: a binding fault, never a refusal
		// or an absent endpoint.
		if !errors.Is(bindErr, ErrBinding) || !errors.Is(bindErr, syscall.ENOTSOCK) || errors.Is(bindErr, ErrPrivateEndpointRefused) {
			t.Fatalf("non-socket endpoint was not a binding fault: %v", bindErr)
		}
	default:
		t.Fatalf("unrecognized native probe of a regular file: unavailable=%t refused=%t err=%v", unavailable, refused, probeErr)
	}
}

func testRefusedEndpointRejectsSymlink(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	target := filepath.Join(filepath.Dir(fixture.stateRoot), "foreign.sock")
	targetInfo := listenRefusedPrivateTestSocket(t, target)
	removePrivateTestPath(t, fixture.socketPath)
	if err := os.Symlink(target, fixture.socketPath); err != nil {
		t.Fatal(err)
	}
	// The existing config fence rejects the symlink before any closure.
	if _, err := fixture.bind(t); err == nil {
		t.Fatal("symlinked endpoint was accepted")
	}
	if info, statErr := os.Lstat(fixture.socketPath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlinked endpoint changed: %v", statErr)
	}
	if info, statErr := os.Lstat(target); statErr != nil || !os.SameFile(info, targetInfo) {
		t.Fatalf("symlink target changed: %v", statErr)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func testRefusedEndpointRejectsOutsidePath(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	outside := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "outside.sock")
	outsideInfo := listenRefusedPrivateTestSocket(t, outside)
	fixture.client.runtimeBinding.Endpoint = "unix:" + outside
	err := fixture.client.closeRefusedPrivateEndpoint(context.Background(), staleRefusedObservation(t))
	if !errors.Is(err, ErrBinding) || !strings.Contains(err.Error(), "outside its runtime directory") {
		t.Fatalf("outside endpoint was not refused: %v", err)
	}
	if info, statErr := os.Lstat(outside); statErr != nil || !os.SameFile(info, outsideInfo) {
		t.Fatalf("outside endpoint changed: %v", statErr)
	}
}

// exitedPrivateTestCommand observes a real command that completed normally
// with a nonzero exit and no response.
func exitedPrivateTestCommand(t *testing.T) (CommandResult, error) {
	t.Helper()
	exited, exitErr := observeCommand(context.Background(), transport.StartOwned(exec.Command(finiteExecutable(t, "exit 1"))), time.Second)
	if exitErr == nil || !exited.Started {
		t.Fatalf("fixture command did not exit unsuccessfully: %+v %v", exited, exitErr)
	}
	return exited, exitErr
}

// staleRefusedObservation is a refusal observation recorded before the
// endpoint changed; the closure must revalidate the current endpoint.
func staleRefusedObservation(t *testing.T) PrivateEndpointObservation {
	t.Helper()
	_, exitErr := exitedPrivateTestCommand(t)
	return PrivateEndpointObservation{state: PrivateEndpointRefused, cause: exitErr}
}

// canonicalRefusedObservation observes a refusal through the only producer: a
// normally completed nonzero command plus the exact refused probe.
func canonicalRefusedObservation(t *testing.T, fixture refusedPrivateEndpointFixture) (PrivateEndpointObservation, error) {
	t.Helper()
	exited, exitErr := exitedPrivateTestCommand(t)
	observation, handled, err := privateReadyEndpointFailure(context.Background(), fixture.client, exited, exitErr)
	if !handled || err != nil || observation.State() != PrivateEndpointRefused || !errors.Is(observation.Cause(), exitErr) {
		t.Fatalf("fixture did not observe a refusal: handled=%t observation=%+v err=%v", handled, observation, err)
	}
	return observation, exitErr
}

func TestPrivateEndpointRefusalProjectsTypedObserverError(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	observation, exitErr := canonicalRefusedObservation(t, fixture)
	refusal := observation.projectedError()
	if !errors.Is(refusal, ErrBinding) || !errors.Is(refusal, ErrPrivateEndpointRefused) || !errors.Is(refusal, exitErr) {
		t.Fatalf("refusal projection lost an identity: %v", refusal)
	}
	for _, other := range []PrivateEndpointObservation{{}, {state: PrivateEndpointReady}, {state: PrivateEndpointAbsent, cause: exitErr}} {
		if err := other.projectedError(); err != nil {
			t.Fatalf("observation %+v projected an error: %v", other, err)
		}
		// Only a refusal reaches the closure; every other observation is a no-op.
		if err := fixture.client.closeRefusedPrivateEndpoint(context.Background(), other); err != nil {
			t.Fatalf("closure acted on %+v: %v", other, err)
		}
	}
	fixture.assertSocketPreserved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

// TestRefusedObservationWithCloseFaultKeepsBothCauses drives the canonical
// transition finalizer with a real refusal, a real bootstrap-lock release
// fault, and a client cleanup fault. Every cause survives, the refusal is
// joined exactly once whether or not the failure already carries it, the
// client closes exactly once, and nothing starts or touches the socket.
func TestRefusedObservationWithCloseFaultKeepsBothCauses(t *testing.T) {
	for _, carried := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure carries refusal %t", carried), func(t *testing.T) {
			fixture := newRefusedPrivateEndpointFixture(t)
			observation, exitErr := canonicalRefusedObservation(t, fixture)
			closeErr := errors.New("transition client close failed")
			cleanups := 0
			fixture.client.cleanup = func() error {
				cleanups++
				return closeErr
			}
			releaseErr := closedBootstrapLockReleaseFault(t)
			checkErr := errors.Join(fmt.Errorf("%w: binding check failed", ErrBinding), releaseErr)
			if carried {
				checkErr = errors.Join(checkErr, observation.projectedError())
			}
			err := failPrivateTransitionClient(fixture.client, observation, checkErr)
			for _, want := range []error{releaseErr, closeErr, ErrBinding, ErrPrivateEndpointRefused, exitErr} {
				if !errors.Is(err, want) {
					t.Fatalf("transition failure lost %v: %v", want, err)
				}
			}
			if refusals := countErrorLeaves(err, ErrPrivateEndpointRefused); refusals != 1 {
				t.Fatalf("transition failure carries the refusal %d times: %v", refusals, err)
			}
			if closeAgainErr := fixture.client.Close(); closeAgainErr != nil || cleanups != 1 {
				t.Fatalf("transition client cleanup ran %d times (second close: %v)", cleanups, closeAgainErr)
			}
			fixture.assertSocketPreserved(t)
			assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
		})
	}
}

// closedBootstrapLockReleaseFault is the real error from releasing a
// bootstrap lock whose descriptor is already closed.
func closedBootstrapLockReleaseFault(t *testing.T) error {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "closed-bootstrap-lock-")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	releaseErr := closeBootstrapLock(&bootstrapLock{file: file})
	if releaseErr == nil {
		t.Fatal("releasing a closed bootstrap lock reported no fault")
	}
	return releaseErr
}

func countErrorLeaves(err, target error) int {
	count := 0
	task.AllErrorCauses(err, func(cause error) bool {
		if errors.Is(cause, target) {
			count++
		}
		return true
	})
	return count
}

// TestEndpointObservationRejectsTruncatedStatusCapture feeds real captured
// status output to the endpoint observer. Output that ends inside the status
// document is never Absent or Refused authority, and nothing is mutated.
func TestEndpointObservationRejectsTruncatedStatusCapture(t *testing.T) {
	for _, state := range []string{"absent", "refused"} {
		t.Run(state, func(t *testing.T) {
			fixture := newRefusedPrivateEndpointFixture(t)
			if state == "absent" {
				removePrivateTestPath(t, fixture.socketPath)
			}
			executable := finiteExecutable(t, `printf '{"tasks"'; exit 1`)
			result, commandErr := observeCommand(context.Background(), startOwnedCaptured(exec.Command(executable), "", nil, FixtureVersion), time.Second)
			if commandErr == nil || !result.Started || result.ExitCode != 1 || !errors.Is(result.Err, io.ErrUnexpectedEOF) {
				t.Fatalf("truncated status capture = %+v, %v", result, commandErr)
			}
			observation, handled, err := privateReadyEndpointFailure(context.Background(), fixture.client, result, commandErr)
			if !handled || observation.State() != 0 || !errors.Is(err, ErrBinding) || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("truncated status produced an endpoint fact: handled=%t observation=%+v err=%v", handled, observation, err)
			}
			if state == "refused" {
				fixture.assertSocketPreserved(t)
			}
			assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
		})
	}
}

func TestRefusedEndpointRequiresNormalCompletedCommand(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	exited, exitErr := exitedPrivateTestCommand(t)
	withErr := func(err error) CommandResult {
		result := exited
		result.Err = err
		return result
	}
	withStdout := exited
	withStdout.Stdout = []byte("{")
	cases := []struct {
		name    string
		result  CommandResult
		err     error
		refused bool
	}{
		{name: "normal completion", result: exited, err: exitErr, refused: true},
		{name: "control owner completion", result: withErr(&controlExitError{code: 1, message: "supervisor control process exited unsuccessfully"}), err: exitErr, refused: true},
		{name: "control owner protocol fault", result: withErr(errors.Join(&controlExitError{code: 1}, errors.New("supervisor control owner exited unsuccessfully"))), err: exitErr},
		{name: "empty status stream", result: withErr(errors.Join(exitErr, errors.Join(ErrUnknown, io.EOF))), err: exitErr, refused: true},
		{name: "joined reader fault", result: withErr(errors.Join(exitErr, errors.New("status reader failed"))), err: exitErr},
		{name: "control limit", result: withErr(errors.Join(exitErr, ErrControlLimit)), err: exitErr},
		{name: "malformed status", result: withErr(errors.Join(exitErr, errors.Join(ErrUnknown, io.ErrUnexpectedEOF))), err: exitErr},
		{name: "wrapped unknown with EOF", result: withErr(errors.Join(exitErr, ErrUnknown, fmt.Errorf("%w: invalid scalar", ErrUnknown), io.EOF)), err: exitErr},
		{name: "signaled command", result: withErr(&controlExitError{code: -1}), err: exitErr},
		{name: "captured response", result: withStdout, err: exitErr},
		{name: "wait fault", result: withErr(errors.New("wait infrastructure failure")), err: exitErr},
		{name: "pending command", result: exited, err: errors.Join(exitErr, &InFlightError{Pending: transport.NewPending(nil)})},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			observation, handled, err := privateReadyEndpointFailure(context.Background(), fixture.client, testCase.result, testCase.err)
			if !handled {
				t.Fatal("refused endpoint result was not handled")
			}
			if got := observation.State() == PrivateEndpointRefused; got != testCase.refused {
				t.Fatalf("typed refusal = %t, want %t: %+v %v", got, testCase.refused, observation, err)
			}
			if (err == nil) != testCase.refused || (err != nil && !errors.Is(err, ErrBinding)) {
				t.Fatalf("refusal and operational error are not exclusive: %+v %v", observation, err)
			}
		})
	}
	fixture.assertSocketPreserved(t)
}

func TestRefusedEndpointObservationsRemainErrors(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	ready, err := fixture.client.Reachable(context.Background())
	if ready || !errors.Is(err, ErrBinding) || !errors.Is(err, ErrPrivateEndpointRefused) {
		t.Fatalf("Reachable softened a refusal: ready=%t err=%v", ready, err)
	}
	probed, ready, err := ProbePrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
	if probed != nil || ready || !errors.Is(err, ErrBinding) {
		t.Fatalf("maintenance probe softened a refusal: client=%v ready=%t err=%v", probed, ready, err)
	}
	fixture.assertSocketPreserved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestMissingMigrationBindingIsNotInventedFromRefusal(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	bindingPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "supervisor.identity.json")
	removePrivateTestPath(t, bindingPath)
	ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
	defer cancel()
	err := WithBootstrapLockContext(ctx, fixture.stateRoot, func(lockedContext context.Context) error {
		current, prepareErr := PreparePrivateForMigrationWithLock(lockedContext, fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
		if prepareErr != nil {
			return prepareErr
		}
		migrateErr := current.MigratePrivateSupervisorWithLock(lockedContext, fixture.stateRoot)
		return errors.Join(migrateErr, current.CloseAfterPending(migrateErr))
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || !errors.Is(err, ErrPrivateEndpointRefused) {
		t.Fatalf("missing predecessor binding was not refused with its cause: %v", err)
	}
	assertPrivateTestPathMissing(t, bindingPath)
	fixture.assertSocketPreserved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}
