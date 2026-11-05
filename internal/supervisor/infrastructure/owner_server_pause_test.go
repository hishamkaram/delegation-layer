package infrastructure

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerServerPauseDrainsOperationsReservesStatusAndResumesSameOwner(t *testing.T) {
	entered := make(chan context.Context, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	host := &ownerServerTestHost{ensure: func(ctx context.Context, _ Operation, _ LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		entered <- ctx
		<-release
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}
	f := newOwnerServerFixture(t, host)
	t.Cleanup(unblock)
	originalEntry, originalWaits := f.entry, f.server.waits
	f.start()
	active := f.dial(t)
	if err := writeOwnerFrame(active, f.request(t, ownerEnsureOwn)); err != nil {
		t.Fatal(err)
	}
	awaitOwnerServerHost(t, entered)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.server.pauseOperationsAndDrain(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("active pause drained: %v", err)
	}
	if err := f.server.resumeOperationsAfterVerifiedAbort(t.Context()); !errors.Is(err, errOwnerWorkBusy) {
		t.Fatalf("undrained pause resumed: %v", err)
	}
	requireOwnerServerRequestCode(t, f, ownerStatus, ownerOK)
	requireOwnerServerRequestCode(t, f, ownerEnsureOwn, ownerBusy)
	if calls.Load() != 1 {
		t.Fatalf("paused request invoked host: %d", calls.Load())
	}
	unblock()
	if reply := readServerReply(t, active); reply.Code != ownerOK {
		t.Fatalf("pause canceled admitted operation: %s", reply.Code)
	}
	ctx, finish := context.WithTimeout(t.Context(), 10*time.Second)
	defer finish()
	if err := f.server.pauseOperationsAndDrain(ctx); err != nil {
		t.Fatal(err)
	}
	// This component exercises the private resume mechanics only. It supplies no
	// durable Aborted proof; the future lifecycle caller must verify that commit.
	if err := f.server.resumeOperationsAfterVerifiedAbort(ctx); err != nil {
		t.Fatal(err)
	}
	if f.server.entry != originalEntry || f.server.waits != originalWaits {
		t.Fatal("resume replaced entry or Wait owner")
	}
	requireOwnerServerRequestCode(t, f, ownerEnsureOwn, ownerOK)
	if calls.Load() != 2 {
		t.Fatalf("resume did not reopen exactly one operation: %d", calls.Load())
	}
	if err := f.server.stopAcceptAndDrain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.server.resumeOperationsAfterVerifiedAbort(ctx); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("permanent stop resumed: %v", err)
	}
}

func requireOwnerServerRequestCode(t *testing.T, f *ownerServerFixture, action ownerAction, expected ownerReplyCode) {
	t.Helper()
	connection := f.dial(t)
	request := f.request(t, action)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	if reply := readServerReply(t, connection); reply.Code != expected {
		t.Fatalf("%s code=%s want=%s", action, reply.Code, expected)
	}
}

func TestOwnerServerPauseIncludesInitialPreparation(t *testing.T) {
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(ctx context.Context, _ Operation, _ LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		entered <- ctx
		<-release
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	completed := make(chan error, 1)
	var joinOnce sync.Once
	join := func() {
		joinOnce.Do(func() {
			select {
			case err := <-completed:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				f.retain = true
				t.Error("initial preparation did not join")
			}
		})
	}
	t.Cleanup(func() { unblock(); join() })
	go func() { _, err := f.server.prepareInitial(t.Context()); completed <- err }()
	awaitOwnerServerHost(t, entered)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.server.pauseOperationsAndDrain(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("initial preparation omitted from pause drain: %v", err)
	}
	if err := f.server.resumeOperationsAfterVerifiedAbort(t.Context()); !errors.Is(err, errOwnerWorkBusy) {
		t.Fatalf("active initial preparation resumed: %v", err)
	}
	unblock()
	join()
	if err := f.server.pauseOperationsAndDrain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.server.resumeOperationsAfterVerifiedAbort(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerServerPauseRejectsConcurrentValidatedAdmission(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	request := f.request(t, ownerEnsureOwn)
	connection := manuallyAcceptOwnerServerConnection(t, f)
	if !f.server.registerConnection(connection) {
		t.Fatal("fixture pre-auth registration failed")
	}
	f.entry.mu.Lock() // Hold the actual resident-validation boundary.
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(func() { f.entry.mu.Unlock() }) }
	completed := make(chan error, 1)
	var joinOnce sync.Once
	var result error
	join := func() {
		joinOnce.Do(func() {
			select {
			case result = <-completed:
			case <-time.After(10 * time.Second):
				f.retain = true
				t.Error("admission validation did not join")
			}
		})
	}
	t.Cleanup(func() { unlock(); join() })
	attempted := make(chan struct{})
	go func() {
		close(attempted)
		_, err := f.server.acceptWork(t.Context(), connection, request)
		completed <- err
	}()
	<-attempted
	if err := f.server.pauseOperationsAndDrain(t.Context()); err != nil {
		t.Fatal(err)
	}
	unlock()
	join()
	if !errors.Is(result, errOwnerWorkBusy) {
		t.Fatalf("pause lost atomic admission boundary: %v", result)
	}
}
