package infrastructure

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRunOwnerModeDecidesRoutingBeforeDescriptorInspection(t *testing.T) {
	before, beforeErr := unix.FcntlInt(3, unix.F_GETFD, 0)
	for _, test := range []struct {
		args    []string
		handled bool
	}{{[]string{"--ordinary"}, false}, {[]string{"--infrastructure-owner"}, true}, {[]string{"--ordinary", "--infrastructure-owner=invalid"}, true}} {
		handled, err := RunOwnerMode(t.Context(), test.args, nil)
		if handled != test.handled || (test.handled && !errors.Is(err, ErrIdentity)) || (!test.handled && err != nil) {
			t.Fatalf("mode routing: handled=%t err=%v", handled, err)
		}
	}
	after, afterErr := unix.FcntlInt(3, unix.F_GETFD, 0)
	if before != after || !errors.Is(afterErr, beforeErr) {
		t.Fatalf("routing touched inherited descriptor: before=%d/%v after=%d/%v", before, beforeErr, after, afterErr)
	}
}

// This test uses real production journals and native server actors. It does not
// attest a managed-image child birth or daemon shutdown.
func TestOwnerRuntimeJoinsInitialPreparationBeforeClosingBorrowers(t *testing.T) {
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	liveWait := make(chan struct{})
	var waitOnce sync.Once
	completeWait := func() { waitOnce.Do(func() { close(liveWait) }) }
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(ctx context.Context, op Operation, _ LaunchContext, _ DaemonStartGuard, waits DaemonWaitOwner) (Disposition, error) {
		if err := waits.RegisterDaemonWait(op.Intent().Inputs.Root, os.Getpid(), liveWait); err != nil {
			return Disposition{}, err
		}
		entered <- ctx
		<-release
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	cancel, done, join := startRuntimeFixture(t, f)
	t.Cleanup(func() { cancel(); unblock(); completeWait(); join() })
	initial := awaitOwnerServerHost(t, entered)
	cancel()
	assertInitialRuntimeBorrower(t, f, initial, done)
	unblock()
	select {
	case <-f.server.drained:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not drain accepted handlers")
	}
	select {
	case <-done:
		t.Fatal("runtime discarded the daemon's outstanding sole Wait")
	case <-time.After(100 * time.Millisecond):
	}
	awaitServingResourceRelease(t, f.entry, done)
	assertRuntimeInstanceLeaseFree(t, f.root)
	completeWait() // The actual command owner alone reports completion.
	join()
	if f.entry.store != nil || f.entry.journal != nil || f.entry.lease != nil || f.entry.endpoint != nil {
		t.Fatal("runtime retained borrowers after handlers and actual Wait settled")
	}
}

func assertInitialRuntimeBorrower(t *testing.T, f *ownerServerFixture, initial context.Context, done <-chan struct{}) {
	t.Helper()
	if err := initial.Err(); err != nil {
		t.Fatalf("lifetime cancellation changed initial startup deadline: %v", err)
	}
	if err := f.entry.validateResident(initial); err != nil {
		t.Fatalf("runtime closed initial borrow: %v", err)
	}
	select {
	case <-done:
		t.Fatal("runtime returned before initial preparation completion")
	default:
	}
}

func assertRuntimeInstanceLeaseFree(t *testing.T, root string) {
	t.Helper()
	lease, acquired, err := TryAcquireLease(t.Context(), filepath.Join(root, instanceLeaseName))
	if lease != nil {
		defer func() {
			if closeErr := lease.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
	}
	if err != nil || !acquired {
		t.Fatalf("settled serving role kept the instance lease while waiting: acquired=%t error=%v", acquired, err)
	}
}

func awaitServingResourceRelease(t *testing.T, entry *ownedResidentEntry, exited <-chan struct{}) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		entry.mu.Lock()
		released := entry.endpoint == nil && entry.lease == nil && entry.journal == nil && entry.store == nil
		entry.mu.Unlock()
		if released {
			return
		}
		select {
		case <-exited:
			t.Fatal("resident exited with an outstanding daemon Wait")
		case <-deadline.C:
			t.Fatal("wait-only stage retained serving resources")
		case <-ticker.C:
		}
	}
}

