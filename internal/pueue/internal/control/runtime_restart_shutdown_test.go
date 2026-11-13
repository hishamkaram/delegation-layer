package control

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrivateSupervisorNativeRuntimeRestartShutdownFailureFreshScopeRetry(t *testing.T) {
	for _, failedStart := range []bool{false, true} {
		name := "missing_selection_hard"
		if failedStart {
			name = "native_Start_failure_pending"
		}
		t.Run(name, func(t *testing.T) { testRuntimeRestartShutdownFreshScope(t, failedStart) })
	}
}

func testRuntimeRestartShutdownFreshScope(t *testing.T, failedStart bool) {
	t.Helper()
	fixture, client, owner, from := runtimeRestartNativeFixture(t)
	client.options.ControlOwnerExecutable = os.Args[0]
	missing, priorEntries := runtimeShutdownFaultOwner(t, fixture.stateRoot, failedStart)
	authorize := client.options.PrivateRuntimeRestartAuthorizer
	firstAdmission := true
	client.options.PrivateRuntimeRestartAuthorizer = func(ctx context.Context, root string, reference RuntimeRestartReference, binding task.SupervisorRef, jobs []Job) error {
		if err := authorize(ctx, root, reference, binding, jobs); err != nil {
			return err
		}
		// Synchronous post-snapshot fault. The installed Pueue pair and saved
		// binding stay intact; only this owned client's owner selection fails.
		if firstAdmission {
			firstAdmission = false
			client.options.ControlOwnerExecutable = missing
		}
		return nil
	}
	var shutdownEntries atomic.Int32
	client.options.Observer = func(event CommandEvent) {
		if event.Stage == "entry" && len(event.Argv) > 0 && event.Argv[len(event.Argv)-1] == "shutdown" {
			shutdownEntries.Add(1)
		}
	}
	var initial, repeated PrivateRuntimeRestartObservation
	var initialErr, repeatedErr error
	err := WithExistingBootstrapLockContext(runtimeRestartNativeContext(t), fixture.stateRoot, func(ctx context.Context) error {
		initial, initialErr = client.QuiesceForRuntimeRestart(ctx, fixture.stateRoot, from)
		client.options.ControlOwnerExecutable = os.Args[0] // Restore before any assertion or fixture cleanup.
		repeated, repeatedErr = client.QuiesceForRuntimeRestart(ctx, fixture.stateRoot, from)
		return nil // Both expected failure observations are asserted below.
	})
	if err != nil || !errors.Is(repeatedErr, ErrPrivateRuntimeRestartPending) || shutdownEntries.Load() != priorEntries {
		t.Fatalf("same-scope failed shutdown was replayed/lost: initial=%+v repeated=%+v entries=%d err=%v", initial, repeated, shutdownEntries.Load(), errors.Join(err, initialErr, repeatedErr))
	}
	assertRuntimeShutdownPendingObservation(t, initial, initialErr)
	assertRuntimeShutdownPendingObservation(t, repeated, repeatedErr)
	assertRuntimeShutdownLivePredecessor(t, client, fixture.stateRoot)

	var recovered PrivateRuntimeRestartObservation
	err = WithExistingBootstrapLockContext(runtimeRestartNativeContext(t), fixture.stateRoot, func(ctx context.Context) error {
		var recoveryErr error
		recovered, recoveryErr = client.QuiesceForRuntimeRestart(ctx, fixture.stateRoot, from)
		return recoveryErr
	})
	if err != nil || recovered.Phase != RuntimeRestartDaemonClosed || shutdownEntries.Load() != priorEntries+1 || len(owner.starts) != 1 {
		t.Fatalf("fresh scope could not reconcile failed shutdown: phase=%s entries=%d starts=%d err=%v", recovered.Phase, shutdownEntries.Load(), len(owner.starts), err)
	}
	if !joinDaemonWait(t, owner.starts[0].done) {
		return
	}
	assertRuntimeShutdownFailurePartition(t, initialErr, failedStart)
}

