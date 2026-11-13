package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRuntimeRestartPurposePreparationRequiresMatchingEX(t *testing.T) {
	fixture, from := runtimeRestartJournalFixture(t)
	acceptRuntimeRestartTest(t, fixture, from)
	before := snapshotPrivateSuccessorTestState(t, fixture.root)
	if err := RequireRuntimeRestartBootstrap(t.Context(), fixture.root); !errors.Is(err, ErrBinding) {
		t.Fatalf("untagged EX accepted: %v", err)
	}
	if client, err := PrepareRuntimeRestartWithLock(t.Context(), fixture.root, from, fixture.client.options); client != nil || !errors.Is(err, ErrBinding) {
		t.Fatalf("untagged preparation accepted: %v", err)
	}
	var borrowed context.Context
	err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		borrowed = ctx
		if err := RequireRuntimeRestartBootstrap(ctx, fixture.root); err != nil {
			return err
		}
		wrong := runtimeRestartTestReference(t, fixture.root, 2)
		if client, err := PrepareRuntimeRestartWithLock(ctx, fixture.root, wrong, fixture.client.options); client != nil || !errors.Is(err, ErrPrivateRuntimeRestartReferenceNotRetained) {
			t.Fatalf("foreign restart preparation accepted: %v", err)
		}
		client, err := PrepareRuntimeRestartWithLock(ctx, fixture.root, from, fixture.client.options)
		if err != nil {
			return err
		}
		if client.Binding() != fixture.client.Binding() {
			t.Error("purpose preparation changed published binding")
		}
		return client.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = RequireRuntimeRestartBootstrap(borrowed, fixture.root); err == nil {
		t.Fatal("expired EX accepted")
	}
	if after := snapshotPrivateSuccessorTestState(t, fixture.root); !reflect.DeepEqual(before, after) {
		t.Fatal("read-only purpose preparation changed private state")
	}
}

func TestRuntimeRestartSuccessorRequiresIndependentAuthorization(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "rejected"}[reject], func(t *testing.T) {
			fixture, from := runtimeRestartJournalFixture(t)
			record := acceptRuntimeRestartTest(t, fixture, from)
			record.Phase = RuntimeRestartDaemonClosed
			if err := writeRuntimeRestartReplacement(fixture.root, record); err != nil {
				t.Fatal(err)
			}
			before := snapshotPrivateSuccessorTestState(t, fixture.root)
			denied := errors.New("successor authority refused")
			expected := error(ErrPrivateRuntimeRestartAuthorization)
			if reject {
				expected = denied
				fixture.client.options.PrivateRuntimeRestartSuccessorAuthorizer = func(context.Context, string, RuntimeRestartReference, RuntimeRestartReference, task.SupervisorRef) error {
					return denied
				}
			}
			startCalls := 0
			fixture.client.options.DaemonStartGuard = daemonStartGuardFunc(func(context.Context, string, func() error) error {
				startCalls++
				return denied
			})
			err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
				_, err := fixture.client.StartRuntimeRestartSuccessor(ctx, fixture.root, from, runtimeRestartTestReference(t, fixture.root, 2))
				return err
			})
			if !errors.Is(err, expected) || startCalls != 0 {
				t.Fatalf("missing/rejected purpose authority reached Start: calls=%d err=%v", startCalls, err)
			}
			if after := snapshotPrivateSuccessorTestState(t, fixture.root); !reflect.DeepEqual(before, after) {
				t.Fatal("refused successor changed durable state")
			}
		})
	}
}

