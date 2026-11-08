package remotecommands

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
)

func TestRemoteCacheReadFailureIsOnlyDiagnostic(t *testing.T) {
	reference, store := newTestReference(t)
	journal, err := store.OpenRemoteReferenceContext(t.Context(), strings.Repeat("a", 32), false)
	if err != nil {
		t.Fatal(err)
	}
	_, putErr := journal.PutContext(t.Context(), "observation-00000000000000000001.json", map[string]string{"corrupt": "cache"})
	if closeErr := journal.Close(); putErr != nil || closeErr != nil {
		t.Fatalf("seed cache: %v %v", putErr, closeErr)
	}
	report := Report{}
	readRemoteCacheReport(t.Context(), reference, &report)
	if len(report.Diagnostics) != 1 || report.Diagnostics[0] != "observation_cache_unavailable" {
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
	reference, _ := newTestReference(t)
	intent, err := reference.Intent()
	if err != nil {
		t.Fatal(err)
	}
	client := Client{Envelopes: pendingEnvelope{}}
	exchange := &expiringRemoteObservation{}
	response, err := client.observeRemoteTask(t.Context(), originRequest{Command: "collect", Watch: 250 * time.Millisecond}, remote.Coordinator{Exchange: exchange}, reference)
	if err != nil {
		t.Fatalf("watch discarded validated pending observation: %v", err)
	}
	assertPendingWatchObservation(t, response, intent.Destination)
	// A validated response and its durable cache write can consume the watch
	// budget before the next poll. Poll count is not part of the watch contract.
	var expirations, cacheFailures int
	for _, diagnostic := range response.Diagnostics {
		switch diagnostic {
		case "observation_watch_expired":
			expirations++
		case "observation_cache_write_failed":
			cacheFailures++
		default:
			t.Fatalf("unexpected observation diagnostic %q", diagnostic)
		}
	}
	if expirations != 1 || cacheFailures > 1 {
		t.Fatalf("diagnostics=%v", response.Diagnostics)
	}
	cached, cacheErr := reference.LastObservation(t.Context())
	if errors.Is(cacheErr, os.ErrNotExist) && cacheFailures == 1 {
		return
	}
	if cacheErr != nil {
		t.Fatalf("read last observation: %v", cacheErr)
	}
	assertPendingWatchObservation(t, cached.Response, intent.Destination)
}

func assertPendingWatchObservation(t *testing.T, response remote.Response, identity remote.Identity) {
	t.Helper()
	const pendingEnvelopeJSON = `{"pending":{"kind":"publication"},"publication":"pending"}`
	if response.Version != remote.ProtocolVersion || response.Identity != identity || string(response.Envelope) != pendingEnvelopeJSON || response.Failure != nil {
		t.Fatalf("watch changed validated pending observation: %+v", response)
	}
}

func TestRemoteWatchWithoutObservationPreservesFailure(t *testing.T) {
	_, err := finishRemoteWatch(remote.Response{}, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing observation hid transport failure: %v", err)
	}
}

// Watch tests supply decoded facts; the app adapter separately proves full-envelope decoding.
type pendingEnvelope struct{}

func (pendingEnvelope) Inspect(json.RawMessage) (ObservationFacts, error) {
	return ObservationFacts{Pending: true}, nil
}

func TestWatchFactsStopAtTerminalOrInteractiveEvidence(t *testing.T) {
	for _, test := range []struct {
		name, command string
		facts         ObservationFacts
		want          bool
	}{
		{"pending", "collect", ObservationFacts{Pending: true}, true},
		{"error", "collect", ObservationFacts{Pending: true, Error: true}, false},
		{"outcome", "collect", ObservationFacts{Pending: true, Outcome: true}, false},
		{"timeout", "collect", ObservationFacts{Pending: true, TimedOut: true}, false},
		{"interrupted", "collect", ObservationFacts{Pending: true, Interrupted: true}, false},
		{"closed_interaction", "collect", ObservationFacts{Pending: true, InteractionClosed: true}, false},
		{"pending_interaction", "collect", ObservationFacts{Pending: true, InteractionPending: true}, false},
		{"events", "events", ObservationFacts{Pending: true, Events: true}, false},
		{"events_during_collection", "collect", ObservationFacts{Pending: true, Events: true}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := observationPending(test.command, test.facts); got != test.want {
				t.Fatalf("watch pending=%t want=%t", got, test.want)
			}
		})
	}
}

func TestControlParsingFailureSettlesAtSubmission(t *testing.T) {
	failure := errors.New("invalid local control file")
	_, err := (Client{}).runRemoteTaskOperation(t.Context(), originRequest{Command: "reply", controlError: failure}, remote.Coordinator{}, nil)
	if !errors.Is(err, failure) {
		t.Fatalf("control parsing error lost: %v", err)
	}
}