func runtimeShutdownFaultOwner(t *testing.T, root string, failedStart bool) (string, int32) {
	t.Helper()

	missing := filepath.Join(root, "faulted-control-owner")
	var priorEntries int32
	if failedStart {
		// An owned fault fixture, not a copied/modified installed executable.
		// Metadata qualification succeeds; native exec must reject its format.
		if err := os.WriteFile(missing, []byte("invalid native executable format\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		priorEntries = 1
	}
	return missing, priorEntries
}

func assertRuntimeShutdownPendingObservation(t *testing.T, view PrivateRuntimeRestartObservation, cause error) {
	t.Helper()
	if view.Phase != RuntimeRestartShutdownPending || cause == nil {
		t.Fatalf("failed shutdown lost pending phase/cause: %+v %v", view, cause)
	}
}

func assertRuntimeShutdownLivePredecessor(t *testing.T, client *Client, root string) {
	t.Helper()

	record, found, readErr := readPrivateBootstrapRecord(root)
	if readErr != nil || !found || record.RuntimeRestart == nil {
		t.Fatalf("failed shutdown lost its durable fence: %v", readErr)
	}
	old := privateProcessIdentity{PID: record.RuntimeRestart.OldPID, StartToken: record.RuntimeRestart.OldProcessStart}
	if absent, observeErr := client.privateProcessUnavailable(old); observeErr != nil || absent {
		t.Fatalf("failure did not leave the exact old daemon alive: absent=%t err=%v", absent, observeErr)
	}
}

func assertRuntimeShutdownFailurePartition(t *testing.T, initialErr error, failedStart bool) {
	t.Helper()
	if failedStart {
		var failure *PrivateRuntimeRestartError
		if !errors.Is(initialErr, ErrPrivateRuntimeRestartPending) || errors.Is(initialErr, ErrConfiguration) || !errors.As(initialErr, &failure) || !errors.Is(failure.DiagnosticCause(), syscall.ENOEXEC) {
			t.Fatalf("proven owner Start failure lost pending/diagnostic: %v", initialErr)
		}
	} else if !errors.Is(initialErr, ErrConfiguration) || !errors.Is(initialErr, os.ErrNotExist) || errors.Is(initialErr, ErrPrivateRuntimeRestartPending) {
		t.Fatalf("missing owner selection hid a real configuration fault: %v", initialErr)
	}
}

func TestPrivateSupervisorNativeRuntimeManagedFixtureCleanup(t *testing.T) {
	var root string
	if !t.Run("managed", func(t *testing.T) {
		fixture, _, owner, _ := runtimeRestartNativeFixture(t)
		root = fixture.stateRoot
		if len(owner.starts) != 1 {
			t.Fatal("managed fixture did not retain its canonical daemon Wait")
		}
	}) {
		return
	}
	if root == "" {
		t.Skip("native fixture prerequisites unavailable")
	}
	if _, err := os.Lstat(filepath.Dir(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed fixture cleanup did not remove its closed owned directory: %v", err)
	}
}

func TestRuntimeRestartManagedCleanupRequiresCanonicalConstruction(t *testing.T) {
	fixture, _ := runtimeRestartJournalFixture(t)
	owned := &nativePrivateBindingFixture{client: fixture.client, cleanupOptions: fixture.client.options}
	var cleanup *Client
	err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var prepareErr error
		cleanup, prepareErr = owned.prepareManagedCleanup(ctx, fixture.root)
		if prepareErr != nil {
			return prepareErr
		}
		_, scopeErr := cleanup.privateShutdownContext(ctx)
		return scopeErr
	})
	if cleanup != nil {
		err = errors.Join(err, closeNativePrivateFixtureClient(t.Context(), cleanup))
	}
	if err != nil {
		t.Fatalf("canonical managed cleanup failed before any daemon Start: %v", err)
	}
}

func TestRuntimeRestartShutdownRequiresPositiveSettlement(t *testing.T) {
	for _, test := range []struct {
		name    string
		proof   controlCommandSettlement
		closed  bool
		pending bool
		result  CommandResult
		cause   error
	}{
		{name: "false Started is not proof", closed: true},
		{name: "lost frames after owner Start", proof: controlCommandSettlement{ownerStarted: true}, closed: true},
		{name: "completion not published", proof: controlCommandSettlement{ownerStarted: true, commandSettled: true}},
		{name: "actual owner.Start failure", proof: controlCommandSettlement{ownerStartFailed: true}, closed: true, pending: true, cause: syscall.ENOEXEC},
		{name: "verified natural command rejection", proof: controlCommandSettlement{ownerStarted: true, commandSettled: true}, closed: true, pending: true, result: CommandResult{Started: true, ExitCode: 7}, cause: &controlExitError{code: 7}},
		{name: "owner qualification failure is not child completion", proof: controlCommandSettlement{ownerStarted: true, commandSettled: true}, closed: true, pending: false, result: CommandResult{ExitCode: -1}, cause: syscall.EIO},
		{name: "Wait errno is not natural rejection", proof: controlCommandSettlement{ownerStarted: true, commandSettled: true}, closed: true, pending: false, result: CommandResult{Started: true, ExitCode: -1}, cause: syscall.EIO},
	} {
		t.Run(test.name, func(t *testing.T) {
			pending := transport.NewPending(nil, nil)
			proof := test.proof
			proof.pending = pending
			cause := test.cause
			if cause == nil {
				cause = syscall.ENOENT
			}
			testResult := test.result
			testResult.Err = cause
			if test.closed {
				transport.CompletePending(pending, testResult)
			}

			observed := runtimeRestartShutdownFailure(cause, &proof)
			if !test.closed {
				transport.CompletePending(pending, testResult)
			}
			assertRuntimeShutdownProjection(t, observed, cause, test.pending)
		})
	}
}

func assertRuntimeShutdownProjection(t *testing.T, observed, cause error, wantPending bool) {
	t.Helper()
	if errors.Is(observed, ErrPrivateRuntimeRestartPending) != wantPending {
		t.Fatalf("unknown command became retryable: %v", observed)
	}
	if wantPending {
		var diagnostic *PrivateRuntimeRestartError
		if !errors.As(observed, &diagnostic) || !errors.Is(diagnostic.DiagnosticCause(), cause) {
			t.Fatal("handled ordinary diagnostic was discarded")
		}
	} else if !errors.Is(observed, cause) {
		t.Fatal("unknown failure's original cause was discarded")
	}
}

func TestRuntimeRestartShutdownPreservesIndependentFaults(t *testing.T) {
	completed := transport.NewCompletedPending(nil, CommandResult{Started: true, ExitCode: 7})
	proof := &controlCommandSettlement{pending: completed, ownerStarted: true, commandSettled: true}
	exit := &controlExitError{code: 7, message: "ordinary fixture rejection"}
	for _, hard := range []error{ErrBinding, ErrConfiguration, os.ErrPermission, ErrControlLimit, task.ErrIdentityMismatch, errors.New("independent protocol fault")} {
		cause := errors.Join(exit, hard)
		observed := runtimeRestartShutdownFailure(cause, proof)
		if errors.Is(observed, ErrPrivateRuntimeRestartPending) || !errors.Is(observed, cause) {
			t.Fatalf("completed command hid independent fault: %v", observed)
		}
	}
	observed := runtimeRestartShutdownFailure(exit, proof)
	if !errors.Is(observed, ErrPrivateRuntimeRestartPending) {
		t.Fatalf("verified ordinary rejection was not pending: %v", observed)
	}
}

func TestRuntimeRestartRefusesUnmanagedControlBeforeAdmission(t *testing.T) {
	fixture, from := runtimeRestartJournalFixture(t)
	fixture.client.options.ControlOwnerExecutable = ""
	before := snapshotPrivateSuccessorTestState(t, fixture.root)
	err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		_, callErr := fixture.client.QuiesceForRuntimeRestart(ctx, fixture.root, from)
		return callErr
	})
	if !errors.Is(err, ErrInfrastructureOwnerRequired) || !reflect.DeepEqual(before, snapshotPrivateSuccessorTestState(t, fixture.root)) {
		t.Fatalf("unmanaged control admitted/mutated a runtime restart: %v", err)
	}
}

func TestRuntimeRestartMetadataFailureHasNoStartPermission(t *testing.T) {
	proof := &controlCommandSettlement{}
	owner := filepath.Join(t.TempDir(), "unavailable-owner")
	command := exec.Command("/usr/bin/true")
	command.Dir = filepath.Dir(owner)
	pending := startControlOwnedCapturedWithSettlement(t.Context(), owner, command, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, "", nil, nil, proof)
	result, done := pending.Result()
	if !done || !errors.Is(result.Err, ErrConfiguration) || result.Started {
		t.Fatalf("metadata fixture unexpectedly admitted work: done=%t result=%+v", done, result)
	}
	if proof.ownerStartFailed {
		t.Fatal("metadata qualification failure acquired actual owner.Start failure permission")
	}
}
