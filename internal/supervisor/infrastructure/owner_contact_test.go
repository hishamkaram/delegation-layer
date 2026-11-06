package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Same-process native socket/store composition proves contact fencing, not an
// independently born resident, process ancestry, daemon readiness or election.
type ownerContactFixture struct {
	auth    *ownerAuthFixture
	journal *intentJournal
	lease   *Lease
	reader  *taskdir.ReadOnlyStore
}

func newOwnerContactFixture(t *testing.T) ownerContactFixture {
	t.Helper()
	return ownerContactFixtureWithPeer(t, false)
}

func ownerContactFixtureWithPeer(t *testing.T, wrongPeer bool) ownerContactFixture {
	t.Helper()
	return ownerContactFixtureWithSelection(t, wrongPeer, "")
}

func ownerContactFixtureWithSelection(t *testing.T, wrongPeer bool, initial InitialOperationKind) ownerContactFixture {
	t.Helper()
	auth := newOwnerAuthFixture(t)
	if err := auth.client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := auth.server.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := openIntentJournal(t.Context(), auth.store, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, acquired, err := journal.tryAcquire(t.Context())
	if err != nil || !acquired {
		t.Fatalf("lease: %t %v", acquired, err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	reference, _, err := journal.stageContext(t.Context(), contextForJournal(t, journal))
	if err != nil {
		t.Fatal(err)
	}
	auth.intent.Inputs.Context = reference
	auth.intent.Inputs.Initial = ownerInitialOperationFixture(journal.pin.RootID(), initial)
	if auth.intent.Inputs.Initial != (InitialOperation{}) {
		auth.intent.Protocol = RecordProtocolVersion
	}
	if wrongPeer {
		auth.assumption.Process.PID++
	}
	fixture := ownerContactFixture{auth: auth, journal: journal, lease: lease, reader: contextPassiveReader(t, journal)}
	recordOwnerContactFixture(t, fixture, wrongPeer)
	return fixture
}

func recordOwnerContactFixture(t *testing.T, f ownerContactFixture, wrongPeer bool) {
	t.Helper()
	auth, journal, lease := f.auth, f.journal, f.lease
	auth.rebind(t)
	requireJournalPut(t, journal, lease, auth.intent)
	assumed, err := canonicalStartupTime(auth.assumption.AssumedAt)
	if err != nil {
		t.Fatal(err)
	}
	if wrongPeer {
		writeOwnerEvidenceFixture(t, journal, assumptionName(auth.intent.Sequence), auth.assumption)
	} else if _, err = journal.putAssumptionWithClock(t.Context(), lease, auth.assumption, func() time.Time { return assumed }); err != nil {
		t.Fatal(err)
	}
	if _, err = journal.putPublication(t.Context(), lease, auth.publication); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(auth.publication.Endpoint.Path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f ownerContactFixture) reply() ownerReply {
	return ownerReply{Protocol: ownerProtocolVersion, RootID: f.auth.request.RootID, OwnerRef: f.auth.request.Owner, PublicationSHA256: f.auth.request.PublicationSHA256, Action: ownerStatus, Code: ownerOK}
}

// The test creator joins the one bounded server turn before fixture cleanup.
func (f ownerContactFixture) serve(t *testing.T, handle func(*net.UnixConn, ownerRequest) error) func() error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		connection, err := f.auth.listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		if err = connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			done <- errors.Join(err, connection.Close())
			return
		}
		request, err := readOwnerFrame[ownerRequest](bufio.NewReader(connection))
		if err == nil {
			err = handle(connection, request)
		}
		done <- errors.Join(err, connection.Close())
	}()
	joined := false
	var result error
	join := func() error {
		if !joined {
			result = <-done
			joined = true
		}
		return result
	}
	t.Cleanup(func() {
		if err := join(); err != nil {
			t.Errorf("contact fixture server: %v", err)
		}
	})
	return join
}

func TestOwnerContactAcceptsLiveOwnerAfterOriginalDeadline(t *testing.T) {
	f := newOwnerContactFixture(t)
	expired, err := f.auth.intent.Expired(time.Now())
	if err != nil || !expired {
		t.Fatalf("fixture startup deadline must be past: %v", err)
	}
	join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		if request != f.auth.request {
			return ErrIdentity
		}
		return writeOwnerFrame(connection, f.reply())
	})
	reply, err := contactOwner(t.Context(), f.reader, f.journal.pin, f.auth.request)
	if err != nil || reply != f.reply() {
		t.Fatalf("live owner contact failed: %v", err)
	}
	if err := join(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerContactRejectsBeforeSending(t *testing.T) {
	for _, changed := range []string{"reference", "publication", "client", "root", "invalid", "socket-mode", "socket-identity", "expired", "canceled", "nil-context"} {
		t.Run(changed, func(t *testing.T) {
			exerciseOwnerContactRefusal(t, changed)
		})
	}
}

func exerciseOwnerContactRefusal(t *testing.T, changed string) {
	t.Helper()
	f := newOwnerContactFixture(t)
	ctx, request := refusedOwnerContactRequest(t, f, changed)
	reply, err := contactOwner(ctx, f.reader, f.journal.pin, request)
	expectsIdentity := changed != "expired" && changed != "canceled" && changed != "nil-context"
	if err == nil || reply != (ownerReply{}) || errors.Is(err, ErrIdentity) != expectsIdentity || strings.Contains(err.Error(), f.auth.root) {
		t.Fatalf("unsafe contact: zero=%t err=%v", reply == (ownerReply{}), err)
	}
	if changed == "expired" && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expiry cause lost")
	}
	if changed == "canceled" && !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation cause lost")
	}
	if changed == "nil-context" && !errors.Is(err, os.ErrInvalid) {
		t.Fatal("invalid context cause lost")
	}
	requireOwnerContactError(t, err)
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func requireNoOwnerContactConnection(t *testing.T, listener *net.UnixListener) {
	t.Helper()
	if err := listener.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	connection, acceptErr := listener.AcceptUnix()
	if acceptErr == nil {
		t.Error("refused contact dialed socket")
		if closeErr := connection.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		return
	}
	var timeout net.Error
	if !errors.As(acceptErr, &timeout) || !timeout.Timeout() {
		t.Fatalf("inspect pending contact connection: %v", acceptErr)
	}
}