func TestOwnerRuntimeDrainsAcceptedHandlerAndPreservesCleanupFault(t *testing.T) {
	var calls atomic.Int32
	initial := make(chan context.Context, 1)
	accepted := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(ctx context.Context, _ Operation, _ LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		if calls.Add(1) == 1 {
			initial <- ctx
		} else {
			accepted <- ctx
			<-release
		}
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	fault := errors.New("native connection cleanup fault after actual close")
	f.server.closeConnection = func(connection *net.UnixConn) error { return errors.Join(connection.Close(), fault) }
	cancel, done, join := startRuntimeFixture(t, f)
	t.Cleanup(func() { cancel(); unblock(); join() })
	awaitOwnerServerHost(t, initial)
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, f.request(t, ownerEnsureOwn)); err != nil {
		t.Fatal(err)
	}
	operation := awaitOwnerServerHost(t, accepted)
	cancel()
	if err := operation.Err(); err != nil {
		t.Fatalf("runtime canceled accepted handler: %v", err)
	}
	if err := f.entry.validateResident(t.Context()); err != nil {
		t.Fatalf("runtime closed handler borrow: %v", err)
	}
	select {
	case <-done:
		t.Fatal("runtime returned before accepted handler completion")
	default:
	}
	unblock()
	if reply := readServerReply(t, connection); reply.Code != ownerOK {
		t.Fatalf("accepted reply lost: %s", reply.Code)
	}
	join()
	if f.entry.closeErr != nil || f.entry.store != nil {
		t.Fatal("cleanup diagnostic prevented actual entry closure")
	}
	if !errors.Is(f.expectedCleanupFault, fault) {
		t.Fatal("runtime erased retained connection cleanup fault")
	}
}

func startRuntimeFixture(t *testing.T, f *ownerServerFixture) (context.CancelFunc, <-chan struct{}, func()) {
	return startRuntimeFixtureExpecting(t, f, context.Canceled)
}

func startRuntimeFixtureExpecting(t *testing.T, f *ownerServerFixture, expected error) (context.CancelFunc, <-chan struct{}, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var result error
	go func() { result = runResidentServer(ctx, f.server); close(done) }()
	var once sync.Once
	join := func() {
		once.Do(func() {
			select {
			case <-done:
				if !errors.Is(result, expected) {
					t.Errorf("runtime lost expected error %v: %v", expected, result)
				}
				f.expectedCleanupFault = result
				// Runtime already drained and closed this listener; its own cleanup
				// must not ask the fixture to wake that disposed native descriptor.
				f.server = nil
			case <-time.After(12 * time.Second):
				f.retain = true
				t.Error("runtime completion owners did not join")
			}
		})
	}
	return cancel, done, join
}

func TestOwnerRuntimeRetainsActualEntryOnIncompleteNativeCleanup(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if err := f.server.stopAcceptAndDrain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.server = nil // Already drained; endpoint cleanup can now close its listener.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cause := errors.New("preparation failed before disposal")
	err := closeResidentEntry(ctx, f.entry, cause)
	var retained *residentEntryCleanupError
	if !errors.As(err, &retained) || retained.entry != f.entry || !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("native cleanup lost actual partial entry: %v", err)
	}
	if retained.entry.endpoint == nil || retained.entry.lease == nil || retained.entry.journal == nil || retained.entry.store == nil {
		t.Fatal("incomplete endpoint cleanup released its native dependencies")
	}
	if _, pinErr := retained.entry.store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("retained Store was closed: %v", pinErr)
	}
	// Stop admission was already drained above. Dispose the retryable canceled
	// endpoint cleanup through its canonical owner before fixture cleanup runs.
	if cleanupErr := closeResidentEntry(t.Context(), retained.entry, nil); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	f.server = nil
}
