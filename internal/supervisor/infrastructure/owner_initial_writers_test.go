package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOwnerInitialFailureDrainsBothAdmittedReplayWriters(t *testing.T) {
	entered := make(chan context.Context, 1)
	initialRelease := make(chan struct{})
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	var initialOnce, firstOnce, secondOnce sync.Once
	releaseInitial := func() { initialOnce.Do(func() { close(initialRelease) }) }
	releaseFirst := func() { firstOnce.Do(func() { close(firstRelease) }) }
	releaseSecond := func() { secondOnce.Do(func() { close(secondRelease) }) }
	releaseAll := func() { releaseInitial(); releaseFirst(); releaseSecond() }
	var calls atomic.Int32
	var cleaned atomic.Bool
	failure := PreparationFailure{Causes: PreparationCausePrivateMigrationRequired, Reason: PreparationReasonObservationFailed, IndependentFault: true}
	host := &ownerServerTestHost{
		recoverSaved: func(ctx context.Context, _ Operation, _ LaunchContext, _ SavedBindingSource, _ ResidentGuards, _ DaemonWaitOwner) (Disposition, error) {
			defer cleaned.Store(true)
			calls.Add(1)
			entered <- ctx
			<-initialRelease
			return Disposition{}, io.ErrUnexpectedEOF
		},
		classify: func(error) PreparationFailure {
			if !cleaned.Load() {
				t.Error("classified before host cleanup returned")
			}
			return failure
		},
	}
	f := newOwnerServerFixtureWithSelection(t, host, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	first, firstServer, firstWork := admitOwnerReplayForTest(t, f, ctx)
	second, secondServer, secondWork := admitOwnerReplayForTest(t, f, ctx)
	writing := make(chan int, 2)
	f.server.writeReply = func(connection *net.UnixConn, reply ownerReply) error {
		if connection == firstServer {
			writing <- 1
			<-firstRelease
		} else {
			writing <- 2
			<-secondRelease
		}
		return writeOwnerFrame(connection, reply)
	}
	_, done, joinRuntime := startOwnerRuntimeTest(t, f)
	if initial := awaitOwnerServerHost(t, entered); initial.Err() != nil {
		t.Fatal(initial.Err())
	}
	s := f.server
	joinFirst := startOwnerResponderForTest(t, f, s, ctx, firstServer, firstWork, releaseAll)
	joinSecond := startOwnerResponderForTest(t, f, s, ctx, secondServer, secondWork, releaseAll)
	releaseInitial()
	for range 2 {
		select {
		case <-writing:
		case <-ctx.Done():
			t.Fatal("admitted initial waiters did not reach their writes")
		}
	}
	releaseFirst()
	assertOwnerFailureReply(t, first, failure)
	joinFirst()
	select {
	case <-done:
		t.Fatal("first delivery discarded another admitted writer")
	default:
	}
	if err := f.entry.validateResident(t.Context()); err != nil {
		t.Fatalf("entry closed while second reply owned it: %v", err)
	}
	releaseSecond()
	assertOwnerFailureReply(t, second, failure)
	if calls.Load() != 1 {
		t.Fatalf("admitted replay reran host: calls=%d", calls.Load())
	}
	joinSecond()
	if err := joinRuntime(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("runtime lost initial error: %v", err)
	}
}

func admitOwnerReplayForTest(t *testing.T, f *ownerServerFixture, ctx context.Context) (*net.UnixConn, *net.UnixConn, acceptedOwnerWork) {
	t.Helper()
	client := f.dial(t)
	if err := f.server.listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	server, err := f.server.listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	owner := f.server
	t.Cleanup(func() { owner.finishConnection(server) })
	if !owner.registerConnection(server) {
		t.Fatal("native replay registration refused")
	}
	request := f.requestContext(t, ctx, ownerRecoverSaved)
	request.Source = f.entry.operation.Intent().Inputs.Initial.Source
	request.InitialResultFor = request.Owner
	if err = writeOwnerFrame(client, request); err != nil {
		t.Fatal(err)
	}
	decoded, err := readOwnerFrame[ownerRequest](bufio.NewReader(server))
	if err != nil {
		t.Fatal(err)
	}
	work, err := owner.acceptWork(ctx, server, decoded)
	if err != nil {
		t.Fatal(err)
	}
	return client, server, work
}

func startOwnerResponderForTest(t *testing.T, f *ownerServerFixture, s *ownerServer, ctx context.Context, connection *net.UnixConn, work acceptedOwnerWork, release func()) func() {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	join := func() {
		once.Do(func() {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				f.retain = true
				t.Error("accepted replay responder did not join")
			}
		})
	}
	t.Cleanup(func() { release(); join() })
	go func() {
		defer close(done)
		defer s.finishConnection(connection)
		s.respondToWork(ctx, connection, work)
	}()
	return join
}

func TestOwnerFailedReplayWriteDoesNotConsumeTerminalDelivery(t *testing.T) {
	var calls, writes atomic.Int32
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{recoverSaved: func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{}, io.ErrUnexpectedEOF
	}}, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	if _, err := f.server.prepareInitial(t.Context()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	f.server.writeReply = func(connection *net.UnixConn, reply ownerReply) error {
		if writes.Add(1) == 1 {
			data, err := task.MarshalCanonical(reply)
			if err != nil {
				return err
			}
			if _, err = connection.Write(data[:len(data)/2]); err != nil {
				return err
			}
			return io.ErrShortWrite
		}
		return writeOwnerFrame(connection, reply)
	}
	f.start()
	owner := f.request(t, ownerStatus).Owner
	request := PreparationRequest{Operation: f.entry.operation.Intent().Inputs.Initial, InitialResultFor: owner}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reader := contextPassiveReader(t, f.entry.journal)
	outcome, err := RequestBoundPreparation(ctx, reader, owner, request)
	if outcome != (PreparationOutcome{}) || writes.Load() != 1 {
		t.Fatalf("failed write was automatically replayed: outcome=%v writes=%d err=%v", outcome, writes.Load(), err)
	}
	requireOwnerContactError(t, err)
	select {
	case <-f.server.initialDelivered:
		t.Fatal("partial frame consumed terminal handoff")
	default:
	}
	// This second request is an explicit non-Own replay decision by this caller.
	outcome, err = RequestBoundPreparation(ctx, reader, owner, request)
	if err != nil || outcome.Condition != PreparationFault || calls.Load() != 1 || writes.Load() != 2 {
		t.Fatalf("explicit replay lost cached failure: outcome=%v calls=%d writes=%d err=%v", outcome, calls.Load(), writes.Load(), err)
	}
	select {
	case <-f.server.initialDelivered:
	default:
		t.Fatal("successful full frame did not consume terminal delivery")
	}
}

func assertOwnerFailureReply(t *testing.T, connection *net.UnixConn, failure PreparationFailure) {
	t.Helper()
	reply, err := readOwnerFrame[ownerReply](bufio.NewReader(connection))
	if err != nil {
		t.Fatalf("admitted full frame was cut: %v", err)
	}
	if reply.Code != ownerFault || reply.Failure != failure {
		t.Fatalf("cached initial failure reply lost: %v", reply)
	}
}