func TestRuntimeRestartUnknownBirthNeverReplaysStart(t *testing.T) {
	fixture, from := runtimeRestartJournalFixture(t)
	record := acceptRuntimeRestartTest(t, fixture, from)
	record.Phase = RuntimeRestartSuccessorAccepted
	record.Successor = runtimeRestartTestReference(t, fixture.root, 2)
	if err := writeRuntimeRestartReplacement(fixture.root, record); err != nil {
		t.Fatal(err)
	}
	fixture.client.options.PrivateRuntimeRestartSuccessorAuthorizer = func(context.Context, string, RuntimeRestartReference, RuntimeRestartReference, task.SupervisorRef) error {
		return nil
	}
	calls := 0
	fixture.client.options.DaemonStartGuard = daemonStartGuardFunc(func(context.Context, string, func() error) error {
		calls++
		return ErrBinding
	})
	before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.root))
	for range 2 {
		err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
			view, err := fixture.client.StartRuntimeRestartSuccessor(ctx, fixture.root, from, record.Successor)
			if view.Phase != RuntimeRestartSuccessorAccepted {
				t.Errorf("ambiguous receipt lost: %+v", view)
			}
			return err
		})
		var failure *PrivateRuntimeRestartError
		if !errors.As(err, &failure) || failure.ReasonCode != "successor_birth_unknown" || !errors.Is(err, ErrUnknown) || !errors.Is(err, ErrPrivateRuntimeRestartPending) {
			t.Fatalf("unknown birth gained retry authority: %v", err)
		}
	}
	if calls != 0 || !bytes.Equal(before, readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.root))) {
		t.Fatal("ambiguous successor was restarted or its proof overwritten")
	}
}

func TestRuntimeRestartProcessComparisonUsesCanonicalIncarnation(t *testing.T) {
	client := &Client{options: Options{processObserver: func(int) (privateProcessObservation, error) {
		return privateProcessObservation{Present: true, StartToken: "darwin:12:34"}, nil
	}}}
	for _, test := range []struct {
		start  string
		absent bool
	}{
		{start: "darwin-v2:12:34:99"},
		{start: "darwin:12:34"},
		{start: "darwin-v2:12:35:99", absent: true},
	} {
		absent, err := client.privateProcessUnavailable(privateProcessIdentity{PID: 17, StartToken: test.start})
		if err != nil || absent != test.absent {
			t.Fatalf("identity %q: absent=%t err=%v", test.start, absent, err)
		}
	}
}

type runtimeRestartTestWaitOwner struct {
	legacy DaemonWaitOwner
	starts []privateDaemonStart
}

func (owner *runtimeRestartTestWaitOwner) RegisterDaemonWait(root string, pid int, done <-chan struct{}) error {
	return owner.legacy.RegisterDaemonWait(root, pid, done)
}

func (owner *runtimeRestartTestWaitOwner) RegisterDaemonWaitBirth(root string, pid int, done <-chan struct{}, birth func() (lifetime.ProcessRef, bool, error)) error {
	owner.starts = append(owner.starts, privateDaemonStart{done: done, birth: birth})
	return owner.legacy.RegisterDaemonWait(root, pid, done)
}

func (owner *runtimeRestartTestWaitOwner) RequireDaemonWaitCustody(_ context.Context, _ string, expected lifetime.ProcessRef) error {
	for _, started := range owner.starts {
		birth, ready, err := started.birth()
		if err != nil || !ready {
			return errors.Join(ErrInfrastructureOwnerRequired, err)
		}
		same, err := lifetime.SameProcess(birth, expected)
		if err != nil {
			return err
		}
		if same {
			select {
			case <-started.done:
				return ErrInfrastructureOwnerRequired
			default:
				return nil
			}
		}
	}
	return ErrInfrastructureOwnerRequired
}

