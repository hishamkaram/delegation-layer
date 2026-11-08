package remotecommands

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
)

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
