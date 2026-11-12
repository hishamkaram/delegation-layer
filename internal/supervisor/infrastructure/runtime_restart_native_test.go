package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// This is a real Pueue/server lifetime regression with fixture authority
// callbacks. It does not prove managed resident birth or outside provenance.
type runtimeNativeExpiry struct {
	f               *ownerServerFixture
	client          *pueue.Client
	options         pueue.Options
	from, successor pueue.RuntimeRestartReference
	starts          atomic.Int32
	fault           atomic.Bool
	expiry          atomic.Pointer[context.Context]
	birthRecorded   chan struct{}
	cancel          context.CancelFunc
	finished        chan error
	runtimeResult   error
}

func TestRuntimeRestartNativeInitialExpiryReconcilesSameOwnerWithoutStart(t *testing.T) {
	clientPath, daemonPath := os.Getenv("DELEGATE_TEST_PUEUE"), os.Getenv("DELEGATE_TEST_PUEUED")
	if clientPath == "" || daemonPath == "" {
		t.Skip("native Pueue testkit pair is not configured")
	}
	native := &runtimeNativeExpiry{birthRecorded: make(chan struct{})}
	host := &ownerServerTestHost{restart: native.prepare}
	native.f = newOwnerServerFixtureWithSelection(t, host, ownerServerFixtureSelection{initial: InitialRuntimeRestart, deadline: time.Now().Add(30 * time.Second)})
	t.Cleanup(func() { native.cleanup(t) })
	native.initialize(t, clientPath, daemonPath)
	native.starts.Store(0)
	native.fault.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	native.cancel, native.finished = cancel, make(chan error, 1)
	go func() { native.finished <- runResidentServer(ctx, native.f.server) }()
	select {
	case <-native.birthRecorded:
	case <-time.After(45 * time.Second):
		t.Fatal("canonical successor birth was not durably recorded")
	}
	select {
	case <-native.f.server.initialDone:
	case <-time.After(45 * time.Second):
		t.Fatal("initial observation did not expire after recorded native Start")
	}
	if native.starts.Load() != 1 || !errors.Is(native.f.server.initialError, context.DeadlineExceeded) || native.f.server.initialOutcome.Condition != PreparationPending || !native.f.server.keepsRuntimeReconciliation() {
		t.Fatalf("real initial expiry lost pending condition/current owner: starts=%d outcome=%+v initial=%v", native.starts.Load(), native.f.server.initialOutcome, native.f.server.initialError)
	}
	native.assertSameOwnerReconciliation(t)
}

func (native *runtimeNativeExpiry) assertSameOwnerReconciliation(t *testing.T) {
	t.Helper()
	ctx, stop := context.WithTimeout(t.Context(), 20*time.Second)
	defer stop()
	if target, err := pueue.ObservePrivateExecutionTarget(ctx, native.f.root, native.options); err == nil || target.State() != pueue.ExecutionTargetUnknown {
		t.Fatalf("pending real target advertised fresh execution/custody eligibility: state=%v err=%v", target.State(), err)
	}
	reader := contextPassiveReader(t, native.f.entry.journal)
	owner := native.f.request(t, ownerStatus).Owner
	outcome, err := RequestBoundPreparation(ctx, reader, owner, PreparationRequest{Operation: native.f.entry.operation.Intent().Inputs.Initial})
	if err != nil || outcome.RuntimeRestart.Phase != RuntimeRestartReady || native.starts.Load() != 1 || native.f.server.keepsRuntimeReconciliation() {
		t.Fatalf("recorded birth could not reconcile through same live owner without another Start: outcome=%+v starts=%d err=%v", outcome, native.starts.Load(), err)
	}
}