// These are actual Pueue processes with fixture authority callbacks. They do
// not prove infrastructure resident closure, outside provenance or provider auth.
func runtimeRestartNativeFixture(t *testing.T) (nativePrivateSupervisorFixture, *Client, *runtimeRestartTestWaitOwner, RuntimeRestartReference) {
	t.Helper()
	fixture := newNativePrivateSupervisorFixture(t)
	owner := &runtimeRestartTestWaitOwner{}
	client, err := fixture.bindWithControlOwner(t, func(legacy DaemonWaitOwner) DaemonWaitOwner {
		owner.legacy = legacy
		return owner
	}, os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	from := runtimeRestartTestReference(t, fixture.stateRoot, 1)
	client.options.PrivateRuntimeRestartAuthorizer = func(ctx context.Context, root string, reference RuntimeRestartReference, binding task.SupervisorRef, jobs []Job) error {
		if root != fixture.stateRoot || reference != from || binding != client.Binding() || len(jobs) != 0 {
			return ErrPrivateRuntimeRestartAuthorization
		}
		return RequireRuntimeRestartBootstrap(ctx, root)
	}
	client.options.PrivateRuntimeRestartSuccessorAuthorizer = func(ctx context.Context, root string, previous, successor RuntimeRestartReference, binding task.SupervisorRef) error {
		if previous != from || successor != runtimeRestartTestReference(t, fixture.stateRoot, 2) || binding != client.Binding() {
			return ErrPrivateRuntimeRestartAuthorization
		}
		return RequireRuntimeRestartBootstrap(ctx, root)
	}
	return fixture, client, owner, from
}

func runtimeRestartNativeQuiesce(t *testing.T, root string, client *Client, from RuntimeRestartReference) PrivateRuntimeRestartObservation {
	t.Helper()
	var view PrivateRuntimeRestartObservation
	err := WithExistingBootstrapLockContext(runtimeRestartNativeContext(t), root, func(ctx context.Context) error {
		var err error
		view, err = client.QuiesceForRuntimeRestart(ctx, root, from)
		return err
	})
	if err != nil || view.Phase != RuntimeRestartDaemonClosed {
		t.Fatalf("native quiescence: view=%+v err=%v", view, err)
	}
	return view
}

func TestPrivateSupervisorNativeRuntimeRestart(t *testing.T) {
	fixture, client, owner, from := runtimeRestartNativeFixture(t)
	before := runtimeRestartImmutableNativeFiles(t, fixture.stateRoot)
	old, err := lifetime.ObserveProcess(ownerPID(t, owner.starts[0]))
	if err != nil {
		t.Fatal(err)
	}
	runtimeRestartNativeQuiesce(t, fixture.stateRoot, client, from)
	if !joinDaemonWait(t, owner.starts[0].done) {
		return
	}
	successor := runtimeRestartTestReference(t, fixture.stateRoot, 2)
	startCalls := 0
	client.options.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		startCalls++
		return start()
	})
	var completed PrivateRuntimeRestartObservation
	err = WithExistingBootstrapLockContext(runtimeRestartNativeContext(t), fixture.stateRoot, func(ctx context.Context) error {
		prepared, prepareErr := PrepareRuntimeRestartWithLock(ctx, fixture.stateRoot, from, client.options)
		if prepareErr != nil {
			return prepareErr
		}
		defer func() {
			if closeErr := prepared.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
		completed, err = prepared.StartRuntimeRestartSuccessor(ctx, fixture.stateRoot, from, successor)
		return err
	})
	if err != nil || completed.Phase != RuntimeRestartReady || completed.Successor != successor || startCalls != 1 || len(owner.starts) != 2 {
		t.Fatalf("native successor: view=%+v starts=%d registry=%d err=%v", completed, startCalls, len(owner.starts), err)
	}
	current, birthErr := awaitDaemonBirth(t, owner.starts[1].birth)
	same, sameErr := lifetime.SameProcess(current, old)
	if birthErr != nil || sameErr != nil || same {
		t.Fatalf("successor reused predecessor incarnation: birth=%v same=%t err=%v", birthErr, same, sameErr)
	}
	assertRuntimeRestartNativeRetries(t, fixture.stateRoot, client, from, successor, &startCalls)
	if after := runtimeRestartImmutableNativeFiles(t, fixture.stateRoot); !reflect.DeepEqual(before, after) {
		t.Fatal("same-binding restart changed immutable root/config/generation bytes")
	}
}

func ownerPID(t *testing.T, started privateDaemonStart) int {
	t.Helper()
	birth, err := awaitDaemonBirth(t, started.birth)
	if err != nil {
		t.Fatal(err)
	}
	return birth.PID()
}

func runtimeRestartImmutableNativeFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	paths := []string{filepath.Join(root, "root.json"), PrivateConfigPath(root), privateSupervisorBindingPath(root), privateDaemonIdentityPath(filepath.Join(root, privateSupervisorDirectory))}
	result := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result[path] = data
	}
	return result
}

