package remote

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remotefiles"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func testIntent() Intent {
	return Intent{Version: 1, OriginRootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), Device: "worker", Executable: "delegate", Destination: Identity{Host: "worker.example", Port: 22, User: "runner", HostKey: "SHA256:known", Root: "/var/state/delegate", RootID: strings.Repeat("c", 32)}, Provider: "claude:print", Config: task.TaskConfig{Permission: "read-only", Budget: "1m"}, RemoteCwd: "/work", Brief: Descriptor{Name: "brief", Length: 5, SHA256: DigestBytes([]byte("hello"))}}
}

func testReference(t *testing.T) (*Reference, *taskdir.Store) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ref, err := OpenReference(context.Background(), store, testIntent().TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := ref.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = ref.SaveIntent(context.Background(), testIntent(), []byte("hello")); err != nil {
		t.Fatal(err)
	}
	return ref, store
}

type exchangeFunc func(context.Context, Request) (Response, error)

func (f exchangeFunc) Call(ctx context.Context, r Request) (Response, error) {
	response, err := f(ctx, r)
	if beforeSend(err) {
		return response, err
	}
	if r.BeforeWrite != nil {
		if gateErr := r.BeforeWrite(); gateErr != nil {
			return Response{}, gateErr
		}
	}
	return response, err
}

func responseFor(r Request) Response {
	digest := ""
	if r.Intent != nil {
		var err error
		digest, err = IntentDigest(*r.Intent)
		if err != nil {
			panic(err)
		}
	}
	return Response{Version: 1, Identity: testIntent().Destination, Envelope: []byte(`{"admission":"admitted","liveness":"running","publication":"unknown"}`), Existing: true, RequestSHA256: digest}
}

func TestLostAdmissionAcknowledgmentReconcilesWithoutRedispatch(t *testing.T) {
	ref, _ := testReference(t)
	var operations []Operation
	var delays []time.Duration
	launches := 0
	coordinator := Coordinator{Retry: RetryPolicy{Sleep: func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }}, Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) {
		operations = append(operations, r.Operation)
		if string(r.Data) != "hello" {
			t.Fatal("brief bytes were not retained")
		}
		if r.Operation == Admit {
			launches++
			return Response{}, io.ErrUnexpectedEOF
		}
		return responseFor(r), nil
	})}
	if _, err := coordinator.Dispatch(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if launches != 1 || !reflect.DeepEqual(operations, []Operation{Admit, Reconcile}) || !reflect.DeepEqual(delays, []time.Duration{time.Second}) {
		t.Fatalf("launches=%d operations=%v delays=%v", launches, operations, delays)
	}
	if _, err := ref.Accepted(); err != nil {
		t.Fatal(err)
	}
	// A new coordinator invocation reads the durable marker, not process memory.
	operations = nil
	if _, err := coordinator.Dispatch(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(operations, []Operation{Reconcile}) {
		t.Fatalf("restart replayed admission: %v", operations)
	}
}

func TestFiveAttemptsAndBackoff(t *testing.T) {
	ref, _ := testReference(t)
	count := 0
	var delays []time.Duration
	coordinator := Coordinator{Retry: RetryPolicy{Sleep: func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }}, Exchange: exchangeFunc(func(context.Context, Request) (Response, error) { count++; return Response{}, io.EOF })}
	if _, err := coordinator.Dispatch(context.Background(), ref); !errors.Is(err, io.EOF) {
		t.Fatalf("error=%v", err)
	}
	if count != 5 || !reflect.DeepEqual(delays, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}) {
		t.Fatalf("count=%d delays=%v", count, delays)
	}
}

