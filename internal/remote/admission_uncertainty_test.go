package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// This exchange uses the production wire boundary so its send marker precedes
// real request bytes, then the response reader sees a malformed frame.
type malformedAdmissionExchange struct {
	operations []Operation
	unsent     error
}

func (e *malformedAdmissionExchange) Call(_ context.Context, request Request) (Response, error) {
	e.operations = append(e.operations, request.Operation)
	if e.unsent != nil {
		return Response{}, &BeforeSendError{Err: e.unsent}
	}
	frame := Framer{Writer: io.Discard, Reader: bytes.NewReader([]byte{0, 0, 0, 0})}
	if err := frame.WriteRequest(request); err != nil {
		return Response{}, err
	}
	return frame.ReadResponse()
}

func TestMalformedAdmissionResponseRetainsUnknownWithoutProtocolRetry(t *testing.T) {
	reference, _ := testReference(t)
	exchange := &malformedAdmissionExchange{}
	coordinator := Coordinator{Exchange: exchange}
	_, err := coordinator.Dispatch(t.Context(), reference)
	if !errors.Is(err, ErrProtocol) || !errors.Is(err, ErrAcceptanceUnknown) {
		t.Fatalf("malformed acknowledgment lost cause or uncertainty: %v", err)
	}
	if len(exchange.operations) != 1 || exchange.operations[0] != Admit {
		t.Fatalf("nontransient malformed response was retried: %v", exchange.operations)
	}
	exchange.unsent = errors.New("authentication failed before reconnect transmission")
	_, err = coordinator.Dispatch(t.Context(), reference)
	if !errors.Is(err, exchange.unsent) || !errors.Is(err, ErrAcceptanceUnknown) {
		t.Fatalf("failed reconnect erased earlier admission uncertainty: %v", err)
	}
	if len(exchange.operations) != 2 || exchange.operations[1] != Reconcile {
		t.Fatalf("restart readmitted after malformed acknowledgment: %v", exchange.operations)
	}
	_, err = coordinator.Reconcile(t.Context(), reference)
	if !errors.Is(err, exchange.unsent) || !errors.Is(err, ErrAcceptanceUnknown) {
		t.Fatalf("explicit reconciliation erased earlier admission uncertainty: %v", err)
	}
	if len(exchange.operations) != 3 || exchange.operations[2] != Reconcile {
		t.Fatalf("explicit reconciliation changed admission operation: %v", exchange.operations)
	}
}

func TestBeforeSendProtocolFailureDoesNotClaimAdmissionUnknown(t *testing.T) {
	reference, _ := testReference(t)
	exchange := &malformedAdmissionExchange{unsent: ErrProtocol}
	_, err := (Coordinator{Exchange: exchange}).Dispatch(t.Context(), reference)
	if !errors.Is(err, ErrProtocol) || errors.Is(err, ErrAcceptanceUnknown) {
		t.Fatalf("known-unsent protocol failure misclassified: %v", err)
	}
	digest, digestErr := IntentDigest(testIntent())
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	sent, markerErr := reference.sendRecorded(digest)
	if markerErr != nil || sent {
		t.Fatalf("unsent request acquired marker: %t %v", sent, markerErr)
	}
	if len(exchange.operations) != 1 {
		t.Fatalf("protocol failure was retried: %v", exchange.operations)
	}
}

func TestAdmissionConflictAfterSendNeedsExplicitAbsenceProof(t *testing.T) {
	for _, absenceProven := range []bool{false, true} {
		t.Run(fmt.Sprint(absenceProven), func(t *testing.T) {
			reference, _ := testReference(t)
			calls := 0
			coordinator := Coordinator{Exchange: exchangeFunc(func(_ context.Context, request Request) (Response, error) {
				calls++
				response := responseFor(request)
				response.Failure = &Failure{Code: "request_conflict", Message: "immutable request differs"}
				if absenceProven {
					response.TaskRecord = "not_created"
				}
				return response, nil
			})}
			_, err := coordinator.Dispatch(t.Context(), reference)
			assertAdmissionFailureClassification(t, err, absenceProven)
			if calls != 1 {
				t.Fatalf("nontransient conflict retried: %d", calls)
			}
		})
	}
}