func assertRuntimeRestartNativeRetries(t *testing.T, root string, client *Client, from, successor RuntimeRestartReference, starts *int) {
	t.Helper()
	before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(root))
	err := WithExistingBootstrapLockContext(runtimeRestartNativeContext(t), root, func(ctx context.Context) error {
		view, err := client.QuiesceForRuntimeRestart(ctx, root, from)
		if err != nil || view.Phase != RuntimeRestartReady {
			return errors.Join(ErrBinding, err)
		}
		view, err = client.StartRuntimeRestartSuccessor(ctx, root, from, successor)
		if err != nil || view.Phase != RuntimeRestartReady {
			return errors.Join(ErrBinding, err)
		}
		_, err = client.StartRuntimeRestartSuccessor(ctx, root, runtimeRestartTestReference(t, root, 3), successor)
		if !errors.Is(err, ErrPrivateRuntimeRestartReferenceNotRetained) {
			t.Errorf("unknown original accepted: %v", err)
		}
		return nil
	})
	if err != nil || *starts != 1 || !bytes.Equal(before, readPrivateSupervisorFile(t, privateBootstrapRecordPath(root))) {
		t.Fatalf("duplicate/lost completed reply changed restart: starts=%d err=%v", *starts, err)
	}
}

func TestPrivateSupervisorNativeRuntimeRestartStartFaults(t *testing.T) {
	for _, postStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_start", true: "after_start"}[postStart], func(t *testing.T) {
			testRuntimeRestartNativeStartFault(t, postStart)
		})
	}
}

func testRuntimeRestartNativeStartFault(t *testing.T, postStart bool) {
	t.Helper()
	fixture, client, owner, from := runtimeRestartNativeFixture(t)
	runtimeRestartNativeQuiesce(t, fixture.stateRoot, client, from)
	denied := errors.New("fixture Start guard fault")
	actualStarts := 0
	client.options.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		if !postStart {
			return denied
		}
		actualStarts++
		return errors.Join(start(), denied)
	})
	successor := runtimeRestartTestReference(t, fixture.stateRoot, 2)
	var faulted PrivateRuntimeRestartObservation
	err := WithExistingBootstrapLockContext(runtimeRestartNativeContext(t), fixture.stateRoot, func(ctx context.Context) error {
		var err error
		faulted, err = client.StartRuntimeRestartSuccessor(ctx, fixture.stateRoot, from, successor)
		return err
	})
	if !errors.Is(err, denied) {
		t.Fatalf("Start fault was lost: %v", err)
	}
	wantPhase := RuntimeRestartDaemonClosed
	if postStart {
		wantPhase = RuntimeRestartSuccessorAccepted
	}
	if faulted.Phase != wantPhase {
		t.Fatalf("faulted phase = %s, want %s", faulted.Phase, wantPhase)
	}
	client.options.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		actualStarts++
		return start()
	})
	if postStart {
		if readyErr := waitPrivateReady(runtimeRestartNativeContext(t), client, nativePrivateObservationTimeout, false); readyErr != nil {
			t.Fatal(readyErr)
		}
	}
	err = WithExistingBootstrapLockContext(runtimeRestartNativeContext(t), fixture.stateRoot, func(ctx context.Context) error {
		view, resumeErr := client.StartRuntimeRestartSuccessor(ctx, fixture.stateRoot, from, successor)
		if view.Phase != RuntimeRestartReady {
			t.Errorf("recovered phase = %s", view.Phase)
		}
		return resumeErr
	})
	if err != nil || actualStarts != 1 || len(owner.starts) != 2 {
		t.Fatalf("fault recovery duplicated/lost native Start: actual=%d custody=%d err=%v", actualStarts, len(owner.starts), err)
	}
}