func (n *runtimeNativeExpiry) initialize(t *testing.T, clientPath, daemonPath string) {
	t.Helper()
	inputs := n.f.entry.operation.Intent().Inputs
	from, err := FormatRuntimeRestartReference(RuntimeRestartReference{RootID: inputs.RootID, Startup: inputs.Initial.RestartFrom})
	if err != nil {
		t.Fatal(err)
	}
	current, err := ReferenceStartup(n.f.entry.operation.Intent())
	if err != nil {
		t.Fatal(err)
	}
	successor, err := FormatRuntimeRestartReference(RuntimeRestartReference{RootID: inputs.RootID, Startup: current})
	if err != nil {
		t.Fatal(err)
	}
	n.from, n.successor = pueue.RuntimeRestartReference(from), pueue.RuntimeRestartReference(successor)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	n.options = pueue.Options{
		ControlOwnerExecutable: self, WaitForPendingClose: true, ObservationTimeout: 15 * time.Second,
		DaemonStartGuard: n, PrivateGenerationGuard: n, DaemonWaitOwner: n.f.server.waits,
		PrivateRuntimeRestartAuthorizer: func(ctx context.Context, root string, from pueue.RuntimeRestartReference, _ task.SupervisorRef, jobs []pueue.Job) error {
			if root != n.f.root || from != n.from || len(jobs) != 0 {
				return pueue.ErrPrivateRuntimeRestartAuthorization
			}
			return pueue.RequireRuntimeRestartBootstrap(ctx, root)
		},
		PrivateRuntimeRestartSuccessorAuthorizer: func(ctx context.Context, root string, from, successor pueue.RuntimeRestartReference, _ task.SupervisorRef) error {
			if root != n.f.root || from != n.from || successor != n.successor {
				return pueue.ErrPrivateRuntimeRestartAuthorization
			}
			return pueue.RequireRuntimeRestartBootstrap(ctx, root)
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err = pueue.WithBootstrapLockContext(ctx, n.f.root, func(locked context.Context) error {
		var prepareErr error
		n.client, prepareErr = pueue.PreparePrivateWithLock(locked, clientPath, daemonPath, n.f.root, n.options)
		if prepareErr != nil {
			return prepareErr
		}
		return n.client.StartPrivate(locked, n.f.root)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (n *runtimeNativeExpiry) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	return n.f.entry.grant.GuardDaemonStart(ctx, root, func() error {
		if err := start(); err != nil {
			return err
		}
		n.starts.Add(1)
		return nil
	})
}

func (n *runtimeNativeExpiry) GuardPrivateGenerationCommit(ctx context.Context, root string, commit func() error) error {
	if err := n.f.entry.grant.GuardPrivateGenerationCommit(ctx, root, commit); err != nil {
		return err
	}
	if !n.fault.Load() {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(root, ".supervisor", "bootstrap.pending"))
	if err != nil {
		return err
	}
	var witness struct {
		Runtime *struct {
			Phase  string `json:"phase"`
			NewPID int    `json:"new_pid"`
		} `json:"runtime_restart"`
	}
	if err = json.Unmarshal(data, &witness); err != nil {
		return err
	}
	if witness.Runtime == nil || witness.Runtime.Phase != "successor_accepted" || witness.Runtime.NewPID <= 0 || !n.fault.CompareAndSwap(true, false) {
		return nil
	}
	close(n.birthRecorded)
	// Settlement deliberately borrows WithoutCancel after Start. Watch the
	// original finite observation, leaving the committed exact birth intact.
	expiry := n.expiry.Load()
	if expiry == nil {
		return errors.New("original initial observation context is missing")
	}
	<-(*expiry).Done()
	// The commit succeeded. Let the real readiness probe observe the expired
	// original context and produce its own classified error.
	return nil
}

func (n *runtimeNativeExpiry) prepare(ctx context.Context, operation Operation, _ LaunchContext, from StartupReference, _ ResidentGuards, _ DaemonWaitOwner) (Disposition, RuntimeRestartReport, error) {
	if n.fault.Load() {
		n.expiry.Store(&ctx)
	}
	var view pueue.PrivateRuntimeRestartObservation
	err := pueue.WithExistingBootstrapLockContext(ctx, n.f.root, func(locked context.Context) error {
		client, err := pueue.PrepareRuntimeRestartWithLock(locked, n.f.root, n.from, n.options)
		if err != nil {
			if client != nil {
				return errors.Join(err, client.CloseAfterPending(err))
			}
			return err
		}
		view, err = client.QuiesceForRuntimeRestart(locked, n.f.root, n.from)
		if err == nil {
			view, err = client.StartRuntimeRestartSuccessor(locked, n.f.root, n.from, n.successor)
		}
		return errors.Join(err, client.CloseAfterPending(err))
	})
	report, refErr := nativeExpiryReport(view, operation.Intent().Inputs.RootID, from)
	disposition := Disposition{}
	if report.Phase == RuntimeRestartReady && err == nil {
		disposition.Kind = DispositionWarmReuse
	}
	return disposition, report, errors.Join(err, refErr)
}

func nativeExpiryReport(view pueue.PrivateRuntimeRestartObservation, rootID string, expected StartupReference) (RuntimeRestartReport, error) {
	if view == (pueue.PrivateRuntimeRestartObservation{}) {
		return RuntimeRestartReport{}, nil
	}
	from, err := ParseRuntimeRestartReference(string(view.From))
	if err != nil || from.RootID != rootID || from.Startup != expected {
		return RuntimeRestartReport{}, errors.Join(ErrIdentity, err)
	}
	// The test edge narrows the core phase only after domain validation below.
	report := RuntimeRestartReport{From: expected, Phase: RuntimeRestartPhase(view.Phase)}
	if view.Successor != "" {
		successor, parseErr := ParseRuntimeRestartReference(string(view.Successor))
		if parseErr != nil || successor.RootID != rootID {
			return RuntimeRestartReport{}, errors.Join(ErrIdentity, parseErr)
		}
		report.Successor = successor.Startup
	}
	return report, report.Validate()
}

func (n *runtimeNativeExpiry) cleanup(t *testing.T) {
	t.Helper()
	n.fault.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cleanupErr := n.stopDaemon(ctx)
	if cleanupErr == nil {
		cleanupErr = n.joinDaemonWaits(ctx)
	}
	cleanupErr = errors.Join(cleanupErr, n.joinRuntime(ctx))
	if n.runtimeResult != nil {
		t.Logf("joined native runtime operation classification: %+v", (&componentEntryHost{}).ClassifyPreparation(n.runtimeResult))
	}
	if cleanupErr != nil {
		n.f.retain = true
		t.Errorf("retained native expiry fixture %s: %v", n.f.root, cleanupErr)
	}
}

func (n *runtimeNativeExpiry) stopDaemon(ctx context.Context) error {
	if n.client != nil {
		cleanupErr := pueue.WithExistingBootstrapLockContext(ctx, n.f.root, func(locked context.Context) error {
			// Creator-owned cleanup stops this exact private fixture daemon;
			// it never fabricates a completed restart receipt to permit shutdown.
			if n.f.server.waits.hasActiveWait() {
				return n.client.Shutdown(locked)
			}
			return nil
		})
		return errors.Join(cleanupErr, n.client.CloseAfterPending(cleanupErr))
	}
	return nil
}

func (n *runtimeNativeExpiry) joinDaemonWaits(ctx context.Context) error {
	n.f.server.waits.mu.Lock()
	var waits []<-chan struct{}
	for done := range n.f.server.waits.active {
		waits = append(waits, done)
	}
	n.f.server.waits.mu.Unlock()
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (n *runtimeNativeExpiry) joinRuntime(ctx context.Context) error {
	if n.cancel != nil {
		n.cancel()
	}
	if n.finished != nil {
		select {
		case result := <-n.finished:
			n.runtimeResult = result
			n.f.expectedCleanupFault = result
			n.f.server = nil
			if retainsResidentCleanup(result) {
				return result
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
