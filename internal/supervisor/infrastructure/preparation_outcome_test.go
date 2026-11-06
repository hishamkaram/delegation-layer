package infrastructure

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func preparationConditionFixtures() []PreparationCondition {
	return []PreparationCondition{
		PreparationOK, PreparationPending, PreparationBusy,
		PreparationIncompatible, PreparationRefused, PreparationFault, PreparationTimeout,
	}
}

func preparationOwnerFixture() StartupReference {
	return StartupReference{Sequence: 1, Epoch: originTestRootID, IntentSHA256: strings.Repeat("a", 64)}
}

func TestPreparationOutcomeConditionsAndZero(t *testing.T) {
	for _, condition := range preparationConditionFixtures() {
		if err := (PreparationOutcome{Condition: condition}).Validate(); err != nil {
			t.Fatalf("known condition refused: %s: %v", condition, err)
		}
	}
	for _, outcome := range []PreparationOutcome{
		{Condition: "unknown"},
		{Startup: preparationOwnerFixture()},
		{Condition: PreparationOK, Startup: StartupReference{Sequence: 1}},
		{Condition: PreparationOK, Disposition: Disposition{Kind: "unknown"}},
		{Condition: PreparationFault, Failure: PreparationFailure{Causes: 1 << 31}},
	} {
		if err := outcome.Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("invalid outcome accepted: %+v: %v", outcome, err)
		}
	}
	zero := PreparationOutcome{}
	if err := zero.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(zero)
	if err != nil || !bytes.Equal(data, []byte("{}\n")) {
		t.Fatalf("zero outcome is not canonical absence: %s: %v", data, err)
	}
	var decoded PreparationOutcome
	if err = task.DecodeStrict(data, &decoded); err != nil || decoded != zero {
		t.Fatalf("zero outcome round trip: %v", err)
	}
}

func TestPreparationOutcomeRetainsCompletedWorkWithLateFault(t *testing.T) {
	for _, condition := range []PreparationCondition{PreparationFault, PreparationTimeout} {
		for _, report := range manualMigrationReportFixtures()[:5] {
			outcome := PreparationOutcome{
				Condition: condition, Startup: preparationOwnerFixture(),
				Disposition: Disposition{Kind: DispositionStarted, Context: LaunchContextReference{SHA256: strings.Repeat("b", 64)}},
				Report:      report,
				Failure:     PreparationFailure{Causes: PreparationCauseDeadline, IndependentFault: true},
			}
			before := outcome
			if err := outcome.Validate(); err != nil || outcome != before {
				t.Fatalf("completed observation lost on late failure: %v", err)
			}
			data, err := task.MarshalCanonical(outcome)
			if err != nil {
				t.Fatal(err)
			}
			var decoded PreparationOutcome
			if err = task.DecodeStrict(data, &decoded); err != nil || decoded != before {
				t.Fatalf("late failure outcome round trip lost observation: %v", err)
			}
		}
	}
}

func TestPreparationOutcomeStrictJSON(t *testing.T) {
	for _, data := range []string{
		`null`, `{"condition":null}`, `{"condition":"fault","failure":null}`,
		`{"condition":"ok","migration_result":"completed"}`,
		`{"condition":"fault","failure":{"causes":1}}`,
		`{"condition":"ok","report":{"state":"migrated"}}`,
		`{"condition":"ok","extra":true}`,
	} {
		var outcome PreparationOutcome
		if err := task.DecodeStrict([]byte(data), &outcome); err == nil {
			t.Fatalf("invalid outcome representation decoded: %s", data)
		}
	}
	for _, data := range []string{
		`{"condition":"unknown"}`, `{"condition":"fault","failure":{"causes":2147483648,"independent_fault":false}}`,
		`{"condition":"fault","failure":{"causes":1,"reason":"unknown","independent_fault":false}}`,
		`{"condition":"ok","report":{"state":"unknown","legacy":"unknown","active":"unknown"}}`,
	} {
		var outcome PreparationOutcome
		if err := task.DecodeStrict([]byte(data), &outcome); err != nil {
			t.Fatalf("invalid semantic fixture did not decode structurally: %v", err)
		}
		if err := outcome.Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("unknown enum or cause accepted: %s: %v", data, err)
		}
	}
}

func TestPreparationRequestValidatedBeforeNormalization(t *testing.T) {
	owner := preparationOwnerFixture()
	for _, operation := range initialOperationFixtures(t, originTestRootID) {
		request := PreparationRequest{Operation: operation, InitialResultFor: owner}
		before := request
		normalized, err := request.Normalized(originTestRootID, owner)
		if err != nil || request != before {
			t.Fatalf("valid request refused or mutated: %v", err)
		}
		want := operation
		if operation.Kind == InitialEnsureOwn {
			want = InitialOperation{}
		}
		if normalized.Operation != want || normalized.InitialResultFor != owner || normalized.Context != (LaunchContextReference{}) {
			t.Fatalf("normalization changed selection: %+v", normalized)
		}
	}
	source, _ := savedBindingSourceFixture(t, SavedBindingTaskMetadata)
	for _, request := range []PreparationRequest{
		{Operation: InitialOperation{Kind: InitialEnsureOwn, Source: source}},
		{Operation: InitialOperation{Kind: InitialRecoverSaved, Source: source}},
		{Context: LaunchContextReference{SHA256: "bad"}},
		{InitialResultFor: StartupReference{Sequence: 2, Epoch: owner.Epoch, IntentSHA256: owner.IntentSHA256}},
		{Operation: InitialOperation{Kind: "unknown"}},
	} {
		if _, err := request.Normalized(originTestRootID, owner); !errors.Is(err, ErrIdentity) {
			t.Fatalf("invalid request normalized: %+v: %v", request, err)
		}
	}
	if err := (PreparationRequest{}).Validate(originTestRootID, StartupReference{}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("missing expected owner accepted: %v", err)
	}
}

func TestEstablishedOwnerClosedSelector(t *testing.T) {
	owner := preparationOwnerFixture()
	for _, established := range []EstablishedOwner{{Owner: owner}, {Owner: owner, InitialResultFor: owner}} {
		if err := established.Validate(); err != nil {
			t.Fatalf("valid established owner refused: %v", err)
		}
	}
	for _, established := range []EstablishedOwner{{}, {InitialResultFor: owner}, {
		Owner: owner, InitialResultFor: StartupReference{Sequence: 2, Epoch: owner.Epoch, IntentSHA256: owner.IntentSHA256},
	}} {
		if err := established.Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("invalid established selector accepted: %+v: %v", established, err)
		}
	}
}

func TestPreparationRequestZeroJSONAndStrictSelectors(t *testing.T) {
	data, err := task.MarshalCanonical(PreparationRequest{})
	if err != nil || !bytes.Equal(data, []byte("{}\n")) {
		t.Fatalf("default Own request bytes: %s: %v", data, err)
	}
	var request PreparationRequest
	if err = task.DecodeStrict(data, &request); err != nil || request != (PreparationRequest{}) {
		t.Fatalf("absent request fields changed default: %v", err)
	}

	if err = request.Validate(originTestRootID, preparationOwnerFixture()); err != nil {
		t.Fatalf("default Own operation refused: %v", err)
	}
	for _, raw := range []string{
		`{"initial_result_for":null}`, `{"operation":null}`, `{"operation":{"kind":""}}`,
		`{"initial_result_for":{"sequence":1}}`, `{"context":null}`, `{"action":"ensure-own"}`,
	} {
		if err = task.DecodeStrict([]byte(raw), &request); err == nil {
			t.Fatalf("invalid request representation decoded: %s", raw)
		}
	}
}