func TestPrivateSupervisorNativeRuntimeRestartRefusesBeforeAcceptance(t *testing.T) {
	fixture, client, _, from := runtimeRestartNativeFixture(t)
	before := runtimeRestartImmutableNativeFiles(t, fixture.stateRoot)
	original := client.options
	for _, fault := range []string{"missing_wait", "denied_policy", "revoked_generation"} {
		t.Run(fault, func(t *testing.T) {
			client.options = original
			denied := errors.New("fixture current authority revoked")
			expected := error(ErrInfrastructureOwnerRequired)
			switch fault {
			case "missing_wait":
				client.options.DaemonWaitOwner = nil
			case "denied_policy":
				expected = denied
				client.options.PrivateRuntimeRestartAuthorizer = func(context.Context, string, RuntimeRestartReference, task.SupervisorRef, []Job) error { return denied }
			case "revoked_generation":
				expected = denied
				client.options.PrivateGenerationGuard = generationGuardFunc(func(context.Context, string, func() error) error { return denied })
			}
			err := WithExistingBootstrapLockContext(runtimeRestartNativeContext(t), fixture.stateRoot, func(ctx context.Context) error {
				_, err := client.QuiesceForRuntimeRestart(ctx, fixture.stateRoot, from)
				return err
			})
			if !errors.Is(err, expected) {
				t.Fatalf("preacceptance refusal = %v", err)
			}
			if _, found, err := readPrivateBootstrapRecord(fixture.stateRoot); err != nil || found {
				t.Fatalf("refusal accepted journal: found=%t err=%v", found, err)
			}
			if err := client.Ready(t.Context()); err != nil {
				t.Fatalf("refusal stopped daemon: %v", err)
			}
		})
	}
	client.options = original
	if after := runtimeRestartImmutableNativeFiles(t, fixture.stateRoot); !reflect.DeepEqual(before, after) {
		t.Fatal("refusal changed immutable files")
	}
}

func runtimeRestartNativeContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*nativePrivateObservationTimeout)
	t.Cleanup(cancel)
	return ctx
}

func TestRuntimeRestartCanceledUnstartedSettlement(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "current_guard", true: "revoked_guard"}[revoked], func(t *testing.T) {
			testRuntimeRestartCanceledUnstarted(t, revoked)
		})
	}
}

