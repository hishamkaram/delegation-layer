package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestReadOwnerArtifactsRequiresCurrentCanonicalSelection(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	before := stopRecordSnapshot(t, f.reader)
	artifacts, err := ReadOwnerArtifacts(ctx, f.reader, f.auth.request.Owner)
	if err != nil || artifacts != f.auth.intent.Inputs.Artifacts {
		t.Fatalf("immutable owner artifacts changed: %+v %v", artifacts, err)
	}
	stale := f.auth.request.Owner
	stale.Sequence++
	if artifacts, err = ReadOwnerArtifacts(ctx, f.reader, stale); !errors.Is(err, task.ErrLockBusy) || artifacts != (Artifacts{}) {
		t.Fatalf("stale selection exposed another owner's artifacts: %+v %v", artifacts, err)
	}
	if !maps.Equal(before, stopRecordSnapshot(t, f.reader)) {
		t.Fatal("artifact observation changed canonical records")
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func TestReadOwnerArtifactsDoesNotTurnMissingEvidenceIntoArtifacts(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unreadable", true: "missing"}[missing], func(t *testing.T) {
			f := newOwnerContactFixture(t)
			path := filepath.Join(f.reader.Root(), "infrastructure", publicationName(1))
			var err error
			if missing {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte("{}\n"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			artifacts, err := ReadOwnerArtifacts(ctx, f.reader, f.auth.request.Owner)
			if err == nil || artifacts != (Artifacts{}) {
				t.Fatalf("invalid evidence became helper selection: %+v %v", artifacts, err)
			}
			requireNoOwnerContactConnection(t, f.auth.listener)
		})
	}
}

func TestOwnerStopAtBusyPreservesExactOwner(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	join := serveOwnerStopFixture(t, f, func(connection *net.UnixConn) error {
		request, err := readOwnerFrame[ownerStopRequest](bufio.NewReader(connection))
		if err != nil {
			return err
		}
		return writeOwnerFrame(connection, ownerStopFixtureReply(request, ownerStopBusy))
	})
	result, err := StopOwnerServingAt(ctx, f.reader, f.auth.request.Owner)
	if err != nil || result.State != OwnerStopBusy || result.Startup != f.auth.request.Owner || result.Process != f.auth.assumption.Process {
		t.Fatalf("bound busy stop lost original owner: %+v %v", result, err)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
	assertAvailable(t, f.lease.path, false)
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func TestOwnerStopAtRefusesStaleSubjectBeforeContact(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	before := stopRecordSnapshot(t, f.reader)
	stale := f.auth.request.Owner
	stale.Sequence++
	result, err := StopOwnerServingAt(ctx, f.reader, stale)
	if !errors.Is(err, task.ErrLockBusy) || result.State != OwnerStopUnknown || result.Startup != (StartupReference{}) {
		t.Fatalf("stale subject selected the current owner: %+v %v", result, err)
	}
	if !maps.Equal(before, stopRecordSnapshot(t, f.reader)) {
		t.Fatal("stale stop changed canonical records")
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func TestOwnerStopAtDoesNotContactConcurrentSuccessor(t *testing.T) {
	f, original := newOwnerStopReleasedSuccessorFixture(t)
	intent, err := f.journal.readIntent(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := ownerEndpointPath(f.reader.Root(), 2)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	socket, err := observeEndpointSocket(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	assumption := f.auth.assumption
	assumption.Startup = reference
	publication := f.auth.publication
	publication.Startup = reference
	publication.AssumptionSHA256, err = assumption.Digest(intent)
	if err != nil {
		t.Fatal(err)
	}
	publication.Endpoint.Path = endpoint
	publication.Endpoint.SocketDev, publication.Endpoint.SocketInode = uint64(socket.Dev), uint64(socket.Ino)
	writeOwnerEvidenceFixture(t, f.journal, assumptionName(2), assumption)
	writeOwnerEvidenceFixture(t, f.journal, publicationName(2), publication)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	before := stopRecordSnapshot(t, f.reader)
	result, err := StopOwnerServingAt(ctx, f.reader, original.reference)
	if !errors.Is(err, task.ErrLockBusy) || result.State != OwnerStopUnknown || result.Startup != (StartupReference{}) {
		t.Fatalf("stale A selected concurrent O2 for stop: %+v %v", result, err)
	}
	if !maps.Equal(before, stopRecordSnapshot(t, f.reader)) {
		t.Fatal("stale A stop changed O2 records")
	}
	requireNoOwnerContactConnection(t, listener)
}

// These same-process native peers own one finite exchange and join it before
// fixture cleanup. No resident process, daemon, provider or replacement starts.
func serveOwnerStopFixture(t *testing.T, f ownerContactFixture, handle func(*net.UnixConn) error) func() error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		connection, err := f.auth.listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		if err = connection.SetDeadline(time.Now().Add(2 * time.Second)); err == nil {
			err = handle(connection)
		}
		done <- errors.Join(err, connection.Close())
	}()
	joined := false
	var result error
	join := func() error {
		if !joined {
			result, joined = <-done, true
		}
		return result
	}
	t.Cleanup(func() {
		if !joined {
			if err := f.auth.listener.SetDeadline(time.Now()); err != nil {
				t.Error(err)
			}
		}
		if err := join(); err != nil {
			t.Errorf("stop fixture exchange: %v", err)
		}
	})
	return join
}

func ownerStopFixtureReply(request ownerStopRequest, code ownerStopCode) ownerStopReply {
	return ownerStopReply{StopProtocol: ownerStopProtocolVersion, RootID: request.RootID, Owner: request.Owner, PublicationSHA256: request.PublicationSHA256, Code: code}
}

func TestOwnerStopClientBusyPreservesOwnerServing(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	join := serveOwnerStopFixture(t, f, func(connection *net.UnixConn) error {
		request, err := readOwnerFrame[ownerStopRequest](bufio.NewReader(connection))
		if err != nil {
			return err
		}
		if request.StopProtocol != 1 || request.RootID != f.reader.RootID() || request.Owner != f.auth.request.Owner || request.PublicationSHA256 != f.auth.request.PublicationSHA256 {
			return ErrIdentity
		}
		return writeOwnerFrame(connection, ownerStopFixtureReply(request, ownerStopBusy))
	})
	result, err := StopOwnerServing(ctx, f.reader)
	if err != nil || result.State != OwnerStopBusy || result.ProcessClosure != OwnerProcessPending || result.Startup != f.auth.request.Owner {
		t.Fatalf("verified busy stop changed its subject: %+v %v", result, err)
	}
	if result.Process != f.auth.assumption.Process {
		t.Fatalf("busy result changed selected process: %+v", result.Process)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
	assertAvailable(t, f.lease.path, false)
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func TestOwnerStopClientAcknowledgementDoesNotEstablishClosure(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	join := serveOwnerStopFixture(t, f, func(connection *net.UnixConn) error {
		request, err := readOwnerFrame[ownerStopRequest](bufio.NewReader(connection))
		if err != nil {
			return err
		}
		return writeOwnerFrame(connection, ownerStopFixtureReply(request, ownerStopAccepted))
	})
	result, err := StopOwnerServing(ctx, f.reader)
	if !errors.Is(err, context.DeadlineExceeded) || result.State != OwnerStopUnknown || result.ProcessClosure != OwnerProcessPending {
		t.Fatalf("ACK/held lease became closure: %+v %v", result, err)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func TestOwnerStopClientLostReplyDoesNotReplay(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var received atomic.Int32
	join := serveOwnerStopFixture(t, f, func(connection *net.UnixConn) error {
		if _, err := readOwnerFrame[ownerStopRequest](bufio.NewReader(connection)); err != nil {
			return err
		}
		received.Add(1)
		return nil
	})
	result, err := StopOwnerServing(ctx, f.reader)
	if !errors.Is(err, io.EOF) || errors.Is(err, ErrIdentity) || result.State != OwnerStopUnknown || result.ProcessClosure != OwnerProcessPending {
		t.Fatalf("lost reply changed owner state or transport cause: %+v %v", result, err)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 {
		t.Fatalf("received %d stop requests", received.Load())
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func TestOwnerStopClientRequiresExactReplyEcho(t *testing.T) {
	for _, changed := range []string{"root", "owner", "publication"} {
		t.Run(changed, func(t *testing.T) {
			f := newOwnerContactFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			join := serveOwnerStopFixture(t, f, func(connection *net.UnixConn) error {
				request, err := readOwnerFrame[ownerStopRequest](bufio.NewReader(connection))
				if err != nil {
					return err
				}
				reply := ownerStopFixtureReply(request, ownerStopBusy)
				switch changed {
				case "root":
					reply.RootID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				case "owner":
					reply.Owner.Sequence++
				case "publication":
					reply.PublicationSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				}
				return writeOwnerFrame(connection, reply)
			})
			result, err := StopOwnerServing(ctx, f.reader)
			if !errors.Is(err, ErrIdentity) || result.State != OwnerStopUnknown {
				t.Fatalf("foreign reply was accepted: %+v %v", result, err)
			}
			if err = join(); err != nil {
				t.Fatal(err)
			}
			requireNoOwnerContactConnection(t, f.auth.listener)
		})
	}
}

func TestOwnerStopClientAuthenticatesNativePeerBeforeSending(t *testing.T) {
	f := ownerContactFixtureWithPeer(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	join := serveOwnerStopFixture(t, f, func(connection *net.UnixConn) error {
		var first [1]byte
		count, err := connection.Read(first[:])
		if count != 0 || !errors.Is(err, io.EOF) {
			return errors.New("unauthenticated peer received stop bytes")
		}
		return nil
	})
	result, err := StopOwnerServing(ctx, f.reader)
	if !errors.Is(err, ErrIdentity) || result.State != OwnerStopUnknown {
		t.Fatalf("wrong native peer accepted: %+v %v", result, err)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerStopProcessBootRelationPrecedesPIDObservation(t *testing.T) {
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	// An invalid process reference cannot be interpreted before BootDifferent.
	selected := ownerStopLocator{bootID: otherStableBoot(t, boot), assumption: StartupAssumption{Process: ProcessIdentity{PID: -1}}}
	present, err := ownerStopProcessPresent(t.Context(), selected)
	if present || !errors.Is(err, ErrIdentity) {
		t.Fatalf("previous boot reached PID interpretation: present=%t error=%v", present, err)
	}
}

func TestOwnerStopClientRejectsUnboundedOrCanceledObservation(t *testing.T) {
	f := newOwnerContactFixture(t)
	for _, ctx := range []context.Context{nil, context.Background()} {
		if _, err := StopOwnerServing(ctx, f.reader); !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("unbounded stop: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	cancel()
	canceled, stop := context.WithCancel(context.Background())
	stop()
	for _, ctx := range []context.Context{ctx, canceled} {
		if _, err := StopOwnerServing(ctx, f.reader); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled stop: %v", err)
		}
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}