func TestMissingRemoteRecordAfterSendRemainsUnknown(t *testing.T) {
	ref, _ := testReference(t)
	calls := 0
	c := Coordinator{Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) {
		calls++
		result := responseFor(r)
		result.Existing = false
		return result, nil
	})}
	if _, err := c.Dispatch(context.Background(), ref); !errors.Is(err, ErrAcceptanceUnknown) {
		t.Fatalf("error=%v", err)
	}
	if _, err := ref.Accepted(); err == nil {
		t.Fatal("missing remote request became accepted")
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestNonTransportFailureNeverRetries(t *testing.T) {
	ref, _ := testReference(t)
	calls := 0
	c := Coordinator{Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) { calls++; return Response{}, ErrIdentity })}
	if _, err := c.Observe(context.Background(), ref, Status, 0, 0); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestControlIDPayloadConflictPersistsAcrossHandles(t *testing.T) {
	ref, store := testReference(t)
	id := strings.Repeat("d", 32)
	first := Control{ID: id, Message: &task.InteractionMessage{ID: id, Kind: "steer", Text: "first"}}
	if err := ref.SaveControl(context.Background(), Steer, first); err != nil {
		t.Fatal(err)
	}
	second, err := OpenReference(context.Background(), store, testIntent().TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := second.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	first.Message.Text = "different"
	if err = second.SaveControl(context.Background(), Steer, first); !errors.Is(err, ErrConflict) {
		t.Fatalf("error=%v", err)
	}
}

func TestBriefExceedingControlLimitRetainsExactBytes(t *testing.T) {
	ref, _ := testReference(t)
	// A distinct reference stores a brief that cannot fit one control record.
	intent := testIntent()
	intent.TaskID = strings.Repeat("e", 32)
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	large, err := OpenReference(context.Background(), store, intent.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := large.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	data := bytes.Repeat([]byte("q"), MaxControlBytes+5)
	intent.Brief = Descriptor{Name: "brief", Length: int64(len(data)), SHA256: DigestBytes(data)}
	if err = large.SaveIntent(context.Background(), intent, data); err != nil {
		t.Fatal(err)
	}
	actual, err := large.Brief()
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("brief differs: %v", err)
	}
	changed := testIntent()
	changed.Provider = "codex:exec"
	if err = ref.SaveIntent(context.Background(), changed, []byte("hello")); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting immutable intent accepted: %v", err)
	}
}

func TestFramingRejectsOversizeBeforePayloadRead(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxControlBytes+1)
	f := Framer{Reader: bytes.NewReader(header[:])}
	var request Request
	if err := f.ReadControl(&request); !errors.Is(err, ErrProtocol) {
		t.Fatalf("error=%v", err)
	}
}

func TestFramingBriefAndStrictRequest(t *testing.T) {
	intent := testIntent()
	var stream bytes.Buffer
	f := Framer{Reader: &stream, Writer: &stream}
	request := Request{Version: 1, Operation: Admit, Intent: &intent, Data: []byte("hello")}
	if err := f.WriteRequest(request); err != nil {
		t.Fatal(err)
	}
	got, err := f.ReadRequest()
	if err != nil || !bytes.Equal(got.Data, request.Data) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if err = f.WriteControl(struct {
		Unknown bool `json:"unknown"`
	}{true}); err != nil {
		t.Fatal(err)
	}
	if err = f.ReadControl(&got); !errors.Is(err, ErrProtocol) {
		t.Fatalf("unknown field accepted: %v", err)
	}
}

func TestRetryDeadlineStopsBeforeAnotherAttempt(t *testing.T) {
	ref, _ := testReference(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	c := Coordinator{Retry: RetryPolicy{Sleep: func(context.Context, time.Duration) error { cancel(); return ctx.Err() }}, Exchange: exchangeFunc(func(context.Context, Request) (Response, error) { calls++; return Response{}, io.EOF })}
	if _, err := c.Dispatch(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestBeforeSendFailureRetriesOriginalAdmission(t *testing.T) {
	ref, _ := testReference(t)
	var operations []Operation
	c := Coordinator{Retry: RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }}, Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) {
		operations = append(operations, r.Operation)
		if len(operations) == 1 {
			return Response{}, &BeforeSendError{Err: io.EOF}
		}
		return responseFor(r), nil
	})}
	if _, err := c.Dispatch(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(operations, []Operation{Admit, Admit}) {
		t.Fatalf("operations=%v", operations)
	}
}

func TestManifestLargerThanControlFrameUsesBoundedDataFrames(t *testing.T) {
	manifest := remotefiles.Manifest{}
	for index := 0; index < 8000; index++ {
		manifest.Entries = append(manifest.Entries, remotefiles.Entry{Path: fmt.Sprintf("%s/file-%05d", strings.Repeat("a", 80), index), SHA256: strings.Repeat("b", 64)})
	}
	var stream bytes.Buffer
	f := Framer{Reader: &stream, Writer: &stream}
	request := Request{Version: 1, Operation: StageBegin, Manifest: &manifest}
	if err := f.WriteRequest(request); err != nil {
		t.Fatal(err)
	}
	if stream.Len() <= MaxControlBytes {
		t.Fatal("fixture did not exceed control frame")
	}
	got, err := f.ReadRequest()
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifest == nil || len(got.Manifest.Entries) != 8000 {
		t.Fatal("manifest entries lost")
	}
}