func testRuntimeRestartCanceledUnstarted(t *testing.T, revoked bool) {
	t.Helper()
	fixture, from := runtimeRestartJournalFixture(t)
	record := acceptRuntimeRestartTest(t, fixture, from)
	record.Phase = RuntimeRestartDaemonClosed
	if err := writeRuntimeRestartReplacement(fixture.root, record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture.client.options.DaemonWaitOwner = &runtimeRestartTestWaitOwner{}
	fixture.client.options.PrivateRuntimeRestartSuccessorAuthorizer = func(context.Context, string, RuntimeRestartReference, RuntimeRestartReference, task.SupervisorRef) error {
		return nil
	}
	guardRevoked := errors.New("fixture current generation revoked")
	fixture.client.options.PrivateGenerationGuard = generationGuardFunc(func(guarded context.Context, _ string, commit func() error) error {
		if revoked && ctx.Err() != nil {
			return guardRevoked
		}
		return commit()
	})
	fixture.client.options.DaemonStartGuard = daemonStartGuardFunc(func(context.Context, string, func() error) error {
		// The successor acceptance is already durable. No Start callback
		// is called, so this live invocation can settle known no-Start.
		cancel()
		return context.Canceled
	})
	var view PrivateRuntimeRestartObservation
	err := WithExistingBootstrapLockContext(ctx, fixture.root, func(locked context.Context) error {
		var err error
		view, err = fixture.client.StartRuntimeRestartSuccessor(locked, fixture.root, from, runtimeRestartTestReference(t, fixture.root, 2))
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was hidden: %v", err)
	}
	want := RuntimeRestartDaemonClosed
	if revoked {
		want = RuntimeRestartSuccessorAccepted
	}
	if view.Phase != want {
		t.Fatalf("settled phase = %s, want %s", view.Phase, want)
	}
	if revoked && !errors.Is(err, guardRevoked) {
		t.Fatalf("cleanup authority error hidden: %v", err)
	}
	assertPrivateDaemonNotStarted(t, fixture.base)
}

func assertPrivateDaemonNotStarted(t *testing.T, base string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(base, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal launched fixture daemon: %v", err)
	}
}

func TestPrivateSupervisorNativeRuntimeRestartCanceledBirthSettlement(t *testing.T) {
	fixture, client, owner, from := runtimeRestartNativeFixture(t)
	runtimeRestartNativeQuiesce(t, fixture.stateRoot, client, from)
	ctx, cancel := context.WithCancel(runtimeRestartNativeContext(t))
	defer cancel()
	client.options.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		startErr := start()
		cancel()
		return startErr
	})
	var view PrivateRuntimeRestartObservation
	err := WithExistingBootstrapLockContext(ctx, fixture.stateRoot, func(locked context.Context) error {
		var resumeErr error
		view, resumeErr = client.StartRuntimeRestartSuccessor(locked, fixture.stateRoot, from, runtimeRestartTestReference(t, fixture.stateRoot, 2))
		return resumeErr
	})
	if !errors.Is(err, context.Canceled) || view.Phase != RuntimeRestartSuccessorAccepted || len(owner.starts) != 2 {
		t.Fatalf("canceled native Start lost ownership: view=%+v starts=%d err=%v", view, len(owner.starts), err)
	}
	assertRuntimeRestartObservationPending(t, err)
	bootstrap, found, readErr := readPrivateBootstrapRecord(fixture.stateRoot)
	if readErr != nil || !found || bootstrap.RuntimeRestart.NewPID == 0 {
		t.Fatalf("canceled Start lost exact birth: found=%t err=%v", found, readErr)
	}
	birth, birthErr := awaitDaemonBirth(t, owner.starts[1].birth)
	if birthErr != nil || bootstrap.RuntimeRestart.NewPID != birth.PID() || bootstrap.RuntimeRestart.NewProcessStart != birth.StartToken() {
		t.Fatalf("settled birth differs from immutable canonical Start: %v", birthErr)
	}
	client.options.DaemonStartGuard = daemonStartGuardFunc(func(context.Context, string, func() error) error {
		t.Error("birth reconciliation attempted Start")
		return ErrBinding
	})
	if readyErr := waitPrivateReady(runtimeRestartNativeContext(t), client, nativePrivateObservationTimeout, false); readyErr != nil {
		t.Fatal(readyErr)
	}
	err = WithExistingBootstrapLockContext(runtimeRestartNativeContext(t), fixture.stateRoot, func(locked context.Context) error {
		var resumeErr error
		view, resumeErr = client.StartRuntimeRestartSuccessor(locked, fixture.stateRoot, from, runtimeRestartTestReference(t, fixture.stateRoot, 2))
		return resumeErr
	})
	if err != nil || view.Phase != RuntimeRestartReady || len(owner.starts) != 2 {
		t.Fatalf("canceled Start recovery: view=%+v err=%v", view, err)
	}
}

