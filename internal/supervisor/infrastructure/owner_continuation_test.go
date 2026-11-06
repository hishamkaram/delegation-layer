package infrastructure

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerContinuationRequiresTaskMetadataSubjectBeforeDialAndHost(t *testing.T) {
	for _, kind := range []SavedBindingKind{SavedBindingTaskSubmission, SavedBindingTaskStop, SavedBindingWorkspaceRequest} {
		t.Run(string(kind), func(t *testing.T) {
			exerciseOwnerContinuationSubject(t, kind)
		})
	}
}

func exerciseOwnerContinuationSubject(t *testing.T, kind SavedBindingKind) {
	t.Helper()
	source, _ := savedBindingSourceFixture(t, kind)
	f := newOwnerContactFixture(t)
	source.RootID = f.auth.request.RootID
	selected := InitialOperation{Kind: InitialPrepareContinuation, Source: source}
	if err := selected.Validate(source.RootID); !errors.Is(err, ErrIdentity) {
		t.Fatalf("non-metadata initial subject: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{Operation: selected})
	if !errors.Is(err, ErrIdentity) {
		t.Fatalf("non-metadata bound subject: %v", err)
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
	var calls atomic.Int32
	server := newOwnerServerFixture(t, &ownerServerTestHost{continuation: func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	request := server.request(t, ownerPrepareContinuation)
	request.Source = source
	request.Source.RootID = request.RootID
	if err = request.validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("wire admitted non-metadata: %v", err)
	}
	server.start()
	connection := server.dial(t)
	// Raw encoding exercises the server's untrusted-frame validation too.
	frame, encodeErr := json.Marshal(request)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if _, err = connection.Write(append(frame, '\n')); err != nil {
		t.Fatal(err)
	}
	if _, err = readOwnerFrame[ownerReply](bufio.NewReader(connection)); !errors.Is(err, io.EOF) {
		t.Fatalf("invalid subject was not frame-refused: %v", err)
	}
	if calls.Load() != 0 || server.server.diagnostic() == nil {
		t.Fatalf("invalid source reached host or escaped diagnostics: calls=%d", calls.Load())
	}
}