func assertAdmissionFailureClassification(t *testing.T, err error, absenceProven bool) {
	t.Helper()
	if errors.Is(err, ErrAcceptanceUnknown) == absenceProven {
		t.Fatalf("absence proof classification changed: absence=%t err=%v", absenceProven, err)
	}
	if absenceProven {
		if !errors.Is(err, ErrAdmissionRejected) {
			t.Fatalf("definite rejection lost: %v", err)
		}
		return
	}
	var operationErr *OperationError
	if !errors.As(err, &operationErr) || operationErr.Code != "request_conflict" {
		t.Fatalf("underlying conflict cause lost: %v", err)
	}
}

func TestUnreadableSendMarkerRetainsUnknownWithoutExchange(t *testing.T) {
	reference, _ := testReference(t)
	corrupt := struct {
		IntentSHA256 int `json:"intent_sha256"`
	}{7}
	if _, err := reference.records.PutContext(t.Context(), "send-intent.json", corrupt); err != nil {
		t.Fatal(err)
	}
	exchange := &malformedAdmissionExchange{}
	_, err := (Coordinator{Exchange: exchange}).Dispatch(t.Context(), reference)
	var invalidType *json.UnmarshalTypeError
	if !errors.As(err, &invalidType) || !errors.Is(err, ErrAcceptanceUnknown) {
		t.Fatalf("unreadable marker lost concrete fault or uncertainty: %v", err)
	}
	if len(exchange.operations) != 0 {
		t.Fatalf("unreadable marker allowed exchange: %v", exchange.operations)
	}
	var retained struct {
		IntentSHA256 int `json:"intent_sha256"`
	}
	if readErr := reference.records.Read("send-intent.json", &retained); readErr != nil || retained != corrupt {
		t.Fatalf("corrupt marker modified: %+v %v", retained, readErr)
	}
}

func TestAcceptedResponseSurvivesConflictingAcceptanceReceipt(t *testing.T) {
	for _, operation := range []Operation{Admit, Reconcile} {
		t.Run(string(operation), func(t *testing.T) {
			reference, _ := testReference(t)
			prior := seedConflictingAcceptance(t, reference)
			calls := 0
			var expected Response
			coordinator := Coordinator{Exchange: exchangeFunc(func(_ context.Context, request Request) (Response, error) {
				calls++
				expected = responseFor(request)
				expected.TaskRecord = "created"
				return expected, nil
			})}
			actual, err := runCacheFaultOperation(t.Context(), coordinator, reference, operation)
			assertAcceptedResponseDespiteReceiptFault(t, actual, expected, err)
			if calls != 1 {
				t.Fatalf("receipt fault retried admission: %d", calls)
			}
			var retained Acceptance
			if readErr := reference.records.Read("accepted.json", &retained); readErr != nil || retained != prior {
				t.Fatalf("conflicting receipt modified: %+v %v", retained, readErr)
			}
		})
	}
}

func seedConflictingAcceptance(t *testing.T, reference *Reference) Acceptance {
	t.Helper()
	prior := Acceptance{IntentSHA256: strings.Repeat("f", 64), Identity: testIntent().Destination, TaskID: testIntent().TaskID}
	if _, err := reference.records.PutContext(t.Context(), "accepted.json", prior); err != nil {
		t.Fatal(err)
	}
	digest, err := IntentDigest(testIntent())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reference.beginSend(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	return prior
}

func assertAcceptedResponseDespiteReceiptFault(t *testing.T, actual, expected Response, err error) {
	t.Helper()
	if !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, ErrAcceptanceUnknown) {
		t.Fatalf("immutable receipt fault lost its cause or downgraded verified acceptance: %v", err)
	}
	if !reflect.DeepEqual(actual, expected) || actual.TaskRecord != "created" || !actual.Existing {
		t.Fatalf("receipt conflict erased validated destination response: got%+v want%+v", actual, expected)
	}
}