func TestPrivateSupervisorNativeRuntimeRestartCanceledShutdownReconciles(t *testing.T) {
	fixture, client, owner, from := runtimeRestartNativeFixture(t)
	ctx, cancel := context.WithCancel(runtimeRestartNativeContext(t))
	defer cancel()
	var shutdownStarts atomic.Int32
	client.options.Observer = func(event CommandEvent) {
		if event.Stage == "started" && len(event.Argv) > 0 && event.Argv[len(event.Argv)-1] == "shutdown" {
			shutdownStarts.Add(1)
			cancel()
		}
	}
	err := WithExistingBootstrapLockContext(ctx, fixture.stateRoot, func(locked context.Context) error {
		_, err := client.QuiesceForRuntimeRestart(locked, fixture.stateRoot, from)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown cancellation lost: %v", err)
	}
	if pending := pendingFrom(err); pending != nil {
		if _, completionErr := awaitPending(runtimeRestartNativeContext(t), pending); completionErr != nil {
			t.Fatal(completionErr)
		}
	}
	if !joinDaemonWait(t, owner.starts[0].done) {
		return
	}
	client.options.Observer = nil
	bootstrap, found, readErr := readPrivateBootstrapRecord(fixture.stateRoot)
	if readErr != nil || !found || bootstrap.RuntimeRestart.From != from || bootstrap.RuntimeRestart.Phase == RuntimeRestartReady {
		t.Fatalf("canceled shutdown lost admission fence: found=%t err=%v", found, readErr)
	}
	runtimeRestartNativeQuiesce(t, fixture.stateRoot, client, from)
	if shutdownStarts.Load() != 1 || len(owner.starts) != 1 {
		t.Fatal("reconciliation replayed shutdown/Start")
	}
}

func TestRuntimeRestartRetainedAliasResume(t *testing.T) {
	fixture, from := runtimeRestartJournalFixture(t)
	if err := fixture.client.PersistPrivateRecoveryBindingContext(t.Context(), fixture.root); err != nil {
		t.Fatal(err)
	}
	aliasBefore := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.root))
	configBefore := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.root))
	acceptRuntimeRestartTest(t, fixture, from)
	pendingBefore := readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.root))
	wrongFrom := runtimeRestartTestReference(t, fixture.root, 2)
	wrongErr := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		_, err := fixture.client.QuiesceForRuntimeRestart(ctx, fixture.root, wrongFrom)
		return err
	})
	if !errors.Is(wrongErr, ErrPrivateRuntimeRestartReferenceNotRetained) || !bytes.Equal(pendingBefore, readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.root))) {
		t.Fatalf("foreign reference gained retained-alias scope: %v", wrongErr)
	}
	fixture.client.options.processObserver = func(int) (privateProcessObservation, error) { return privateProcessObservation{}, nil }
	err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		view, err := fixture.client.QuiesceForRuntimeRestart(ctx, fixture.root, from)
		if view.Phase != RuntimeRestartDaemonClosed {
			t.Errorf("resumed retained-alias phase = %s", view.Phase)
		}
		return err
	})
	if err != nil {
		t.Fatalf("same accepted operation blocked by its cleanup-only alias: %v", err)
	}
	if !bytes.Equal(aliasBefore, readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.root))) || !bytes.Equal(configBefore, readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.root))) {
		t.Fatal("runtime resume mutated retained alias/config")
	}
	if complete, err := PrivateSupervisorMigrationComplete(fixture.root, fixture.client.Binding()); complete || !errors.Is(err, ErrPrivateRuntimeRestartPending) {
		t.Fatalf("matching resume scope leaked into generic admission: complete=%t err=%v", complete, err)
	}
}

func TestRuntimeRestartPreparationPreservesUnreadableBinding(t *testing.T) {
	fixture, from := runtimeRestartJournalFixture(t)
	path := privateSupervisorBindingPath(fixture.root)
	corrupt := []byte("{")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		client, err := PrepareRuntimeRestartWithLock(ctx, fixture.root, from, fixture.client.options)
		if client != nil {
			t.Error("unreadable publication produced a client")
		}
		return err
	})
	if err == nil || errors.Is(err, ErrPrivateSupervisorNotInitialized) || !bytes.Equal(corrupt, readPrivateSupervisorFile(t, path)) {
		t.Fatalf("unreadable binding became absent/repaired: %v", err)
	}
}

func TestRuntimeRestartAliasScopePreservesCompetingTransitions(t *testing.T) {
	for _, transition := range []string{"migration", "identity-publication"} {
		t.Run(transition, func(t *testing.T) {
			fixture, from := runtimeRestartJournalFixture(t)
			if err := fixture.client.PersistPrivateRecoveryBindingContext(t.Context(), fixture.root); err != nil {
				t.Fatal(err)
			}
			acceptRuntimeRestartTest(t, fixture, from)
			installRuntimeRestartCompetingTransition(t, fixture, transition)
			fixture.client.options.processObserver = func(int) (privateProcessObservation, error) { return privateProcessObservation{}, nil }
			before := snapshotPrivateSuccessorTestState(t, fixture.root)
			err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
				_, err := fixture.client.QuiesceForRuntimeRestart(ctx, fixture.root, from)
				return err
			})
			if err == nil || !reflect.DeepEqual(before, snapshotPrivateSuccessorTestState(t, fixture.root)) {
				t.Fatalf("runtime alias scope ignored/mutated competing %s: %v", transition, err)
			}
		})
	}
}

