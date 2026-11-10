package remotecommands

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/remote"
)

type payloadReadExchange struct {
	data             []byte
	descriptor       remote.Descriptor
	calls            int
	changeDescriptor bool
}

type stalledPayloadExchange struct {
	release <-chan struct{}
	entered chan<- time.Time
}

func (e stalledPayloadExchange) Call(ctx context.Context, _ remote.Request) (remote.Response, error) {
	deadline, _ := ctx.Deadline()
	e.entered <- deadline
	select {
	case <-ctx.Done():
		return remote.Response{}, ctx.Err()
	case <-e.release:
		return remote.Response{}, context.Canceled
	}
}

func TestPayloadStreamPreservesCallerBoundAndCancelsStalledExchange(t *testing.T) {
	reference, _ := newTestReference(t)
	intent, err := reference.Intent()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	caller, stopCaller := context.WithDeadline(t.Context(), deadline)
	defer stopCaller()
	ctx, cancel := context.WithTimeout(caller, remote.DefaultTransferTimeout)
	defer cancel()
	release, done := make(chan struct{}), make(chan struct{})
	entered := make(chan time.Time, 1)
	t.Cleanup(func() {
		close(release)
		<-done
	})
	reader := &payloadReader{
		ctx: ctx, coordinator: remote.Coordinator{Exchange: stalledPayloadExchange{release: release, entered: entered}},
		request: remote.Request{
			Version: remote.ProtocolVersion, Operation: remote.Download, Expected: &intent.Destination,
			Root: intent.Destination.Root, TaskID: intent.TaskID, IntentSHA256: task.ComputeSHA256([]byte("intent")),
			Transfer: &remote.TransferRequest{Selector: "payload"},
		},
		descriptor: remote.Descriptor{Name: "result.txt", Length: 1, SHA256: task.ComputeSHA256([]byte("x"))},
	}
	result := make(chan error, 1)
	go func() {
		defer close(done)
		var buffer [1]byte
		_, readErr := reader.Read(buffer[:])
		result <- readErr
	}()
	select {
	case got := <-entered:
		if !got.Equal(deadline) {
			t.Fatalf("transfer replaced caller deadline: got=%s want=%s", got, deadline)
		}
	case <-time.After(time.Second):
		t.Fatal("payload reader never entered the controlled stalled exchange")
	}
	stopCaller()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stalled transfer lost caller cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled transfer retained its reader after caller cancellation")
	}
}

func (e *payloadReadExchange) Call(_ context.Context, request remote.Request) (remote.Response, error) {
	e.calls++
	start, length := request.Transfer.Offset, request.Transfer.Length
	descriptor := e.descriptor
	if e.changeDescriptor {
		descriptor.SHA256 = task.ComputeSHA256([]byte("changed"))
	}
	return remote.Response{Version: remote.ProtocolVersion, Identity: *request.Expected, Data: append([]byte(nil), e.data[start:start+length]...), Transfer: &remote.TransferReceipt{Availability: "available", Descriptor: descriptor, Length: length, Offset: start + length, Complete: start+length == int64(len(e.data))}}, nil
}

func TestPayloadReaderUsesBoundedExistingDownloadAndRejectsDescriptorDrift(t *testing.T) {
	reference, _ := newTestReference(t)
	intent, err := reference.Intent()
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("z"), remote.MaxDataBytes+3)
	descriptor := remote.Descriptor{Name: "result.txt", Length: int64(len(data)), SHA256: task.ComputeSHA256(data)}
	exchange := &payloadReadExchange{data: data, descriptor: descriptor}
	request := remote.Request{Version: remote.ProtocolVersion, Operation: remote.Download, Expected: &intent.Destination, Root: intent.Destination.Root, TaskID: intent.TaskID, IntentSHA256: task.ComputeSHA256([]byte("intent")), Transfer: &remote.TransferRequest{Selector: "payload"}}
	reader := &payloadReader{ctx: t.Context(), coordinator: remote.Coordinator{Exchange: exchange}, request: request, descriptor: descriptor}
	buffer := make([]byte, len(data))
	n, err := reader.Read(buffer)
	if err != nil || n != remote.MaxDataBytes {
		t.Fatalf("first bounded read=%d, %v", n, err)
	}
	if n, err = reader.Read(buffer); err != nil || n != 3 {
		t.Fatalf("remaining read=%d, %v", n, err)
	}
	if n, err = reader.Read(buffer); n != 0 || !errors.Is(err, io.EOF) || exchange.calls != 2 {
		t.Fatalf("end=%d,%v calls=%d", n, err, exchange.calls)
	}
	exchange.changeDescriptor = true
	reader.offset = 0
	if n, err = reader.Read(buffer); n != 0 || !errors.Is(err, remote.ErrProtocol) {
		t.Fatalf("descriptor drift accepted=%d,%v", n, err)
	}
}

func TestRemoteInputBoundsIncompleteControlFrame(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}()
	bounded, err := newRemoteInput(t.Context(), reader, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, readErr := (remote.Framer{Reader: bounded}).ReadRequest(); result <- readErr }()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 10)
	if _, err = writer.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unbounded or wrong read error: %v", err)
		}
	case <-time.After(time.Second):
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("incomplete control frame exceeded idle bound")
	}
}

func TestRemoteInputBoundsIncompleteDataFrame(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}()
	bounded, err := newRemoteInput(t.Context(), reader, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, readErr := (remote.Framer{Reader: bounded}).ReadData(); result <- readErr }()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], remote.MaxDataBytes)
	if _, err = writer.Write(append(header[:], byte(1))); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wrong data read error: %v", err)
		}
	case <-time.After(time.Second):
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("incomplete data frame exceeded idle bound")
	}
}

func TestRemoteInputHonorsOverallDeadline(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	bounded, err := newRemoteInput(ctx, reader, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	_, err = bounded.Read(data[:])
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overall deadline lost: %v", err)
	}
}

func TestRemoteOutputBoundsUnreadResponse(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	bounded, err := newRemoteOutput(t.Context(), writer, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bounded.Write([]byte("response"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unread response lost idle bound: %v", err)
	}
}

func TestRemoteFinalPinFailureKeepsAdmissionUnknown(t *testing.T) {
	for _, operation := range []remote.Operation{remote.Admit, remote.Reconcile} {
		for _, record := range []string{"created", "not_created"} {
			response := remote.Response{Version: remote.ProtocolVersion, Identity: remote.Identity{Root: "/recorded-root"}, TaskRecord: record, Existing: true, Envelope: []byte(`{"task_record":"created"}`)}
			result := remoteFinalPinFailure(operation, response, remote.ErrIdentity)
			if result.TaskRecord != "unknown" || result.Failure == nil || result.Failure.Code != "acceptance_unknown" || result.Existing || len(result.Envelope) != 0 {
				t.Fatalf("root change became definite failure: %+v", result)
			}
			err := &remote.OperationError{Code: result.Failure.Code, Message: result.Failure.Message}
			if !errors.Is(err, remote.ErrAcceptanceUnknown) {
				t.Fatalf("uncertainty classification lost: %v", err)
			}
		}
	}
}
