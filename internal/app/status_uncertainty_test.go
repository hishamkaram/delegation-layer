package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestStatusObservationResultRetainsResponseAndError(t *testing.T) {
	pending := &pueue.Pending{}
	inFlight := &pueue.InFlightError{Pending: pending}
	storageFailure := errors.New("fixture task storage read failed")
	response := newResponse("status")
	response.RootID = strings.Repeat("a", 32)
	response.TaskID = strings.Repeat("b", 32)
	response.Raw = []taskdir.LogDescriptor{{Path: "/fixture/task/raw/stdout"}}
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{name: "successful observation"},
		{name: "unknown observation", err: pueue.ErrUnknown},
		{name: "pending observation", err: inFlight},
		{name: "pending repair", err: fmt.Errorf("repair queued runner: %w", inFlight)},
		{name: "unknown and pending", err: errors.Join(pueue.ErrUnknown, inFlight)},
		{name: "genuine binding failure", err: pueue.ErrBinding, code: 1},
		{name: "binding then pending", err: errors.Join(pueue.ErrBinding, inFlight), code: 1},
		{name: "pending then binding", err: errors.Join(inFlight, pueue.ErrBinding), code: 1},
		{name: "storage failure", err: storageFailure, code: 1},
		{name: "storage then pending", err: errors.Join(storageFailure, inFlight), code: 1},
		{name: "pending then storage", err: errors.Join(inFlight, storageFailure), code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := statusObservationResult(response, tc.err)
			if result.code != tc.code || !errors.Is(result.err, tc.err) {
				t.Fatalf("status changed observation classification or cause: code=%d err=%v want code=%d err=%v", result.code, result.err, tc.code, tc.err)
			}
			want := response
			if tc.code != 0 {
				want.Error = tc.err.Error()
			}
			if !reflect.DeepEqual(result.response, want) {
				t.Fatalf("status discarded or invented response evidence: got=%+v want=%+v", result.response, want)
			}
			if errors.Is(tc.err, pueue.ErrInFlight) {
				var retained *pueue.InFlightError
				if !errors.As(result.err, &retained) || retained == nil || retained.Pending != pending || !errors.Is(result.err, inFlight) {
					t.Fatalf("status lost the original pending command: %v", result.err)
				}
			}
			assertStatusUncertaintyJSON(t, result, response, tc.err)
		})
	}
}

func assertStatusUncertaintyJSON(t *testing.T, result commandResult, original Response, observationErr error) {
	t.Helper()
	var output bytes.Buffer
	if writeErr := writeJSONResponse(&output, "status", &result.response, result.err); writeErr != nil {
		t.Fatal(writeErr)
	}
	var decoded Response
	if decodeErr := json.Unmarshal(output.Bytes(), &decoded); decodeErr != nil {
		t.Fatalf("status observation did not produce JSON: %v; output=%s", decodeErr, output.Bytes())
	}
	want := original
	if observationErr != nil {
		want.Error = observationErr.Error()
	}
	// Exit-zero uncertainty must keep its diagnostic while leaving admission,
	// liveness, publication, task identity and existing descriptors unchanged.
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("status JSON lost its diagnostic or changed authority: got=%+v want=%+v", decoded, want)
	}
}
