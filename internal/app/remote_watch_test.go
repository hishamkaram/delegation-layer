package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
)

func TestRemoteCacheReadFailureIsOnlyDiagnostic(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	store := newBareInspectionStore(t)
	reference, err := remote.OpenReference(t.Context(), store, fixture.intent.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reference.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	journal, err := store.OpenRemoteReferenceContext(t.Context(), fixture.intent.TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	_, putErr := journal.PutContext(t.Context(), "observation-00000000000000000001.json", map[string]string{"corrupt": "cache"})
	if closeErr := journal.Close(); putErr != nil || closeErr != nil {
		t.Fatalf("seed cache: %v %v", putErr, closeErr)
	}
	report := remoteCLIResponse{}
	readRemoteCacheReport(t.Context(), reference, &report)
	if len(report.Diagnostics) != 1 || report.Diagnostics[0] != "observation_cache_unavailable" || remoteCLIResultCode("dispatch", report, nil) != 0 {
		t.Fatalf("optional cache changed operation result: %+v", report)
	}
}

type expiringRemoteObservation struct{ calls int }

func (e *expiringRemoteObservation) Call(ctx context.Context, request remote.Request) (remote.Response, error) {
	e.calls++
	if e.calls > 1 {
		<-ctx.Done()
		return remote.Response{}, ctx.Err()
	}
	return remote.Response{Version: remote.ProtocolVersion, Identity: *request.Expected, Envelope: []byte(`{"pending":{"kind":"publication"},"publication":"pending"}`)}, nil
}

func TestRemoteWatchExpiryRetainsValidatedPendingObservation(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	reference, err := openRemoteTestReference(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	exchange := &expiringRemoteObservation{}
	response, err := observeRemoteTask(t.Context(), Arguments{Command: "collect", Watch: 250 * time.Millisecond}, remote.Coordinator{Exchange: exchange}, reference)
	if err != nil || response.Version != remote.ProtocolVersion || !remoteObservationPending("collect", response.Envelope) {
		t.Fatalf("watch discarded pending observation: response=%+v err=%v", response, err)
	}
	if exchange.calls != 2 || len(response.Diagnostics) != 1 || response.Diagnostics[0] != "observation_watch_expired" {
		t.Fatalf("calls=%d diagnostics=%v", exchange.calls, response.Diagnostics)
	}
}

func TestRemoteWatchWithoutObservationPreservesFailure(t *testing.T) {
	_, err := finishRemoteWatch(remote.Response{}, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing observation hid transport failure: %v", err)
	}
}