func refusedOwnerContactRequest(t *testing.T, f ownerContactFixture, changed string) (context.Context, ownerRequest) {
	t.Helper()
	request := f.auth.request
	ctx := t.Context()
	switch changed {
	case "invalid":
		request.Action = "invalid"
	case "reference":
		request.Owner.Sequence++
	case "publication":
		request.PublicationSHA256 = strings.Repeat("f", 64)
	case "client":
		request.Client.StartToken = "fixture:spoofed"
	case "root":
		request.RootID = strings.Repeat("e", 32)
	case "socket-mode":
		if err := os.Chmod(f.auth.publication.Endpoint.Path, 0o660); err != nil {
			t.Fatal(err)
		}
	case "socket-identity":
		replaceOwnerContactSocket(t, f.auth.publication.Endpoint.Path)
	case "expired":
		request.Deadline = time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	case "canceled":
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	case "nil-context":
		ctx = nil // Deliberately exercise the invalid context boundary.
	default:
		t.Fatal("unknown fixture")
	}
	return ctx, request
}

func TestOwnerContactRejectsReplySubjectAndCurrentChanges(t *testing.T) {
	for _, changed := range []string{"reply-root", "reply-epoch", "latest-epoch", "root-pin", "socket"} {
		t.Run(changed, func(t *testing.T) {
			f := newOwnerContactFixture(t)
			join := f.serve(t, func(connection *net.UnixConn, _ ownerRequest) error {
				return f.replyAfterChange(t, connection, changed)
			})
			reply, err := contactOwner(t.Context(), f.reader, f.journal.pin, f.auth.request)
			if err == nil || reply != (ownerReply{}) || strings.Contains(err.Error(), f.auth.root) {
				t.Fatalf("stale reply accepted: %v", err)
			}
			requireOwnerContactError(t, err)
			if err := join(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func (f ownerContactFixture) replyAfterChange(t *testing.T, connection *net.UnixConn, changed string) error {
	reply := f.reply()
	switch changed {
	case "reply-root":
		reply.RootID = strings.Repeat("e", 32)
	case "reply-epoch":
		reply.OwnerRef.Epoch = strings.Repeat("e", 32)
	case "latest-epoch":
		next := f.auth.intent
		next.Sequence++
		next.Epoch = strings.Repeat("e", 32)
		if _, err := f.journal.putIntent(t.Context(), f.lease, next); err != nil {
			return err
		}
	case "root-pin":
		return f.replyDuringRootReplacement(connection, reply)
	case "socket":
		replaceOwnerContactSocket(t, f.auth.publication.Endpoint.Path)
	default:
		return ErrIdentity
	}
	return writeOwnerFrame(connection, reply)
}

func (f ownerContactFixture) replyDuringRootReplacement(connection *net.UnixConn, reply ownerReply) (resultErr error) {
	moved := f.auth.root + "-moved"
	if err := os.Rename(f.auth.root, moved); err != nil {
		return err
	}
	replaced := false
	defer func() {
		if replaced {
			resultErr = errors.Join(resultErr, os.Remove(f.auth.root))
		}
		resultErr = errors.Join(resultErr, os.Rename(moved, f.auth.root))
	}()
	if err := os.Mkdir(f.auth.root, 0o700); err != nil {
		return err
	}
	replaced = true
	if err := writeOwnerFrame(connection, reply); err != nil {
		return err
	}
	return awaitOwnerContactEOF(connection)
}

func awaitOwnerContactEOF(connection *net.UnixConn) error {
	var data [1]byte
	n, err := connection.Read(data[:])
	if n != 0 || !errors.Is(err, io.EOF) {
		return errors.Join(ErrIdentity, err)
	}
	return nil
}

func replaceOwnerContactSocket(t *testing.T, path string) {
	t.Helper()
	if err := os.Rename(path, path+"-saved"); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerContactAuthenticatesPeerBeforeSending(t *testing.T) {
	f := ownerContactFixtureWithPeer(t, true)
	observed := make(chan error, 1)
	go func() {
		connection, err := f.auth.listener.AcceptUnix()
		if err != nil {
			observed <- err
			return
		}
		if err = connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			observed <- errors.Join(err, connection.Close())
			return
		}
		var data [1]byte
		n, readErr := connection.Read(data[:])
		if n != 0 {
			readErr = errors.New("unauthenticated peer received request bytes")
		}
		if n == 0 && errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		observed <- errors.Join(readErr, connection.Close())
	}()
	joined := false
	join := func() {
		if !joined {
			if err := <-observed; err != nil {
				t.Error(err)
			}
			joined = true
		}
	}
	t.Cleanup(join)
	reply, err := contactOwner(t.Context(), f.reader, f.journal.pin, f.auth.request)
	if !errors.Is(err, ErrIdentity) || reply != (ownerReply{}) {
		t.Fatalf("wrong native peer accepted: %v", err)
	}
	requireOwnerContactError(t, err)
	join()
}

func TestOwnerContactCancellationWakesAndJoinsSocket(t *testing.T) {
	for _, wireDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller cancellation", true: "wire deadline"}[wireDeadline], func(t *testing.T) { exerciseOwnerContactCancellation(t, wireDeadline) })
	}
}

func exerciseOwnerContactCancellation(t *testing.T, wireDeadline bool) {
	t.Helper()
	f := newOwnerContactFixture(t)
	request := f.auth.request
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if wireDeadline {
		request.Deadline = time.Now().UTC().Add(2 * time.Second).Format(time.RFC3339Nano)
	}
	join := f.serve(t, func(connection *net.UnixConn, _ ownerRequest) error {
		if !wireDeadline {
			cancel()
		}
		return awaitOwnerContactEOF(connection)
	})
	reply, err := contactOwner(ctx, f.reader, f.journal.pin, request)
	expected := context.Canceled
	if wireDeadline {
		expected = context.DeadlineExceeded
	}
	if !errors.Is(err, expected) || errors.Is(err, ErrIdentity) || reply != (ownerReply{}) {
		t.Fatalf("bounded contact: zero=%t err=%v", reply == (ownerReply{}), err)
	}
	requireOwnerContactError(t, err)
	if joinErr := join(); joinErr != nil {
		t.Fatal(joinErr)
	}
}

func TestOwnerContactTransportEOFDoesNotClaimIdentity(t *testing.T) {
	f := newOwnerContactFixture(t)
	join := f.serve(t, func(*net.UnixConn, ownerRequest) error { return nil })
	reply, err := contactOwner(t.Context(), f.reader, f.journal.pin, f.auth.request)
	if !errors.Is(err, io.EOF) || errors.Is(err, ErrIdentity) || reply != (ownerReply{}) {
		t.Fatalf("transport EOF misclassified: zero=%t err=%v", reply == (ownerReply{}), err)
	}
	requireOwnerContactError(t, err)
	if err := join(); err != nil {
		t.Fatal(err)
	}
}

func requireOwnerContactError(t *testing.T, err error) {
	t.Helper()
	var contactErr ownerContactError
	if !errors.As(err, &contactErr) || err.Error() != "infrastructure owner contact failed" {
		t.Fatalf("contact error was not redacted: %v", err)
	}
}

func TestOwnerContactVerifiedCompletionSurvivesLateContext(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) { checkOwnerContactLateContext(t, deadline) })
	}
}

func checkOwnerContactLateContext(t *testing.T, deadline bool) {
	t.Helper()
	// The completion policy is tested after verification, without a native timing
	// race or a production I/O hook. Native exchange tests prove reply verification.
	verified := ownerReplyFixture()
	ctx, cancel := context.WithCancel(t.Context())
	if deadline {
		cancel()
		ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	}
	defer cancel()
	if !deadline {
		cancel()
	}
	lateErr := ownerContactContextError(ctx)
	if lateErr == nil {
		t.Fatal("late context observation missing")
	}
	reply, err := completeOwnerContact(true, verified, nil, lateErr, nil)
	if err != nil || reply != verified {
		t.Fatalf("late context erased verified reply: %v", err)
	}
}

func TestOwnerContactVerifiedReplySurvivesCleanupFaults(t *testing.T) {
	verified := ownerReplyFixture()
	wakeFault := errors.New("untrusted wake failure")
	closeFault := errors.New("untrusted close failure")
	reply, err := completeOwnerContact(true, verified, nil, context.Canceled, errors.Join(wakeFault, closeFault))
	if reply != verified || !errors.Is(err, wakeFault) || !errors.Is(err, closeFault) || errors.Is(err, context.Canceled) || errors.Is(err, ErrIdentity) {
		t.Fatalf("verified cleanup result lost: %v", err)
	}
	requireOwnerContactError(t, err)
	if strings.Contains(err.Error(), "untrusted") {
		t.Fatal("cleanup diagnostic was not redacted")
	}
}

func TestOwnerContactFailedExchangeKeepsUnknownOutcomeAndCauses(t *testing.T) {
	cleanupFault := errors.New("untrusted cleanup failure")
	for _, exchangeErr := range []error{io.ErrShortWrite, io.EOF} {
		reply, err := completeOwnerContact(false, ownerReplyFixture(), exchangeErr, context.DeadlineExceeded, cleanupFault)
		if reply != (ownerReply{}) || !errors.Is(err, exchangeErr) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cleanupFault) || errors.Is(err, ErrIdentity) {
			t.Fatalf("failed exchange causes lost: %v", err)
		}
		requireOwnerContactError(t, err)
	}
}
