package remote

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
)

func TestOverrideSupportIsCheckedBeforeSendAndReusedAfterPreparation(t *testing.T) {
	ref, _ := testReference(t)
	var operations []Operation
	policy := resourceadmission.GuardPolicy{DisableCPU: true}
	c := Coordinator{ResourceGuards: policy, Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) {
		operations = append(operations, r.Operation)
		response := responseFor(r)
		if r.Operation == Hello {
			response.Capabilities = []string{ResourceGuardCapability}
		}
		if r.Operation == Admit && (r.ResourceGuards == nil || *r.ResourceGuards != policy) {
			t.Errorf("admission lost requested policy: %+v", r.ResourceGuards)
		}
		return response, nil
	})}
	prepared, err := c.PrepareDispatch(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	assertNotSent(t, ref)
	if _, err = prepared.Dispatch(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(operations, []Operation{Hello, Admit}) {
		t.Fatalf("capability check duplicated or skipped: %v", operations)
	}
	saved, readErr := ref.Intent()
	if readErr != nil || !reflect.DeepEqual(saved, testIntent()) {
		t.Fatalf("invocation policy changed durable intent/config: %+v %v", saved, readErr)
	}
}

func TestUnsupportedOverrideLeavesReferenceUnsentAndUnrejected(t *testing.T) {
	for _, prepare := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "prepared"}[prepare], func(t *testing.T) {
			ref, _ := testReference(t)
			calls := 0
			c := Coordinator{ResourceGuards: resourceadmission.GuardPolicy{DisableMemory: true}, Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) {
				calls++
				if r.Operation != Hello {
					t.Fatalf("unsupported peer received %s", r.Operation)
				}
				return responseFor(r), nil
			})}
			var err error
			if prepare {
				_, err = c.PrepareDispatch(t.Context(), ref)
			} else {
				_, err = c.Dispatch(t.Context(), ref)
			}
			if !errors.Is(err, ErrResourceGuardUnsupported) || errors.Is(err, ErrAcceptanceUnknown) {
				t.Fatalf("unsupported error=%v", err)
			}
			assertNotSent(t, ref)
			if calls != 1 {
				t.Fatalf("unexpected retries=%d", calls)
			}
		})
	}
}

func assertNotSent(t *testing.T, ref *Reference) {
	t.Helper()
	digest, err := IntentDigest(testIntent())
	if err != nil {
		t.Fatal(err)
	}
	sent, err := ref.sendRecorded(digest)
	if err != nil || sent {
		t.Fatalf("unexpected send marker sent=%t err=%v", sent, err)
	}
	if _, err = ref.rejectedResponse(testIntent(), digest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestLostOverrideAcknowledgmentReconcilesWithoutPolicyOrNewHello(t *testing.T) {
	ref, _ := testReference(t)
	var operations []Operation
	c := Coordinator{ResourceGuards: resourceadmission.GuardPolicy{DisableCPU: true}, Retry: RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }}, Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) {
		operations = append(operations, r.Operation)
		if r.Operation == Admit {
			return Response{}, io.ErrUnexpectedEOF
		}
		if r.Operation == Reconcile && (r.ResourceGuards != nil || r.BeforeWrite != nil) {
			t.Fatal("reconciliation retained admitting fields")
		}
		response := responseFor(r)
		response.Capabilities = []string{ResourceGuardCapability}
		return response, nil
	})}
	if _, err := c.Dispatch(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(operations, []Operation{Hello, Admit, Reconcile}) {
		t.Fatalf("operations=%v", operations)
	}
	operations = nil
	prepared, err := c.PrepareDispatch(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepared.Dispatch(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(operations, []Operation{Reconcile}) {
		t.Fatalf("recorded send renegotiated or replayed: %v", operations)
	}
}

func TestGuardOverrideIsAdmissionOnly(t *testing.T) {
	policy := resourceadmission.GuardPolicy{DisableCPU: true}
	for _, op := range []Operation{Hello, Prepare, Reconcile, Status, Collect, Cancel, Events, Reply, Steer, StageBegin, StageChunk, StageCommit, Download} {
		if err := (Request{Version: ProtocolVersion, Operation: op, ResourceGuards: &policy}).Validate(); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s accepted override: %v", op, err)
		}
	}
	if err := (Request{Version: ProtocolVersion, Operation: Admit, ResourceGuards: &policy}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedOverrideCannotBeReusedForAnotherPolicy(t *testing.T) {
	ref, _ := testReference(t)
	hellos := 0
	c := Coordinator{ResourceGuards: resourceadmission.GuardPolicy{DisableCPU: true}, Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) {
		if r.Operation == Hello {
			hellos++
		}
		response := responseFor(r)
		response.Capabilities = []string{ResourceGuardCapability}
		return response, nil
	})}
	prepared, err := c.PrepareDispatch(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	prepared.ResourceGuards = resourceadmission.GuardPolicy{DisableMemory: true}
	if _, err = prepared.Dispatch(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if hellos != 2 {
		t.Fatalf("policy reused another capability observation: %d", hellos)
	}
}

func TestChangingGuardsCannotRescueDurablyRejectedReference(t *testing.T) {
	ref, _ := testReference(t)
	digest, err := IntentDigest(testIntent())
	if err != nil {
		t.Fatal(err)
	}
	rejection := responseFor(Request{})
	rejection.TaskRecord = "not_created"
	if err = ref.recordRejection(t.Context(), digest, rejection); err != nil {
		t.Fatal(err)
	}
	c := Coordinator{ResourceGuards: resourceadmission.GuardPolicy{DisableCPU: true}, Exchange: exchangeFunc(func(context.Context, Request) (Response, error) {
		t.Fatal("durable rejection contacted peer")
		return Response{}, nil
	})}
	c, err = c.PrepareDispatch(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.Dispatch(t.Context(), ref)
	if !errors.Is(err, ErrAdmissionRejected) || response.TaskRecord != "not_created" {
		t.Fatalf("changed guard altered rejection: %+v %v", response, err)
	}
}
