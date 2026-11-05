package infrastructure

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerServerInitialPreparationKeepsOriginalDeadline(t *testing.T) {
	observed := make(chan time.Time, 1)
	host := &ownerServerTestHost{ensure: func(ctx context.Context, _ Operation, _ LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		deadline, finite := ctx.Deadline()
		if !finite {
			t.Error("initial preparation has no finite deadline")
		}
		observed <- deadline
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}
	f := newOwnerServerFixture(t, host)
	disposition, err := f.server.prepareInitial(t.Context())
	if err != nil || disposition.Kind != DispositionWarmReuse {
		t.Fatalf("initial preparation: %+v %v", disposition, err)
	}
	expected, err := canonicalStartupTime(f.entry.operation.Intent().Deadline)
	if err != nil {
		t.Fatal(err)
	}
	if deadline := <-observed; !deadline.Equal(expected) {
		t.Fatalf("initial deadline renewed: %v != %v", deadline, expected)
	}
	if _, err = f.server.prepareInitial(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("initial preparation ran twice: %v", err)
	}
}

func TestOwnerServerRejectsSpoofedNativeClientAndPublication(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	f.start()
	for _, spoof := range []string{"client", "publication"} {
		request := f.request(t, ownerEnsureOwn)
		if spoof == "client" {
			request.Client.PID++
		} else {
			request.PublicationSHA256 = strings.Repeat("f", 64)
		}
		connection := f.dial(t)
		if err := writeOwnerFrame(connection, request); err != nil {
			t.Fatal(err)
		}
		if reply := readServerReply(t, connection); reply.Code != ownerRefused {
			t.Fatalf("spoof %s accepted: %s", spoof, reply.Code)
		}
	}
	if err := f.entry.validateResident(t.Context()); err != nil {
		t.Fatalf("refusal closed entry: %v", err)
	}
}

func TestOwnerServerPreservesVerifiedStartedDispositionOnHostFault(t *testing.T) {
	host := &ownerServerTestHost{ensure: func(_ context.Context, _ Operation, selected LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		digest, err := selected.Digest()
		if err != nil {
			return Disposition{}, err
		}
		// Protocol component value only; this fake is not native Start proof.
		return Disposition{Kind: DispositionStarted, Context: LaunchContextReference{SHA256: digest}}, os.ErrClosed
	}}
	f := newOwnerServerFixture(t, host)
	f.start()
	request := f.request(t, ownerEnsureOwn)
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	reply := readServerReply(t, connection)
	if reply.Code != ownerFault || reply.Disposition.Kind != DispositionStarted || reply.Disposition.Context != f.entry.operation.Intent().Inputs.Context {
		t.Fatalf("host fault erased known outcome: %+v", reply)
	}
}

func TestOwnerServerOperationCapacityReservesStatusAndReturnsExactBusy(t *testing.T) {
	entered := make(chan context.Context, ownerOperationLimit)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	host := &ownerServerTestHost{ensure: func(ctx context.Context, _ Operation, _ LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		entered <- ctx
		<-release
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}
	f := newOwnerServerFixture(t, host)
	t.Cleanup(unblock)
	f.start()
	connections := make([]*net.UnixConn, 0, ownerOperationLimit)
	for range ownerOperationLimit {
		connection := f.dial(t)
		if err := writeOwnerFrame(connection, f.request(t, ownerEnsureOwn)); err != nil {
			t.Fatal(err)
		}
		awaitOwnerServerHost(t, entered)
		connections = append(connections, connection)
	}
	status := f.dial(t)
	statusRequest := f.request(t, ownerStatus)
	if err := writeOwnerFrame(status, statusRequest); err != nil {
		t.Fatal(err)
	}
	if reply := readServerReply(t, status); reply.Code != ownerOK {
		t.Fatalf("long operations starved Status: %s", reply.Code)
	}
	excess := f.dial(t)
	busyRequest := f.request(t, ownerEnsureOwn)
	if err := writeOwnerFrame(excess, busyRequest); err != nil {
		t.Fatal(err)
	}
	busy := readServerReply(t, excess)
	if busy.Code != ownerBusy {
		t.Fatalf("operation cap did not give known Busy: %+v", busy)
	}
	if diagnostic := f.server.diagnostic(); diagnostic != nil {
		t.Fatalf("clean capacity refusal occupied diagnostic slot: %v", diagnostic)
	}
	intent, assumption, publication, _, err := f.server.readResident(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = validateOwnerReplyBinding(intent, assumption, publication, busyRequest, busy); err != nil {
		t.Fatalf("Busy changed subject: %v", err)
	}
	select {
	case <-entered:
		t.Fatal("over-cap operation invoked host")
	default:
	}
	unblock()
	for _, connection := range connections {
		if reply := readServerReply(t, connection); reply.Code != ownerOK {
			t.Fatalf("accepted capacity work refused: %s", reply.Code)
		}
	}
}

func TestOwnerServerIncompleteNativeFrameHasFiniteDeadline(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	f.start()
	connection := f.dial(t)
	started := time.Now()
	var data [1]byte
	if _, err := connection.Read(data[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("incomplete frame not closed within socket watchdog: %v", err)
	}
	if time.Since(started) < ownerFrameReadTimeout {
		t.Fatal("frame budget expired before its fixed duration")
	}
	if err := f.entry.validateResident(t.Context()); err != nil {
		t.Fatalf("frame timeout closed resident: %v", err)
	}
}

func TestOwnerServerAcceptFaultRetainsAcceptedWorkAndEntry(t *testing.T) {
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(ctx context.Context, _ Operation, _ LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		entered <- ctx
		<-release
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	t.Cleanup(unblock)
	f.start()
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, f.request(t, ownerEnsureOwn)); err != nil {
		t.Fatal(err)
	}
	accepted := awaitOwnerServerHost(t, entered)
	if err := f.server.listener.SetDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-f.served:
		if err == nil {
			t.Fatal("unexpected accept fault was hidden")
		}
		f.served = nil
	case <-time.After(10 * time.Second):
		t.Fatal("accept loop did not return")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.server.stopAcceptAndDrain(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("active handler was treated as drained: %v", err)
	}
	if err := accepted.Err(); err != nil {
		t.Fatalf("accept failure canceled accepted operation: %v", err)
	}
	if err := f.entry.validateResident(t.Context()); err != nil {
		t.Fatalf("accept failure closed native owner: %v", err)
	}
	unblock()
	if reply := readServerReply(t, connection); reply.Code != ownerOK {
		t.Fatalf("accepted work lost after accept fault: %s", reply.Code)
	}
}

func TestOwnerServerClosingRejectsAfterNativeValidationBeforeAdmission(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	request := f.request(t, ownerEnsureOwn)
	client := f.dial(t)
	if err := f.server.listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	connection, err := f.server.listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.server.finishConnection(connection) })
	if !f.server.registerConnection(connection) {
		t.Fatal("fixture connection registration refused")
	}
	f.server.stopAccepting()
	if _, err = f.server.acceptWork(t.Context(), connection, request); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closing admitted validated native request: %v", err)
	}
	select {
	case <-f.server.drained:
		t.Fatal("drain ignored registered handler")
	default:
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerServerRejectsMissingSelectedContext(t *testing.T) {
	var calls atomic.Int32
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	f.start()
	request := f.request(t, ownerEnsureOwn)
	request.Context = LaunchContextReference{SHA256: strings.Repeat("f", 64)}
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	if reply := readServerReply(t, connection); reply.Code != ownerRefused || calls.Load() != 0 {
		t.Fatalf("missing context dispatched: code=%s calls=%d", reply.Code, calls.Load())
	}
}