func TestWireRootIdentityGainsEndpointOnlyAtOrigin(t *testing.T) {
	wire := Response{Version: 1, Identity: Identity{Root: "/remote/state", RootID: strings.Repeat("a", 32)}}
	var stream bytes.Buffer
	f := Framer{Reader: &stream, Writer: &stream}
	if err := f.WriteResponse(wire); err != nil {
		t.Fatal(err)
	}
	response, err := f.ReadResponse()
	if err != nil {
		t.Fatal(err)
	}
	if err = response.Validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("unauthenticated endpoint accepted: %v", err)
	}
	response.Identity.Host = "remote"
	response.Identity.Port = 22
	response.Identity.User = "runner"
	response.Identity.HostKey = "SHA256:key"
	if err = response.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedResponseCleanupFailurePreservesAcceptance(t *testing.T) {
	reference, _ := testReference(t)
	calls := 0
	coordinator := Coordinator{Exchange: exchangeFunc(func(_ context.Context, request Request) (Response, error) {
		calls++
		return responseFor(request), &VerifiedResponseError{Err: io.EOF}
	})}
	response, err := coordinator.Dispatch(t.Context(), reference)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !response.Existing || len(response.Diagnostics) != 1 {
		t.Fatalf("verified response discarded: calls%d response%+v", calls, response)
	}
	accepted, err := reference.Accepted()
	if err != nil || accepted.TaskID != testIntent().TaskID {
		t.Fatalf("acceptance not retained: %+v %v", accepted, err)
	}
}

func TestLastObservationFollowsJournalOrderAfterClockRollback(t *testing.T) {
	reference, _ := testReference(t)
	first := responseFor(Request{})
	if _, err := reference.records.PutContext(t.Context(), "observation-00000000000000000001.json", Observation{Sequence: 1, ObservedAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano), Response: first}); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Envelope = []byte(`{"publication":"committed"}`)
	if err := reference.Observe(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	latest, err := reference.LastObservation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if latest.Sequence != 2 || string(latest.Response.Envelope) != string(second.Envelope) {
		t.Fatalf("old wall clock won journal order: %+v", latest)
	}
}

func TestKnownUnsentFailuresDoNotStrandReference(t *testing.T) {
	reference, _ := testReference(t)
	failed := Coordinator{Retry: RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }}, Exchange: exchangeFunc(func(context.Context, Request) (Response, error) { return Response{}, &BeforeSendError{Err: io.EOF} })}
	if _, err := failed.Dispatch(t.Context(), reference); !errors.Is(err, io.EOF) {
		t.Fatalf("unsent failure: %v", err)
	}
	digest, err := IntentDigest(testIntent())
	if err != nil {
		t.Fatal(err)
	}
	sent, err := reference.sendRecorded(digest)
	if err != nil || sent {
		t.Fatalf("unsent request became uncertain: %t %v", sent, err)
	}
	succeeding := Coordinator{Exchange: exchangeFunc(func(_ context.Context, request Request) (Response, error) {
		if request.Operation != Admit {
			t.Fatalf("known unsent request reconciled instead of admitted: %s", request.Operation)
		}
		return responseFor(request), nil
	})}
	if _, err = succeeding.Dispatch(t.Context(), reference); err != nil {
		t.Fatal(err)
	}
}

func TestDefiniteRejectionIsRetainedWithoutReconciliation(t *testing.T) {
	reference, _ := testReference(t)
	calls := 0
	coordinator := Coordinator{Exchange: exchangeFunc(func(_ context.Context, request Request) (Response, error) {
		calls++
		response := responseFor(request)
		response.Existing = false
		response.TaskRecord = "not_created"
		response.Failure = &Failure{Code: "invalid_request", Message: "request rejected before task creation"}
		return response, nil
	})}
	for range 2 {
		response, err := coordinator.Dispatch(t.Context(), reference)
		if !errors.Is(err, ErrAdmissionRejected) || response.TaskRecord != "not_created" {
			t.Fatalf("definite rejection became uncertain: %+v %v", response, err)
		}
	}
	if calls != 1 {
		t.Fatalf("rejected request was sent again: %d", calls)
	}
}