func TestRuntimeRestartReadinessKeepsLegacyProjectionSeparate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &Client{}
	legacy := client.Ready(ctx)
	if !errors.Is(legacy, ErrBinding) || !errors.Is(legacy, context.Canceled) {
		t.Fatalf("ordinary Ready lost its existing error projection: %v", legacy)
	}
	observed := client.runtimeRestartReadiness(ctx)
	classified := runtimeRestartReadinessFailure(observed)
	if !errors.Is(classified, context.Canceled) || !errors.Is(classified, ErrPrivateRuntimeRestartPending) || errors.Is(classified, ErrBinding) {
		t.Fatalf("runtime observation expiry became a binding fault: %v", classified)
	}
	var pending *PrivateRuntimeRestartError
	if !errors.As(classified, &pending) || pending.ReasonCode != "successor_not_ready" {
		t.Fatalf("runtime readiness lost its bounded pending reason: %v", classified)
	}
}

type runtimeReadinessRetainedError struct{ cause error }

func (e *runtimeReadinessRetainedError) Error() string         { return e.cause.Error() }
func (e *runtimeReadinessRetainedError) Unwrap() error         { return e.cause }
func (*runtimeReadinessRetainedError) RetainsResidentCleanup() {}

func TestRuntimeRestartReadinessPreservesIndependentFaults(t *testing.T) {
	guardFault := errors.New("fixture generation guard revoked")
	for _, fault := range []struct {
		name  string
		cause error
	}{
		{"binding", ErrBinding},
		{"configuration", ErrConfiguration},
		{"schema", task.ErrUnsupportedSchema},
		{"storage", os.ErrPermission},
		{"guard", guardFault},
		{"Is alias", runtimeReadinessDeadlineAliasError{}},
		{"non-comparable", runtimeReadinessOpaqueError("fixture")},
		{"retained cleanup", &runtimeReadinessRetainedError{cause: context.DeadlineExceeded}},
	} {
		t.Run(fault.name, func(t *testing.T) {
			original := errors.Join(context.DeadlineExceeded, fault.cause)
			observed := runtimeRestartReadinessFailure(original)
			if errors.Is(observed, ErrPrivateRuntimeRestartPending) || !errors.Is(observed, context.DeadlineExceeded) || !errors.Is(observed, original) {
				t.Fatalf("observation deadline masked independent %s: %v", fault.name, observed)
			}
		})
	}
	original := errors.Join(context.DeadlineExceeded, ErrInFlight)
	observed := runtimeRestartReadinessFailure(original)
	if !errors.Is(observed, ErrPrivateRuntimeRestartPending) || !errors.Is(observed, ErrInFlight) || !errors.Is(observed, context.DeadlineExceeded) {
		t.Fatalf("finite pending-command observation lost its causes: %v", observed)
	}
}

func TestRuntimeRestartReadinessStillValidatesActualBinding(t *testing.T) {
	fixture := newGenerationGuardFixture(t)
	if err := os.WriteFile(fixture.client.Binding().ConfigPath, []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed := runtimeRestartReadinessFailure(fixture.client.runtimeRestartReadiness(t.Context()))
	if !errors.Is(observed, ErrBinding) || errors.Is(observed, ErrPrivateRuntimeRestartPending) {
		t.Fatalf("runtime readiness bypassed actual config validation: %v", observed)
	}
}

func assertRuntimeRestartObservationPending(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrPrivateRuntimeRestartPending) || errors.Is(err, ErrBinding) {
		t.Fatalf("known-born observation expiry became a binding fault: %v", err)
	}
}

type runtimeReadinessDeadlineAliasError struct{}

func (runtimeReadinessDeadlineAliasError) Error() string { return "fixture Is alias" }
func (runtimeReadinessDeadlineAliasError) Is(target error) bool {
	return errors.Is(target, context.DeadlineExceeded)
}

type runtimeReadinessOpaqueError []byte

func (runtimeReadinessOpaqueError) Error() string { return "opaque fixture fault" }
